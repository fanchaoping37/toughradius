//go:build linux

package pppoe

// AF_PACKET raw socket support for PPPoE Discovery on Linux.
// Requires CAP_NET_RAW / root. The socket is bound to ETH_P_PPP_DISC (0x8863)
// so only Discovery frames are received; Session frames (0x8864) are handled
// by the kernel pppoe driver in production deployments.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// RawSocket sends and receives PPPoE Discovery frames on one interface.
type RawSocket struct {
	iface     string
	fd        int
	localMAC  net.HardwareAddr
	sessionID atomic.Uint32
}

// OpenRawSocket binds an AF_PACKET socket to the named interface.
func OpenRawSocket(iface string) (*RawSocket, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("pppoe: interface %s: %w", iface, err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_PPP_DISC)))
	if err != nil {
		return nil, fmt.Errorf("pppoe: AF_PACKET socket (need CAP_NET_RAW): %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_PPP_DISC),
		Ifindex:  ifi.Index,
	}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("pppoe: bind: %w", err)
	}
	s := &RawSocket{iface: iface, fd: fd, localMAC: append([]byte(nil), ifi.HardwareAddr...)}
	s.sessionID.Store(1)
	return s, nil
}

func htons(v uint16) uint16 { return (v << 8) | (v >> 8) }

// Close releases the socket.
func (s *RawSocket) Close() error {
	if s == nil || s.fd < 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}

// Send writes a complete Ethernet frame.
func (s *RawSocket) Send(frame []byte) error {
	if s == nil || s.fd < 0 {
		return errors.New("pppoe: socket closed")
	}
	return unix.Send(s.fd, frame, 0)
}

// Serve reads Discovery frames until ctx is cancelled, invoking handle for
// each decoded PPPoE packet together with the source MAC. Replies produced by
// handle are sent back to the source MAC as Ethernet frames.
func (s *RawSocket) Serve(ctx context.Context, acName string, handle func(*Packet, net.HardwareAddr) ([]Tag, error)) error {
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	buf := make([]byte, 2048)
	for {
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return fmt.Errorf("pppoe: recv: %w", err)
		}
		etherType, payload, err := ParseEthernetFrame(buf[:n])
		if err != nil || etherType != EtherTypeDiscovery {
			continue
		}
		packet, err := Parse(payload)
		if err != nil {
			continue
		}
		srcMAC := net.HardwareAddr(append([]byte(nil), buf[6:12]...))
		tags, err := handle(packet, srcMAC)
		if err != nil {
			continue
		}
		replyCode, sessionID, replyTags, err := HandleDiscovery(packet, acName, uint16(s.sessionID.Add(1))) //nolint:gosec
		if err != nil {
			continue
		}
		if replyCode == 0 {
			continue
		}
		if len(tags) > 0 {
			replyTags = tags
		}
		pppoeFrame := Encode(replyCode, sessionID, replyTags)
		dst := packetHostUniqMAC(srcMAC)
		eth, err := EthernetFrame(dst, s.localMAC, EtherTypeDiscovery, pppoeFrame)
		if err != nil {
			continue
		}
		if err := s.Send(eth); err != nil {
			return err
		}
	}
}

func packetHostUniqMAC(src net.HardwareAddr) net.HardwareAddr {
	// PADI is broadcast; PADO/PADS are unicast to the source MAC.
	return src
}
