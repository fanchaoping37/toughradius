// Package pppoe implements the RFE 2516 PPPoE Discovery and PPP framing
// primitives needed to interwork with a RADIUS-backed broadband gateway.
//
// Authoritative references:
//   - RFC 2516: A Method for transmitting PPP over Ethernet (PPPoE)
//     §4 EtherType 0x8863 (Discovery) / 0x8864 (Session)
//     §5 Discovery codes PADI=0x09 PADO=0x07 PADR=0x19 PADS=0x65 PADT=0xa7
//     §5 Tag = Tag-Type(2) | Tag-Length(2) | Value; Appendix A tag types
//     §6/§7 PPPoE header = Ver/Type(0x11) | Code | Session-ID | Length
//   - RFC 1661: PPP LCP codes 1..10, protocol 0xC021
//   - RFC 1334: PAP Authenticate-Request=1/Ack=2/Nak=3, protocol 0xC023
//   - IANA PPP Numbers: IPCP=0x8021, IPv6CP=0x8057, CHAP=0xC223
//   - RFC 2865 §2.2: PAP Peer-ID -> User-Name, Password -> User-Password
//
// This package is wire-format only. Actually transmitting frames requires an
// AF_PACKET raw socket on Linux (build tag linux, see socket_linux.go) plus
// CAP_NET_RAW. On other platforms the protocol layer remains fully testable.
package pppoe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const (
	EtherTypeDiscovery = 0x8863
	EtherTypeSession   = 0x8864

	CodePADI    = 0x09
	CodePADO    = 0x07
	CodePADR    = 0x19
	CodePADS    = 0x65
	CodePADT    = 0xa7
	CodeSession = 0x00

	TagServiceName    = 0x0101
	TagACName         = 0x0102
	TagHostUniq       = 0x0103
	TagACCookie       = 0x0104
	TagVendorSpecific = 0x0105
	TagRelaySessionID = 0x0110

	ProtoLCP    = 0xC021
	ProtoPAP    = 0xC023
	ProtoCHAP   = 0xC223
	ProtoIPCP   = 0x8021
	ProtoIPv6CP = 0x8057

	LCPConfigureRequest = 1
	LCPConfigureAck     = 2
	LCPConfigureNak     = 3
	LCPConfigureReject  = 4
	LCPTerminateRequest = 5
	LCPTerminateAck     = 6
	LCPEchoRequest      = 9
	LCPEchoReply        = 10

	PAPAuthenticateRequest = 1
	PAPAuthenticateAck     = 2
	PAPAuthenticateNak     = 3

	HeaderLen = 6
)

var (
	ErrShortFrame = errors.New("pppoe: frame shorter than header")
	ErrBadVersion = errors.New("pppoe: invalid version/type")
	ErrBadTag     = errors.New("pppoe: malformed tag")
	ErrBadPAP     = errors.New("pppoe: malformed PAP packet")
)

// Packet is a decoded PPPoE Discovery/Session frame.
type Packet struct {
	Code      byte
	SessionID uint16
	Tags      map[uint16][]byte
	TagOrder  []uint16
	Protocol  uint16 // session phase only
	Payload   []byte // PPP payload (session phase) or raw tag area
}

// Parse decodes a PPPoE header and all tags (Discovery) or the PPP protocol
// field (Session). The 14-byte Ethernet header is NOT part of data.
func Parse(data []byte) (*Packet, error) {
	if len(data) < HeaderLen {
		return nil, ErrShortFrame
	}
	if data[0] != 0x11 {
		return nil, ErrBadVersion
	}
	p := &Packet{
		Code:      data[1],
		SessionID: binary.BigEndian.Uint16(data[2:4]),
		Tags:      make(map[uint16][]byte),
	}
	length := int(binary.BigEndian.Uint16(data[4:6]))
	if length > len(data)-HeaderLen {
		length = len(data) - HeaderLen
	}
	body := data[HeaderLen : HeaderLen+length]

	if p.Code == CodeSession {
		if len(body) < 2 {
			return nil, ErrShortFrame
		}
		p.Protocol = binary.BigEndian.Uint16(body[0:2])
		p.Payload = append([]byte(nil), body[2:]...)
		return p, nil
	}

	off := 0
	for off+4 <= len(body) {
		tagType := binary.BigEndian.Uint16(body[off : off+2])
		tagLen := int(binary.BigEndian.Uint16(body[off+2 : off+4]))
		off += 4
		if off+tagLen > len(body) {
			return nil, ErrBadTag
		}
		value := append([]byte(nil), body[off:off+tagLen]...)
		off += tagLen
		if _, exists := p.Tags[tagType]; !exists {
			p.TagOrder = append(p.TagOrder, tagType)
		}
		p.Tags[tagType] = value
	}
	return p, nil
}

// Encode serialises a Discovery packet with tags in insertion order.
func Encode(code byte, sessionID uint16, tags []Tag) []byte {
	body := make([]byte, 0, 128)
	for _, tag := range tags {
		if len(tag.Value) > 65535 {
			tag.Value = tag.Value[:65535]
		}
		var hdr [4]byte
		binary.BigEndian.PutUint16(hdr[0:2], tag.Type)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(tag.Value))) //nolint:gosec
		body = append(body, hdr[:]...)
		body = append(body, tag.Value...)
	}
	out := make([]byte, HeaderLen, HeaderLen+len(body))
	out[0] = 0x11
	out[1] = code
	binary.BigEndian.PutUint16(out[2:4], sessionID)
	binary.BigEndian.PutUint16(out[4:6], uint16(len(body))) //nolint:gosec
	return append(out, body...)
}

// EncodeSession serialises a Session-phase PPP frame.
func EncodeSession(sessionID uint16, protocol uint16, payload []byte) []byte {
	body := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(body[0:2], protocol)
	copy(body[2:], payload)
	out := make([]byte, HeaderLen, HeaderLen+len(body))
	out[0] = 0x11
	out[1] = CodeSession
	binary.BigEndian.PutUint16(out[2:4], sessionID)
	binary.BigEndian.PutUint16(out[4:6], uint16(len(body))) //nolint:gosec
	return append(out, body...)
}

// Tag is a PPPoE Discovery TLV.
type Tag struct {
	Type  uint16
	Value []byte
}

// PAPCredentials is the decoded PAP Authenticate-Request (RFC 1334 §2.2.1).
type PAPCredentials struct {
	Identifier uint8
	PeerID     string
	Password   string
}

// ParsePAP decodes a PAP Authenticate-Request.
func ParsePAP(payload []byte) (*PAPCredentials, error) {
	if len(payload) < 7 {
		return nil, ErrBadPAP
	}
	if payload[0] != PAPAuthenticateRequest {
		return nil, fmt.Errorf("%w: code %d", ErrBadPAP, payload[0])
	}
	peerLen := int(payload[4])
	if 5+peerLen+1 > len(payload) {
		return nil, ErrBadPAP
	}
	peerID := string(payload[5 : 5+peerLen])
	passLen := int(payload[5+peerLen])
	if 6+peerLen+passLen > len(payload) {
		return nil, ErrBadPAP
	}
	password := string(payload[6+peerLen : 6+peerLen+passLen])
	return &PAPCredentials{Identifier: payload[1], PeerID: peerID, Password: password}, nil
}

// EncodePAP builds a PAP Authenticate-Request (used in tests/simulators).
func EncodePAP(identifier byte, peerID, password string) []byte {
	out := []byte{PAPAuthenticateRequest, identifier, 0, 0} // length patched below
	out = append(out, byte(len(peerID)))
	out = append(out, peerID...)
	out = append(out, byte(len(password)))
	out = append(out, password...)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out))) //nolint:gosec
	return out
}

// EthernetFrame wraps a PPPoE payload in an Ethernet II header.
func EthernetFrame(dst, src net.HardwareAddr, etherType uint16, payload []byte) ([]byte, error) {
	if len(dst) != 6 || len(src) != 6 {
		return nil, errors.New("pppoe: Ethernet MAC must be 6 bytes")
	}
	out := make([]byte, 14, 14+len(payload))
	copy(out[0:6], dst)
	copy(out[6:12], src)
	binary.BigEndian.PutUint16(out[12:14], etherType)
	return append(out, payload...), nil
}

// ParseEthernetFrame strips the Ethernet header, returning etherType + PPPoE payload.
func ParseEthernetFrame(frame []byte) (etherType uint16, payload []byte, err error) {
	if len(frame) < 14 {
		return 0, nil, ErrShortFrame
	}
	return binary.BigEndian.Uint16(frame[12:14]), frame[14:], nil
}

// DiscoveryState models the client-visible PADI->PADO->PADR->PADS sequence.
type DiscoveryState string

const (
	StateIdle     DiscoveryState = "idle"
	StatePADI     DiscoveryState = "padi_sent"
	StatePADO     DiscoveryState = "pado_received"
	StatePADR     DiscoveryState = "padr_sent"
	StateSession  DiscoveryState = "session"
	StateTerminal DiscoveryState = "padt"
)

// Session represents one negotiated PPPoE session.
type Session struct {
	SessionID uint16
	MAC       net.HardwareAddr
	Service   string
	State     DiscoveryState
	HostUniq  []byte
}

// ParseServiceName returns the Service-Name tag value (RFC 2516 §5.1).
func (p *Packet) ServiceName() string {
	if v, ok := p.Tags[TagServiceName]; ok {
		return string(v)
	}
	return ""
}

// HandleDiscovery is the AC-side state machine. It returns the reply code,
// session ID and tags to send, or an error when the packet must be dropped.
func HandleDiscovery(p *Packet, acName string, nextSessionID uint16) (byte, uint16, []Tag, error) {
	switch p.Code {
	case CodePADI:
		// RFC 2516 §5.2: PADO offers AC-Name and an empty Service-Name tag.
		return CodePADO, 0, []Tag{
			{Type: TagACName, Value: []byte(acName)},
			{Type: TagServiceName, Value: p.Tags[TagServiceName]},
		}, nil
	case CodePADR:
		return CodePADS, nextSessionID, []Tag{
			{Type: TagServiceName, Value: p.Tags[TagServiceName]},
		}, nil
	case CodePADT:
		return CodePADT, p.SessionID, nil, nil
	default:
		return 0, 0, nil, fmt.Errorf("pppoe: unexpected discovery code 0x%02x", p.Code)
	}
}
