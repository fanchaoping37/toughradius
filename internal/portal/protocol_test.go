package portal

import (
	"bytes"
	"crypto/md5" //nolint:gosec // test computes the protocol-mandated MD5
	"net"
	"testing"
)

func testSecret() []byte { return []byte("portal-shared-secret") }

func testChallenge() []byte {
	return []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
}

// independentAuth computes the expected authenticator without using the
// implementation under test. requestAuth nil means request-style (zeroed).
func independentAuth(t *testing.T, p *Packet, secret []byte, requestAuth *[16]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	head := make([]byte, HeaderLenV2)
	head[0] = p.Version
	head[1] = byte(p.Type)
	head[2] = p.AuthType
	binaryBEUint16(head[4:6], p.SerialNo)
	binaryBEUint16(head[6:8], p.ReqID)
	copy(head[8:12], p.UserIP.To4())
	binaryBEUint16(head[12:14], p.UserPort)
	head[14] = p.ErrCode
	head[15] = byte(len(p.Attributes))
	if requestAuth != nil {
		copy(head[16:32], requestAuth[:])
	}
	buf.Write(head)
	for _, a := range p.Attributes {
		buf.WriteByte(a.Type)
		buf.WriteByte(byte(len(a.Value) + 2))
		buf.Write(a.Value)
	}
	sum := md5.Sum(append(buf.Bytes(), secret...)) //nolint:gosec // test vector
	return sum[:]
}

func binaryBEUint16(dst []byte, v uint16) {
	dst[0] = byte(v >> 8)
	dst[1] = byte(v)
}

func TestEncodeRequestAuthenticator(t *testing.T) {
	p := NewPacket(TypeReqChallenge, AuthChap, 0x1234, 0, net.ParseIP("192.168.1.100"), 0)
	if p.AddStringAttr(AttrUserName, "alice@campus") != nil {
		t.Fatal("AddStringAttr failed")
	}
	wire, err := p.EncodeRequest(testSecret())
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != HeaderLenV2+2+len("alice@campus") {
		t.Fatalf("wire length = %d", len(wire))
	}
	want := independentAuth(t, p, testSecret(), nil)
	if !bytes.Equal(wire[16:32], want) {
		t.Errorf("request authenticator = %x, want %x", wire[16:32], want)
	}
	if !p.Verify(testSecret(), nil) {
		t.Error("Verify(request) failed")
	}
	if p.Verify([]byte("wrong-secret"), nil) {
		t.Error("Verify accepted wrong secret")
	}
}

func TestEncodeResponseAuthenticator(t *testing.T) {
	reqAuth := [16]byte{0xaa, 0xbb, 0xcc, 0xdd, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	p := NewPacket(TypeAckChallenge, AuthChap, 0x1234, 0x5678, net.ParseIP("192.168.1.100"), 0)
	if err := p.AddAttr(AttrChallenge, testChallenge()); err != nil {
		t.Fatal(err)
	}
	wire, err := p.EncodeResponse(testSecret(), reqAuth)
	if err != nil {
		t.Fatal(err)
	}
	want := independentAuth(t, p, testSecret(), &reqAuth)
	if !bytes.Equal(wire[16:32], want) {
		t.Errorf("response authenticator = %x, want %x", wire[16:32], want)
	}
	if !p.Verify(testSecret(), &reqAuth) {
		t.Error("Verify(response) failed")
	}
	if p.Verify(testSecret(), nil) {
		t.Error("Verify(response) accepted nil request authenticator")
	}
}

func TestParseRoundTrip(t *testing.T) {
	p := NewPacket(TypeReqAuth, AuthChap, 0x1234, 0x5678, net.ParseIP("10.0.0.1"), 0)
	_ = p.AddStringAttr(AttrUserName, "alice")
	chap, err := ChapPassword(ChapID(p.ReqID), "secret123", testChallenge())
	if err != nil {
		t.Fatal(err)
	}
	_ = p.AddAttr(AttrChapPassword, chap[:])
	wire, err := p.EncodeRequest(testSecret())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != VersionV2 || got.Type != TypeReqAuth || got.AuthType != AuthChap ||
		got.SerialNo != 0x1234 || got.ReqID != 0x5678 || got.ErrCode != 0 ||
		!got.UserIP.Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("header mismatch: %+v", got.Header)
	}
	if len(got.Attributes) != 2 || got.GetStringAttr(AttrUserName) != "alice" {
		t.Errorf("attributes mismatch: %+v", got.Attributes)
	}
	gotChap, ok := got.GetAttr(AttrChapPassword)
	if !ok || !bytes.Equal(gotChap, chap[:]) {
		t.Errorf("chap password mismatch: %x", gotChap)
	}
	if !got.Verify(testSecret(), nil) {
		t.Error("parsed packet failed verification")
	}
}

func TestChapPasswordVector(t *testing.T) {
	h := md5.New() //nolint:gosec // test vector
	h.Write([]byte{0x78})
	h.Write([]byte("campuspwd"))
	h.Write(testChallenge())
	want := h.Sum(nil)
	got, err := ChapPassword(0x78, "campuspwd", testChallenge())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:], want) {
		t.Errorf("CHAP password = %x, want %x", got, want)
	}
	if ChapID(0x1278) != 0x78 {
		t.Error("ChapID must use the low byte of ReqID")
	}
	if _, err := ChapPassword(1, "pwd", []byte("short")); err == nil {
		t.Error("expected error for short challenge")
	}
}

func TestParseRejectsMalformedPackets(t *testing.T) {
	valid := NewPacket(TypeReqChallenge, AuthChap, 1, 0, net.ParseIP("1.2.3.4"), 0)
	_ = valid.AddStringAttr(AttrUserName, "alice")
	wire, _ := valid.EncodeRequest(testSecret())

	tests := []struct {
		name string
		data []byte
	}{
		{"short", wire[:10]},
		{"wrong version", append([]byte{0x01}, wire[1:]...)},
		{"truncated attribute header", wire[:HeaderLenV2+1]},
		{"bad attribute length", func() []byte {
			b := append([]byte(nil), wire...)
			b[HeaderLenV2+1] = 1
			return b
		}()},
		{"trailing bytes", append(append([]byte(nil), wire...), 0x00)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.data); err == nil {
				t.Error("expected parse error")
			}
		})
	}
}
