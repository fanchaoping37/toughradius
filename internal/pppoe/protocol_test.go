package pppoe

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func TestParseEncodeDiscovery(t *testing.T) {
	frame := Encode(CodePADI, 0, []Tag{
		{Type: TagServiceName, Value: []byte("campus")},
		{Type: TagHostUniq, Value: []byte{1, 2, 3, 4}},
	})
	p, err := Parse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != CodePADI || p.SessionID != 0 || p.ServiceName() != "campus" {
		t.Fatalf("parsed: code=%x sid=%d svc=%s", p.Code, p.SessionID, p.ServiceName())
	}
	if !bytes.Equal(p.Tags[TagHostUniq], []byte{1, 2, 3, 4}) {
		t.Fatal("Host-Uniq tag lost")
	}
	if binary.BigEndian.Uint16(frame[4:6]) != uint16(len(frame)-6) {
		t.Fatal("length field wrong")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	if _, err := Parse([]byte{1, 2, 3}); err != ErrShortFrame {
		t.Fatalf("short: %v", err)
	}
	bad := Encode(CodePADI, 0, nil)
	bad[0] = 0x12
	if _, err := Parse(bad); err != ErrBadVersion {
		t.Fatalf("version: %v", err)
	}
	badTag := Encode(CodePADI, 0, []Tag{{Type: TagServiceName, Value: []byte("abc")}})
	badTag[8] = 0xff // tag length high byte
	if _, err := Parse(badTag); err != ErrBadTag {
		t.Fatalf("tag: %v", err)
	}
}

func TestSessionFrameAndPAP(t *testing.T) {
	pap := EncodePAP(7, "student", "secret")
	frame := EncodeSession(0x1234, ProtoPAP, pap)
	p, err := Parse(frame)
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != CodeSession || p.SessionID != 0x1234 || p.Protocol != ProtoPAP {
		t.Fatalf("session frame: %+v", p)
	}
	creds, err := ParsePAP(p.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Identifier != 7 || creds.PeerID != "student" || creds.Password != "secret" {
		t.Fatalf("PAP creds: %+v", creds)
	}
}

func TestParsePAPRejectsTruncated(t *testing.T) {
	if _, err := ParsePAP([]byte{1, 2, 0}); err != ErrBadPAP {
		t.Fatalf("short: %v", err)
	}
	if _, err := ParsePAP([]byte{2, 1, 0, 5, 1, 'a'}); err == nil {
		t.Fatal("non-request code accepted")
	}
}

func TestEthernetFrame(t *testing.T) {
	dst, _ := net.ParseMAC("ff:ff:ff:ff:ff:ff")
	src, _ := net.ParseMAC("00:11:22:33:44:55")
	frame, err := EthernetFrame(dst, src, EtherTypeDiscovery, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	et, payload, err := ParseEthernetFrame(frame)
	if err != nil || et != EtherTypeDiscovery || len(payload) != 3 {
		t.Fatalf("ethernet: et=%x payload=%v err=%v", et, payload, err)
	}
}

func TestDiscoveryStateMachine(t *testing.T) {
	padi := Encode(CodePADI, 0, []Tag{{Type: TagServiceName, Value: []byte("campus")}})
	pkt, _ := Parse(padi)
	code, sid, tags, err := HandleDiscovery(pkt, "tr-ac", 0)
	if err != nil || code != CodePADO || sid != 0 {
		t.Fatalf("PADI handling: code=%x err=%v", code, err)
	}
	foundAC := false
	for _, tag := range tags {
		if tag.Type == TagACName && string(tag.Value) == "tr-ac" {
			foundAC = true
		}
	}
	if !foundAC {
		t.Fatal("AC-Name missing in PADO")
	}

	padr := Encode(CodePADR, 0, []Tag{{Type: TagServiceName, Value: []byte("campus")}})
	pkt, _ = Parse(padr)
	code, sid, _, err = HandleDiscovery(pkt, "tr-ac", 42)
	if err != nil || code != CodePADS || sid != 42 {
		t.Fatalf("PADR handling: code=%x sid=%d err=%v", code, sid, err)
	}

	padt := Encode(CodePADT, 42, nil)
	pkt, _ = Parse(padt)
	code, sid, _, err = HandleDiscovery(pkt, "tr-ac", 0)
	if err != nil || code != CodePADT || sid != 42 {
		t.Fatalf("PADT handling: code=%x sid=%d err=%v", code, sid, err)
	}
}
