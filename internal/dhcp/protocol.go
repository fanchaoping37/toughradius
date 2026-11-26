// Package dhcp implements a minimal DHCPv4 server and DNS redirector used to
// steer unauthenticated subscribers to the captive Portal.
//
// Protocol facts (all RFC-backed):
//   - BOOTP/DHCP fixed header is 236 octets, followed by magic cookie
//     0x63825363 and TLV options (RFC 2131 §2/§3, RFC 2132 §2).
//   - DHCP message types use option 53: Discover=1 Offer=2 Request=3
//     Decline=4 Ack=5 Nak=6 Release=7 Inform=8 (RFC 2132 §9.6).
//   - Relay Agent Information is option 82; sub-options 1=Circuit-ID,
//     2=Remote-ID and MUST be echoed verbatim in replies (RFC 3046).
//   - The standards-track Captive Portal URI option is 160 (RFC 7710 §2.1).
//     The widely deployed but non-standard option 243 is also emitted for
//     compatibility with legacy CPE; it is NOT an RFC option.
//   - DHCP messages are carried on UDP 67 (server) / 68 (client) (RFC 2131).
package dhcp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const (
	// ServerPort and ClientPort are the well-known DHCP UDP ports.
	ServerPort = 67
	ClientPort = 68

	// MagicCookie precedes the variable-length option area.
	MagicCookie uint32 = 0x63825363

	// HeaderLen is the fixed BOOTP header length (sname/file included).
	HeaderLen = 236

	// Op codes.
	OpBootRequest = 1
	OpBootReply   = 2

	// Option codes.
	OptSubnetMask      = 1
	OptRouter          = 3
	OptDNSServer       = 6
	OptHostName        = 12
	OptRequestedIP     = 50
	OptLeaseTime       = 51
	OptMsgType         = 53
	OptServerID        = 54
	OptRequestedParams = 55
	OptRelayAgent      = 82
	OptCaptivePortal   = 160 // RFC 7710
	OptPortalURL243    = 243 // vendor-private compatibility only
	OptEnd             = 255
	OptPad             = 0

	// DHCP message types (option 53).
	MsgDiscover = 1
	MsgOffer    = 2
	MsgRequest  = 3
	MsgDecline  = 4
	MsgAck      = 5
	MsgNak      = 6
	MsgRelease  = 7
	MsgInform   = 8

	// Option 82 sub-option codes (RFC 3046 §3).
	SubOptCircuitID = 1
	SubOptRemoteID  = 2
)

var (
	ErrShortPacket   = errors.New("dhcp: packet shorter than fixed header")
	ErrBadCookie     = errors.New("dhcp: invalid magic cookie")
	ErrNoMessageType = errors.New("dhcp: missing option 53 message type")
	ErrBadOption     = errors.New("dhcp: malformed option")
)

// Packet is a decoded DHCPv4 message. Raw options are kept for verbatim
// relay-agent (option 82) echo as mandated by RFC 3046 §2.2.
type Packet struct {
	Op         byte
	HType      byte
	HLen       byte
	Hops       byte
	XID        uint32
	Secs       uint16
	Flags      uint16
	CIAddr     net.IP
	YIAddr     net.IP
	SIAddr     net.IP
	GIAddr     net.IP
	CHAddr     [16]byte
	Options    map[byte][]byte
	MsgType    byte
	RelayAgent []byte // complete, unmodified option 82 value
}

// Parse decodes a DHCP message. Truncated or malformed option tails are
// tolerated (parsing stops) because real CPE occasionally pads packets;
// absence of option 53 is always an error for BOOTREQUEST messages.
func Parse(data []byte) (*Packet, error) {
	if len(data) < HeaderLen+4 {
		return nil, ErrShortPacket
	}
	if binary.BigEndian.Uint32(data[HeaderLen:HeaderLen+4]) != MagicCookie {
		return nil, ErrBadCookie
	}
	p := &Packet{
		Op:      data[0],
		HType:   data[1],
		HLen:    data[2],
		Hops:    data[3],
		XID:     binary.BigEndian.Uint32(data[4:8]),
		Secs:    binary.BigEndian.Uint16(data[8:10]),
		Flags:   binary.BigEndian.Uint16(data[10:12]),
		CIAddr:  net.IP(data[12:16]).To4(),
		YIAddr:  net.IP(data[16:20]).To4(),
		SIAddr:  net.IP(data[20:24]).To4(),
		GIAddr:  net.IP(data[24:28]).To4(),
		Options: make(map[byte][]byte),
	}
	copy(p.CHAddr[:], data[28:44])

	off := HeaderLen + 4
	for off < len(data) {
		code := data[off]
		off++
		if code == OptEnd {
			break
		}
		if code == OptPad {
			continue
		}
		if off >= len(data) {
			break
		}
		length := int(data[off])
		off++
		if off+length > len(data) {
			return nil, ErrBadOption
		}
		value := append([]byte(nil), data[off:off+length]...)
		off += length
		p.Options[code] = value
		if code == OptMsgType && length >= 1 {
			p.MsgType = value[0]
		}
		if code == OptRelayAgent {
			p.RelayAgent = value
		}
	}
	if p.Op == OpBootRequest && p.MsgType == 0 {
		return nil, ErrNoMessageType
	}
	return p, nil
}

// MAC returns the client hardware address as a colon-separated string.
func (p *Packet) MAC() string {
	n := int(p.HLen)
	if n <= 0 || n > len(p.CHAddr) {
		n = 6
	}
	hw := make(net.HardwareAddr, n)
	copy(hw, p.CHAddr[:n])
	return hw.String()
}

// BroadcastFlag reports the RFC 2131 §4.1 B flag (top bit of flags).
func (p *Packet) BroadcastFlag() bool { return p.Flags&0x8000 != 0 }

// RequestedIP returns option 50 when present.
func (p *Packet) RequestedIP() net.IP {
	if v, ok := p.Options[OptRequestedIP]; ok && len(v) == 4 {
		return net.IPv4(v[0], v[1], v[2], v[3])
	}
	return nil
}

// ServerIdentifier returns option 54 when present.
func (p *Packet) ServerIdentifier() net.IP {
	if v, ok := p.Options[OptServerID]; ok && len(v) == 4 {
		return net.IPv4(v[0], v[1], v[2], v[3])
	}
	return nil
}

// Reply builds a BOOTREPLY packet. Relay agent option 82 is re-appended
// verbatim and last (before End), per RFC 3046 §2.2.
func (p *Packet) Reply(msgType byte, yiaddr, serverID net.IP, opts []Option) ([]byte, error) {
	buf := make([]byte, HeaderLen+4, HeaderLen+4+256)
	buf[0] = OpBootReply
	buf[1] = p.HType
	buf[2] = p.HLen
	// hops/secs are copied from the request for relay correctness (RFC 2131 §4.1).
	buf[3] = p.Hops
	binary.BigEndian.PutUint32(buf[4:8], p.XID)
	binary.BigEndian.PutUint16(buf[8:10], p.Secs)
	if p.BroadcastFlag() {
		binary.BigEndian.PutUint16(buf[10:12], 0x8000)
	}
	if p.CIAddr != nil {
		copy(buf[12:16], p.CIAddr.To4())
	}
	if yiaddr != nil {
		copy(buf[16:20], yiaddr.To4())
	}
	if serverID != nil {
		copy(buf[20:24], serverID.To4())
	}
	if p.GIAddr != nil {
		copy(buf[24:28], p.GIAddr.To4())
	}
	copy(buf[28:44], p.CHAddr[:])
	binary.BigEndian.PutUint32(buf[HeaderLen:HeaderLen+4], MagicCookie)

	buf = appendOption(buf, OptMsgType, []byte{msgType})
	for _, o := range opts {
		buf = appendOption(buf, o.Code, o.Value)
	}
	if len(p.RelayAgent) > 0 {
		buf = appendOption(buf, OptRelayAgent, p.RelayAgent)
	}
	buf = append(buf, OptEnd)
	return buf, nil
}

// Option is a DHCP TLV option.
type Option struct {
	Code  byte
	Value []byte
}

func appendOption(dst []byte, code byte, value []byte) []byte {
	if len(value) > 255 {
		value = value[:255]
	}
	dst = append(dst, code, byte(len(value)))
	return append(dst, value...)
}

// IPOption builds an option carrying one or more IPv4 addresses.
func IPOption(code byte, addrs ...net.IP) Option {
	v := make([]byte, 0, 4*len(addrs))
	for _, a := range addrs {
		if ip := a.To4(); ip != nil {
			v = append(v, ip...)
		}
	}
	return Option{Code: code, Value: v}
}

// Uint32Option builds a 4-byte big-endian numeric option.
func Uint32Option(code byte, value uint32) Option {
	v := make([]byte, 4)
	binary.BigEndian.PutUint32(v, value)
	return Option{Code: code, Value: v}
}

// StringOption builds a byte-string option (truncated to 255 octets).
func StringOption(code byte, value string) Option {
	if len(value) > 255 {
		value = value[:255]
	}
	return Option{Code: code, Value: []byte(value)}
}

// ParseRelayAgent extracts option 82 sub-options as a map. Unknown sub-options
// are ignored; values are opaque and must be compared byte-for-byte (RFC 3046 §3.1).
func ParseRelayAgent(value []byte) map[byte][]byte {
	result := make(map[byte][]byte)
	for i := 0; i+2 <= len(value); {
		code := value[i]
		length := int(value[i+1])
		i += 2
		if i+length > len(value) {
			break
		}
		result[code] = append([]byte(nil), value[i:i+length]...)
		i += length
	}
	return result
}

// CircuitID returns the RFC 3046 sub-option 1 value, or empty when absent.
func (p *Packet) CircuitID() string {
	return string(ParseRelayAgent(p.RelayAgent)[SubOptCircuitID])
}

// RemoteID returns the RFC 3046 sub-option 2 value, or empty when absent.
func (p *Packet) RemoteID() string {
	return string(ParseRelayAgent(p.RelayAgent)[SubOptRemoteID])
}

// FormatXID renders a transaction ID for logs.
func (p *Packet) FormatXID() string { return fmt.Sprintf("0x%08x", p.XID) }
