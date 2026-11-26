// Package dhcpv6 implements a minimal DHCPv6 server (RFC 3315, obsoleted by
// RFC 8415/9915) for IPv6 single-stack subscriber address binding and source
// tracing, plus RFC 7710 Captive Portal URI option 103.
//
// Authoritative references:
//   - RFC 3315 §5.3 message types: Solicit=1 Advertise=2 Request=3 Confirm=4
//     Renew=5 Rebind=6 Reply=7 Information-Request=11 Relay-Forward=12
//     Relay-Reply=13 (IANA DHCPv6 registry now cites RFC 9915; values unchanged)
//   - RFC 3315 §9 DUID: type 1=LLT, 2=EN, 3=LL, 4=UUID (RFC 6355)
//   - RFC 3315 §22.4 IA_NA option 3: IAID(4)|T1(4)|T2(4)
//   - RFC 3315 §22.6 IAADDR option 5: IPv6(16)|Preferred(4)|Valid(4)
//   - RFC 3646 DNS recursive name servers option 23
//   - RFC 7710 §2.2 Captive Portal URI option 103 (2-byte code/len + URI)
//   - UDP 546 (client) / 547 (server/relay), RFC 3315 §5.2
package dhcpv6

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const (
	ClientPort = 546
	ServerPort = 547

	MsgSolicit            = 1
	MsgAdvertise          = 2
	MsgRequest            = 3
	MsgConfirm            = 4
	MsgRenew              = 5
	MsgRebind             = 6
	MsgReply              = 7
	MsgRelease            = 8
	MsgDecline            = 9
	MsgInformationRequest = 11
	MsgRelayForward       = 12
	MsgRelayReply         = 13

	OptClientID   = 1
	OptServerID   = 2
	OptIA_NA      = 3
	OptIAAddr     = 5
	OptORO        = 6
	OptDNS        = 23
	OptCaptiveURI = 103 // RFC 7710 §2.2

	DUID_LLT  = 1
	DUID_EN   = 2
	DUID_LL   = 3
	DUID_UUID = 4

	HeaderLen = 4 // msg-type(1) + transaction-id(3)
)

var (
	ErrShortPacket = errors.New("dhcpv6: short packet")
	ErrBadOption   = errors.New("dhcpv6: malformed option")
)

// Option is a DHCPv6 TLV with 2-byte code and length.
type Option struct {
	Code  uint16
	Value []byte
}

// Packet is a decoded DHCPv6 message.
type Packet struct {
	MsgType byte
	TxID    [3]byte
	Options map[uint16][]byte
	Order   []uint16
}

// Parse decodes a DHCPv6 message header and options.
func Parse(data []byte) (*Packet, error) {
	if len(data) < HeaderLen {
		return nil, ErrShortPacket
	}
	p := &Packet{
		MsgType: data[0],
		Options: make(map[uint16][]byte),
	}
	copy(p.TxID[:], data[1:4])
	for off := HeaderLen; off+4 <= len(data); {
		code := binary.BigEndian.Uint16(data[off : off+2])
		length := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		off += 4
		if off+length > len(data) {
			return nil, ErrBadOption
		}
		value := append([]byte(nil), data[off:off+length]...)
		off += length
		if _, exists := p.Options[code]; !exists {
			p.Order = append(p.Order, code)
		}
		p.Options[code] = value
	}
	return p, nil
}

// Encode serialises a DHCPv6 message with options in insertion order.
func Encode(msgType byte, txID [3]byte, opts []Option) []byte {
	out := make([]byte, HeaderLen, HeaderLen+256)
	out[0] = msgType
	copy(out[1:4], txID[:])
	for _, o := range opts {
		var h [4]byte
		binary.BigEndian.PutUint16(h[0:2], o.Code)
		binary.BigEndian.PutUint16(h[2:4], uint16(len(o.Value))) //nolint:gosec
		out = append(out, h[:]...)
		out = append(out, o.Value...)
	}
	return out
}

// DUID represents a DHCPv6 Unique Identifier (RFC 3315 §9).
type DUID struct {
	Type byte // 1=LLT, 2=EN, 3=LL, 4=UUID
	Raw  []byte
}

// ParseDUID parses a DUID option value. DUIDs must be treated as opaque and
// compared byte-for-byte (RFC 3315 §9).
func ParseDUID(value []byte) (*DUID, error) {
	if len(value) < 2 {
		return nil, ErrBadOption
	}
	duidType := binary.BigEndian.Uint16(value[0:2])
	if duidType > 4 || len(value) > 130 {
		return nil, ErrBadOption
	}
	return &DUID{Type: byte(duidType), Raw: append([]byte(nil), value...)}, nil
}

// Key returns a stable string key for binding tables.
func (d *DUID) Key() string {
	return fmt.Sprintf("%d:%x", d.Type, d.Raw)
}

// IA_NA represents an Identity Association for Non-temporary Addresses.
type IA_NA struct {
	IAID      uint32
	T1        uint32
	T2        uint32
	Addr      net.IP
	Preferred uint32
	Valid     uint32
}

// ParseIA_NA decodes option 3 (and its nested IAADDR option 5).
func ParseIA_NA(value []byte) (*IA_NA, error) {
	if len(value) < 12 {
		return nil, ErrBadOption
	}
	ia := &IA_NA{
		IAID: binary.BigEndian.Uint32(value[0:4]),
		T1:   binary.BigEndian.Uint32(value[4:8]),
		T2:   binary.BigEndian.Uint32(value[8:12]),
	}
	for off := 12; off+4 <= len(value); {
		code := binary.BigEndian.Uint16(value[off : off+2])
		length := int(binary.BigEndian.Uint16(value[off+2 : off+4]))
		off += 4
		if off+length > len(value) {
			return nil, ErrBadOption
		}
		if code == OptIAAddr && length >= 24 {
			ia.Addr = net.IP(value[off : off+16])
			ia.Preferred = binary.BigEndian.Uint32(value[off+16 : off+20])
			ia.Valid = binary.BigEndian.Uint32(value[off+20 : off+24])
		}
		off += length
	}
	return ia, nil
}

// IA_NAOption builds option 3 with an embedded IAADDR (option 5).
func IA_NAOption(iaid uint32, addr net.IP, preferred, valid uint32, t1, t2 uint32) (Option, error) {
	if addr == nil || addr.To16() == nil {
		return Option{}, errors.New("dhcpv6: address must be IPv6")
	}
	addrBytes := addr.To16()
	iaaddr := make([]byte, 4+24)
	binary.BigEndian.PutUint16(iaaddr[0:2], OptIAAddr)
	binary.BigEndian.PutUint16(iaaddr[2:4], 24)
	copy(iaaddr[4:20], addrBytes)
	binary.BigEndian.PutUint32(iaaddr[20:24], preferred)
	binary.BigEndian.PutUint32(iaaddr[24:28], valid)

	value := make([]byte, 12+len(iaaddr))
	binary.BigEndian.PutUint32(value[0:4], iaid)
	binary.BigEndian.PutUint32(value[4:8], t1)
	binary.BigEndian.PutUint32(value[8:12], t2)
	copy(value[12:], iaaddr)
	return Option{Code: OptIA_NA, Value: value}, nil
}

// DNSOption builds RFC 3646 option 23 (concatenated 16-byte IPv6 addresses).
func DNSOption(addrs ...net.IP) Option {
	value := make([]byte, 0, 16*len(addrs))
	for _, a := range addrs {
		if v := a.To16(); v != nil {
			value = append(value, v...)
		}
	}
	return Option{Code: OptDNS, Value: value}
}

// CaptiveURIOption builds RFC 7710 §2.2 option 103.
func CaptiveURIOption(uri string) Option {
	return Option{Code: OptCaptiveURI, Value: []byte(uri)}
}

// ClientDUID returns the client DUID option value or nil.
func (p *Packet) ClientDUID() []byte { return p.Options[OptClientID] }
