// Package portal implements the Huawei / CMCC Portal protocol version 2
// (the de-facto standard for Web authentication in mainland China).
//
// Scope: server-side UDP protocol exchange with NAS devices — Challenge,
// Auth, Logout, Info and notification messages. RADIUS authentication is
// performed by the NAS against ToughRADIUS on UDP 1812; this package does
// not issue RADIUS requests itself and does not host a login page.
//
// Wire format (all multi-byte fields are big-endian):
//
//	offset 0  Ver(1) | Type(1) | AuthType(1) | Rsvd(1)
//	offset 4  SerialNo(2) | ReqID(2)
//	offset 8  UserIP(4) | UserPort(2) | ErrCode(1) | AttrNum(1)
//	offset 16 Authenticator(16)   ← MD5 over the whole packet + shared secret
//	offset 32 Attributes: Type(1) | Length(1) | Value(Length-2) ...
//
// For requests the Authenticator field is zeroed before the MD5; for
// responses the field is replaced with the corresponding request's
// Authenticator (the same anti-replay scheme used by RADIUS accounting).
package portal

import (
	"bytes"
	"crypto/md5" //nolint:gosec // Portal V2 mandates MD5 authenticators
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// Protocol constants.
const (
	VersionV2       byte = 0x02
	HeaderLenV2          = 32
	MaxPacketLen         = 1024
	MaxAttrValueLen      = 253
)

// MessageType identifies a Portal message.
type MessageType byte

const (
	TypeReqChallenge MessageType = 0x01 // Portal → NAS
	TypeAckChallenge MessageType = 0x02 // NAS → Portal
	TypeReqAuth      MessageType = 0x03 // Portal → NAS
	TypeAckAuth      MessageType = 0x04 // NAS → Portal
	TypeReqLogout    MessageType = 0x05 // Portal → NAS
	TypeAckLogout    MessageType = 0x06 // NAS → Portal
	TypeAffAckAuth   MessageType = 0x07 // Portal → NAS (no reply expected)
	TypeNtfLogout    MessageType = 0x08 // NAS → Portal (forced logout)
	TypeReqInfo      MessageType = 0x09 // Portal → NAS
	TypeAckInfo      MessageType = 0x0a // NAS → Portal
	TypeAckNtfLogout MessageType = 0x0e // Portal → NAS (optional)
	TypeUserSyn      MessageType = 0x10
	TypeAckUserSyn   MessageType = 0x11
	TypeStatusNotify MessageType = 0x81 // NAS → Portal (keepalive)
	TypeAckStatus    MessageType = 0x82 // Portal → NAS
)

func (t MessageType) String() string {
	switch t {
	case TypeReqChallenge:
		return "REQ_CHALLENGE"
	case TypeAckChallenge:
		return "ACK_CHALLENGE"
	case TypeReqAuth:
		return "REQ_AUTH"
	case TypeAckAuth:
		return "ACK_AUTH"
	case TypeReqLogout:
		return "REQ_LOGOUT"
	case TypeAckLogout:
		return "ACK_LOGOUT"
	case TypeAffAckAuth:
		return "AFF_ACK_AUTH"
	case TypeNtfLogout:
		return "NTF_LOGOUT"
	case TypeReqInfo:
		return "REQ_INFO"
	case TypeAckInfo:
		return "ACK_INFO"
	case TypeAckNtfLogout:
		return "ACK_NTF_LOGOUT"
	case TypeStatusNotify:
		return "STATUS_NOTIFY"
	case TypeAckStatus:
		return "ACK_STATUS_NOTIFY"
	default:
		return fmt.Sprintf("TYPE_0x%02x", byte(t))
	}
}

// Authentication modes, meaningful for REQ_AUTH only.
const (
	AuthChap byte = 0x00
	AuthPap  byte = 0x01
)

// Attribute types.
const (
	AttrUserName      byte = 0x01
	AttrPassword      byte = 0x02
	AttrChallenge     byte = 0x03
	AttrChapPassword  byte = 0x04
	AttrTextInfo      byte = 0x05
	AttrUpLinkFlux    byte = 0x06
	AttrDownLinkFlux  byte = 0x07
	AttrPort          byte = 0x08
	AttrBasIP         byte = 0x0a
	AttrUserMac       byte = 0x0b
	AttrUserPrivateIP byte = 0x0d
	AttrWebAuthenInfo byte = 0x40
	AttrUserIPv6      byte = 0xf1
)

// ErrCode values. Interpretation depends on the message type.
const (
	ErrSuccess         byte = 0x00
	ErrReject          byte = 0x01
	ErrLinkEstablished byte = 0x02 // user already online — treat as success
	ErrAuthenticating  byte = 0x03
	ErrFailed          byte = 0x04
	ErrOnlineLimit     byte = 0x05
	ErrOtherAuth       byte = 0x06
	ErrUserNotFound    byte = 0xfd
)

// Header is the fixed 32-byte V2 packet header.
type Header struct {
	Version       byte
	Type          MessageType
	AuthType      byte
	Reserved      byte
	SerialNo      uint16
	ReqID         uint16
	UserIP        net.IP // IPv4, 4 bytes
	UserPort      uint16
	ErrCode       byte
	AttrNum       byte
	Authenticator [16]byte
}

// AVP is one Portal TLV attribute. Length on the wire is len(Value)+2.
type AVP struct {
	Type  byte
	Value []byte
}

// Packet is a complete Portal V2 message.
type Packet struct {
	Header
	Attributes []AVP
}

// NewPacket builds a request/response packet with a zeroed authenticator.
func NewPacket(typ MessageType, authType byte, serial, reqID uint16, userIP net.IP, errCode byte) *Packet {
	return &Packet{Header: Header{
		Version:  VersionV2,
		Type:     typ,
		AuthType: authType,
		SerialNo: serial,
		ReqID:    reqID,
		UserIP:   userIP.To4(),
		ErrCode:  errCode,
	}}
}

// AddAttr appends an attribute, enforcing the V2 value length limit.
func (p *Packet) AddAttr(typ byte, value []byte) error {
	if len(value) > MaxAttrValueLen {
		return fmt.Errorf("portal: attribute 0x%02x value too long: %d", typ, len(value))
	}
	p.Attributes = append(p.Attributes, AVP{Type: typ, Value: append([]byte(nil), value...)})
	p.AttrNum = byte(len(p.Attributes))
	return nil
}

// AddStringAttr appends a string attribute.
func (p *Packet) AddStringAttr(typ byte, value string) error {
	return p.AddAttr(typ, []byte(value))
}

// GetAttr returns the first attribute of the given type.
func (p *Packet) GetAttr(typ byte) ([]byte, bool) {
	for _, a := range p.Attributes {
		if a.Type == typ {
			return a.Value, true
		}
	}
	return nil, false
}

// GetStringAttr returns the first attribute of the given type as a string.
func (p *Packet) GetStringAttr(typ byte) string {
	value, _ := p.GetAttr(typ)
	return string(value)
}

// Parse decodes a V2 packet. It validates the version, header length,
// attribute count and TLV boundaries, but not the authenticator — callers
// must verify that separately with the shared secret.
func Parse(data []byte) (*Packet, error) {
	if len(data) < HeaderLenV2 {
		return nil, fmt.Errorf("portal: packet too short: %d", len(data))
	}
	if data[0] != VersionV2 {
		return nil, fmt.Errorf("portal: unsupported version 0x%02x", data[0])
	}
	if len(data) > MaxPacketLen {
		return nil, fmt.Errorf("portal: packet too long: %d", len(data))
	}
	p := &Packet{Header: Header{
		Version:  data[0],
		Type:     MessageType(data[1]),
		AuthType: data[2],
		Reserved: data[3],
		SerialNo: binary.BigEndian.Uint16(data[4:6]),
		ReqID:    binary.BigEndian.Uint16(data[6:8]),
		UserIP:   net.IPv4(data[8], data[9], data[10], data[11]),
		UserPort: binary.BigEndian.Uint16(data[12:14]),
		ErrCode:  data[14],
		AttrNum:  data[15],
	}}
	copy(p.Authenticator[:], data[16:32])

	pos := HeaderLenV2
	for i := byte(0); i < p.AttrNum; i++ {
		if pos+2 > len(data) {
			return nil, fmt.Errorf("portal: truncated attribute header at %d", pos)
		}
		attrType := data[pos]
		attrLen := int(data[pos+1])
		if attrLen < 2 || pos+attrLen > len(data) {
			return nil, fmt.Errorf("portal: invalid attribute length %d at %d", attrLen, pos)
		}
		p.Attributes = append(p.Attributes, AVP{
			Type:  attrType,
			Value: append([]byte(nil), data[pos+2:pos+attrLen]...),
		})
		pos += attrLen
	}
	if pos != len(data) {
		return nil, fmt.Errorf("portal: trailing bytes after attributes: %d", len(data)-pos)
	}
	return p, nil
}

// encodeWithAuth serializes the packet using the supplied authenticator
// bytes in place of the on-wire field (zeros for requests, the corresponding
// request authenticator for responses), then writes the computed MD5 into
// both the returned bytes and p.Authenticator.
func (p *Packet) encodeWithAuth(secret []byte, authSource *[16]byte) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, HeaderLenV2+len(p.Attributes)*8))
	head := make([]byte, HeaderLenV2)
	head[0] = p.Version
	head[1] = byte(p.Type)
	head[2] = p.AuthType
	head[3] = p.Reserved
	binary.BigEndian.PutUint16(head[4:6], p.SerialNo)
	binary.BigEndian.PutUint16(head[6:8], p.ReqID)
	if ip := p.UserIP.To4(); ip != nil {
		copy(head[8:12], ip)
	}
	binary.BigEndian.PutUint16(head[12:14], p.UserPort)
	head[14] = p.ErrCode
	if len(p.Attributes) > 255 {
		return nil, fmt.Errorf("portal: too many attributes: %d", len(p.Attributes))
	}
	head[15] = byte(len(p.Attributes))
	if authSource != nil {
		copy(head[16:32], authSource[:])
	}
	buf.Write(head)
	for _, a := range p.Attributes {
		if len(a.Value) > MaxAttrValueLen {
			return nil, fmt.Errorf("portal: attribute 0x%02x value too long: %d", a.Type, len(a.Value))
		}
		buf.WriteByte(a.Type)
		buf.WriteByte(byte(len(a.Value) + 2))
		buf.Write(a.Value)
	}
	wire := buf.Bytes()
	if len(wire) > MaxPacketLen {
		return nil, fmt.Errorf("portal: encoded packet too long: %d", len(wire))
	}
	sum := md5.Sum(append(wire, secret...)) //nolint:gosec // protocol requirement
	copy(wire[16:32], sum[:])
	copy(p.Authenticator[:], sum[:])
	return wire, nil
}

// EncodeRequest signs and serializes a request (Authenticator zeroed for MD5).
func (p *Packet) EncodeRequest(secret []byte) ([]byte, error) {
	return p.encodeWithAuth(secret, nil)
}

// EncodeResponse signs and serializes a response against the given request
// authenticator (RFC-style anti-replay: response MD5 includes request auth).
func (p *Packet) EncodeResponse(secret []byte, requestAuth [16]byte) ([]byte, error) {
	return p.encodeWithAuth(secret, &requestAuth)
}

// Verify validates the packet authenticator. Pass requestAuth for responses
// (ACK_* messages) and nil for requests (including AFF_ACK_AUTH and
// NTF_LOGOUT, which use request-style authenticators).
func (p *Packet) Verify(secret []byte, requestAuth *[16]byte) bool {
	head := make([]byte, HeaderLenV2)
	head[0] = p.Version
	head[1] = byte(p.Type)
	head[2] = p.AuthType
	head[3] = p.Reserved
	binary.BigEndian.PutUint16(head[4:6], p.SerialNo)
	binary.BigEndian.PutUint16(head[6:8], p.ReqID)
	if ip := p.UserIP.To4(); ip != nil {
		copy(head[8:12], ip)
	}
	binary.BigEndian.PutUint16(head[12:14], p.UserPort)
	head[14] = p.ErrCode
	head[15] = byte(len(p.Attributes))
	if requestAuth != nil {
		copy(head[16:32], requestAuth[:])
	}
	for _, a := range p.Attributes {
		if len(a.Value) > MaxAttrValueLen {
			return false
		}
		head = append(head, a.Type, byte(len(a.Value)+2))
		head = append(head, a.Value...)
	}
	sum := md5.Sum(append(head, secret...)) //nolint:gosec // protocol requirement
	return bytes.Equal(sum[:], p.Authenticator[:])
}

// ChapPassword computes the 16-byte CHAP password:
//
//	MD5(chapID || password || challenge), chapID = low byte of ReqID
//
// Password is used as-is (no zero padding); the challenge comes from
// ACK_CHALLENGE and must be exactly 16 bytes.
func ChapPassword(chapID byte, password string, challenge []byte) ([16]byte, error) {
	var out [16]byte
	if len(challenge) != 16 {
		return out, errors.New("portal: challenge must be 16 bytes")
	}
	h := md5.New() //nolint:gosec // protocol requirement
	h.Write([]byte{chapID})
	h.Write([]byte(password))
	h.Write(challenge)
	copy(out[:], h.Sum(nil))
	return out, nil
}

// ChapID returns the CHAP identifier carried in a Portal ReqID.
func ChapID(reqID uint16) byte {
	return byte(reqID & 0xff)
}
