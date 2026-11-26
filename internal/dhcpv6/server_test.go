package dhcpv6

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func duidValue() []byte {
	v := make([]byte, 14)
	binary.BigEndian.PutUint16(v[0:2], DUID_LLT)
	binary.BigEndian.PutUint16(v[2:4], 1) // hw type ethernet
	v[10], v[11], v[12], v[13] = 0x00, 0x11, 0x22, 0x33
	return v
}

func solicitPacket(tx [3]byte) []byte {
	ia, _ := IA_NAOption(0x1234, net.ParseIP("2001:db8:1::1"), 0, 0, 0, 0)
	return Encode(MsgSolicit, tx, []Option{
		{Code: OptClientID, Value: duidValue()},
		ia,
		{Code: OptORO, Value: []byte{0, OptDNS, 0, OptCaptiveURI}},
	})
}

func TestParseAndEncode(t *testing.T) {
	pkt := solicitPacket([3]byte{1, 2, 3})
	p, err := Parse(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if p.MsgType != MsgSolicit || p.TxID != [3]byte{1, 2, 3} {
		t.Fatalf("header: %+v", p)
	}
	if _, ok := p.Options[OptClientID]; !ok {
		t.Fatal("client DUID option missing")
	}
	duid, err := ParseDUID(p.ClientDUID())
	if err != nil || duid.Type != DUID_LLT {
		t.Fatalf("DUID: %+v err=%v", duid, err)
	}
}

func TestDUIDTypes(t *testing.T) {
	for _, typ := range []uint16{DUID_LLT, DUID_EN, DUID_LL, DUID_UUID} {
		v := make([]byte, 6)
		binary.BigEndian.PutUint16(v[0:2], typ)
		d, err := ParseDUID(v)
		if err != nil || uint16(d.Type) != typ {
			t.Fatalf("DUID type %d: %+v err=%v", typ, d, err)
		}
		if d.Key() == "" {
			t.Fatal("empty DUID key")
		}
	}
	if _, err := ParseDUID([]byte{0}); err != ErrBadOption {
		t.Fatalf("short DUID: %v", err)
	}
}

func TestIA_NAOption(t *testing.T) {
	addr := net.ParseIP("2001:db8:1::42")
	opt, err := IA_NAOption(7, addr, 3600, 7200, 1800, 6300)
	if err != nil {
		t.Fatal(err)
	}
	ia, err := ParseIA_NA(opt.Value)
	if err != nil {
		t.Fatal(err)
	}
	if ia.IAID != 7 || ia.T1 != 1800 || ia.T2 != 6300 || !ia.Addr.Equal(addr) ||
		ia.Preferred != 3600 || ia.Valid != 7200 {
		t.Fatalf("IA_NA: %+v", ia)
	}
}

func TestServerAssignAndTrace(t *testing.T) {
	_, prefix, _ := net.ParseCIDR("2001:db8:1::/64")
	s, err := NewServer(Config{
		Prefix:    *prefix,
		DNS:       []net.IP{net.ParseIP("2001:db8::53")},
		PortalURL: "http://portal.example.com/login",
		Lease:     time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.Handle(solicitPacket([3]byte{9, 8, 7}))
	if resp == nil {
		t.Fatal("no reply")
	}
	p, err := Parse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if p.MsgType != MsgReply || p.TxID != [3]byte{9, 8, 7} {
		t.Fatalf("reply header: %+v", p)
	}
	if _, ok := p.Options[OptCaptiveURI]; !ok {
		t.Fatal("RFC 7710 portal URI option missing")
	}
	ia, err := ParseIA_NA(p.Options[OptIA_NA])
	if err != nil || ia.Addr == nil {
		t.Fatalf("reply IA_NA: %+v err=%v", ia, err)
	}
	if !prefix.Contains(ia.Addr) {
		t.Fatalf("assigned address %s outside prefix", ia.Addr)
	}

	duid, _ := ParseDUID(duidValue())
	binding, ok := s.LookupByAddr(ia.Addr)
	if !ok || binding.DUID != duid.Key() {
		t.Fatalf("binding: %+v ok=%v", binding, ok)
	}
	if !s.SetUsername(duid.Key(), "student01") {
		t.Fatal("set username failed")
	}
	binding, _ = s.LookupByAddr(ia.Addr)
	if binding.Username != "student01" {
		t.Fatalf("username not traced: %+v", binding)
	}

	// Same DUID + IAID gets the same address (stability across renew).
	resp2 := s.Handle(solicitPacket([3]byte{1, 1, 1}))
	p2, _ := Parse(resp2)
	ia2, _ := ParseIA_NA(p2.Options[OptIA_NA])
	if !ia2.Addr.Equal(ia.Addr) {
		t.Fatalf("address changed on renew: %s -> %s", ia.Addr, ia2.Addr)
	}
}

func TestInformationRequest(t *testing.T) {
	_, prefix, _ := net.ParseCIDR("2001:db8:2::/64")
	s, _ := NewServer(Config{Prefix: *prefix, PortalURL: "http://portal"})
	req := Encode(MsgInformationRequest, [3]byte{4, 5, 6}, nil)
	resp := s.Handle(req)
	p, err := Parse(resp)
	if err != nil || p.MsgType != MsgReply {
		t.Fatalf("info reply: %+v err=%v", p, err)
	}
	if string(p.Options[OptCaptiveURI]) != "http://portal" {
		t.Fatal("portal URI missing")
	}
}
