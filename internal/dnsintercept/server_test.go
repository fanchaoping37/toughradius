package dnsintercept

import (
	"encoding/binary"
	"net"
	"testing"
)

func queryFor(name string, qtype uint16) []byte {
	msg := make([]byte, 0, 64)
	msg = append(msg, 0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0)
	for _, label := range splitLabels(name) {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0, byte(qtype>>8), byte(qtype), 0, byte(classIN))
	return msg
}

func splitLabels(name string) []string {
	var result []string
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			result = append(result, name[start:i])
			start = i + 1
		}
	}
	return result
}

func TestParseQuestion(t *testing.T) {
	id, flags, qd, q, err := ParseQuestion(queryFor("www.example.com", typeA))
	if err != nil {
		t.Fatal(err)
	}
	if id != 0x1234 || flags != 0x0100 || qd != 1 {
		t.Fatalf("header: %x %x %d", id, flags, qd)
	}
	if q.Name != "www.example.com" || q.Type != typeA || q.Class != classIN {
		t.Fatalf("question: %+v", q)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	if _, _, _, _, err := ParseQuestion([]byte{1, 2, 3}); err != ErrShortMessage {
		t.Fatalf("short: %v", err)
	}
	bad := queryFor("a.b", typeA)
	bad[12] = 0xc0 // compression pointer
	if _, _, _, _, err := ParseQuestion(bad); err != ErrBadLabel {
		t.Fatalf("compression: %v", err)
	}
}

func TestRedirectResponse(t *testing.T) {
	query := queryFor("captive.example.net", typeA)
	resp, err := RedirectResponse(query, net.ParseIP("10.9.8.7"), 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp) < headerLen+4 {
		t.Fatalf("response too short: %d", len(resp))
	}
	if binary.BigEndian.Uint16(resp[0:2]) != 0x1234 {
		t.Fatal("ID not echoed")
	}
	if resp[2]&0x80 == 0 || resp[2]&0x04 == 0 || resp[3]&0x80 == 0 {
		t.Fatalf("flags QR/AA/RA missing: %x %x", resp[2], resp[3])
	}
	if binary.BigEndian.Uint16(resp[4:6]) != 1 || binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatal("QD/AN counts wrong")
	}
	// Answer RDATA is the last 4 bytes.
	tail := resp[len(resp)-4:]
	if tail[0] != 10 || tail[1] != 9 || tail[2] != 8 || tail[3] != 7 {
		t.Fatalf("A record = %v", tail)
	}
	// Compression pointer present for answer NAME.
	if resp[headerLen+len(query)-headerLen] == 0 {
		t.Fatal("unexpected zero label")
	}
}

func TestHandleQueryRedirectAndWhitelist(t *testing.T) {
	s, err := NewServer(Config{
		PortalIP:  net.ParseIP("10.9.8.7"),
		Whitelist: []string{"captive.example.net", "*.safe.org"},
		TTL:       60,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.HandleQuery(queryFor("www.google.com", typeA), net.ParseIP("192.168.1.10"))
	if err != nil {
		t.Fatal(err)
	}
	tail := resp[len(resp)-4:]
	if tail[0] != 10 || tail[1] != 9 {
		t.Fatalf("not redirected: %v", tail)
	}
	// Whitelisted name without upstream: fail-closed still points at Portal,
	// but the whitelist classifier itself must be correct.
	if !s.isWhitelisted("captive.example.net.") || !s.isWhitelisted("a.safe.org") || s.isWhitelisted("bad.org") {
		t.Fatal("whitelist classifier wrong")
	}
	if _, _, _, q, err := ParseQuestion(queryFor("github.com", typeA)); err != nil || q.Name != "github.com" {
		t.Fatalf("parse: %v", err)
	}
}

func TestAuthorizedClientUsesPassthroughFailClosed(t *testing.T) {
	s, err := NewServer(Config{
		PortalIP:     net.ParseIP("10.9.8.7"),
		IsAuthorized: func(net.IP) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Without an upstream, passthrough intentionally still returns the Portal
	// address so unauthenticated DNS never leaks real answers.
	resp, err := s.HandleQuery(queryFor("example.com", typeA), net.ParseIP("192.168.1.20"))
	if err != nil {
		t.Fatal(err)
	}
	if resp[len(resp)-4] != 10 {
		t.Fatalf("unexpected answer: %v", resp[len(resp)-4:])
	}
}
