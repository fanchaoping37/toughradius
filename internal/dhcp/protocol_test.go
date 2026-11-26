package dhcp

import (
	"encoding/binary"
	"net"
	"testing"
)

func discoverPacket() []byte {
	pkt := make([]byte, HeaderLen+4)
	pkt[0] = OpBootRequest
	pkt[1] = 1
	pkt[2] = 6
	binary.BigEndian.PutUint32(pkt[4:8], 0x11223344)
	binary.BigEndian.PutUint16(pkt[10:12], 0x8000) // broadcast flag
	copy(pkt[28:34], []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55})
	binary.BigEndian.PutUint32(pkt[HeaderLen:HeaderLen+4], MagicCookie)
	pkt = append(pkt, OptMsgType, 1, MsgDiscover, OptEnd)
	return pkt
}

func TestParseDiscover(t *testing.T) {
	p, err := Parse(discoverPacket())
	if err != nil {
		t.Fatal(err)
	}
	if p.MsgType != MsgDiscover {
		t.Fatalf("msg type = %d", p.MsgType)
	}
	if p.XID != 0x11223344 {
		t.Fatalf("xid = %x", p.XID)
	}
	if p.MAC() != "00:11:22:33:44:55" {
		t.Fatalf("mac = %s", p.MAC())
	}
}

func TestParseRejectsBadCookieAndShort(t *testing.T) {
	if _, err := Parse([]byte{1, 2, 3}); err != ErrShortPacket {
		t.Fatalf("short: %v", err)
	}
	bad := discoverPacket()
	bad[HeaderLen] = 0
	if _, err := Parse(bad); err != ErrBadCookie {
		t.Fatalf("cookie: %v", err)
	}
}

func TestReplyEchoesOption82Last(t *testing.T) {
	p, _ := Parse(discoverPacket())
	sub := []byte{SubOptCircuitID, 4, 'e', 't', 'h', '0', SubOptRemoteID, 2, 'A', 'B'}
	p.RelayAgent = sub
	reply, err := p.Reply(MsgOffer, net.ParseIP("192.168.10.50"), net.ParseIP("10.0.0.1"),
		[]Option{IPOption(OptRouter, net.ParseIP("192.168.10.1"))})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Op != OpBootReply || parsed.MsgType != MsgOffer {
		t.Fatalf("reply op/type = %d/%d", parsed.Op, parsed.MsgType)
	}
	if string(parsed.RelayAgent) != string(sub) {
		t.Fatalf("option 82 not echoed: %x", parsed.RelayAgent)
	}
	if parsed.CircuitID() != "eth0" || parsed.RemoteID() != "AB" {
		t.Fatalf("suboptions: %q %q", parsed.CircuitID(), parsed.RemoteID())
	}
	// Option 82 must be the last option before End.
	if reply[len(reply)-1-len(sub)-2] != OptRelayAgent {
		t.Fatal("option 82 not last")
	}
}

func TestIPOptionAndUint32(t *testing.T) {
	o := IPOption(OptRouter, net.ParseIP("1.2.3.4"), net.ParseIP("5.6.7.8"))
	if len(o.Value) != 8 || o.Value[4] != 5 {
		t.Fatalf("ip option: %x", o.Value)
	}
	u := Uint32Option(OptLeaseTime, 3600)
	if binary.BigEndian.Uint32(u.Value) != 3600 {
		t.Fatalf("uint32 option: %x", u.Value)
	}
}

func TestHandleDiscoverOfferAndAck(t *testing.T) {
	s, err := NewServer(ServerConfig{
		ServerIP: net.ParseIP("10.0.0.1"),
		Pools: []Pool{{
			Network: "192.168.10.0/24", Gateway: "192.168.10.1",
			DNS: []string{"8.8.8.8"}, Start: "192.168.10.10", End: "192.168.10.20",
			Lease: 3600e9, PortalURL: "http://portal.example.com/login",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, dst, err := s.handlePacket(discoverPacket())
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(reply)
	if err != nil {
		t.Fatal(err)
	}
	if p.MsgType != MsgOffer || p.YIAddr.String() != "192.168.10.10" {
		t.Fatalf("offer: type=%d ip=%s", p.MsgType, p.YIAddr)
	}
	if string(p.Options[OptCaptivePortal]) != "http://portal.example.com/login" {
		t.Fatalf("option 160 missing: %v", p.Options)
	}
	if string(p.Options[OptPortalURL243]) != "http://portal.example.com/login" {
		t.Fatal("legacy option 243 missing")
	}
	if dst.IP.String() != "255.255.255.255" || dst.Port != ClientPort {
		t.Fatalf("broadcast dst = %s", dst)
	}

	// DHCPREQUEST selecting the offered address => ACK with the same IP.
	req := discoverPacket()
	req[len(req)-1] = MsgRequest
	req = append(req[:len(req)-1], OptRequestedIP, 4, 192, 168, 10, 10, OptServerID, 4, 10, 0, 0, 1, OptEnd)
	// Rebuild tail correctly: remove the original OptEnd first.
	req = discoverPacket()
	req = req[:len(req)-1]
	req = append(req, OptMsgType, 1, MsgRequest, OptRequestedIP, 4, 192, 168, 10, 10,
		OptServerID, 4, 10, 0, 0, 1, OptEnd)
	reply, _, err = s.handlePacket(req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ = Parse(reply)
	if p.MsgType != MsgAck || p.YIAddr.String() != "192.168.10.10" {
		t.Fatalf("ack: type=%d ip=%s", p.MsgType, p.YIAddr)
	}
}

func TestOption82RelayReplyGoesToRelay(t *testing.T) {
	s, err := NewServer(ServerConfig{
		ServerIP: net.ParseIP("10.0.0.1"),
		Pools: []Pool{{
			Network: "192.168.20.0/24", Gateway: "192.168.20.1",
			Start: "192.168.20.10", End: "192.168.20.20", Lease: 3600e9,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pkt := discoverPacket()
	copy(pkt[24:28], net.ParseIP("192.168.20.1").To4())
	sub := []byte{SubOptCircuitID, 2, 'p', '1'}
	tail := []byte{OptMsgType, 1, MsgDiscover, OptRelayAgent, byte(len(sub))}
	tail = append(tail, sub...)
	tail = append(tail, OptEnd)
	pkt = append(pkt[:HeaderLen+4], tail...)
	_, dst, err := s.handlePacket(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if dst.IP.String() != "192.168.20.1" || dst.Port != ServerPort {
		t.Fatalf("relay dst = %s:%d", dst.IP, dst.Port)
	}
}

func TestReleaseAndLookup(t *testing.T) {
	s, err := NewServer(ServerConfig{
		ServerIP: net.ParseIP("10.0.0.1"),
		Pools: []Pool{{
			Network: "10.10.0.0/24", Gateway: "10.10.0.1",
			Start: "10.10.0.2", End: "10.10.0.3", Lease: 3600e9,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.handlePacket(discoverPacket()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupLease("00:11:22:33:44:55"); !ok {
		t.Fatal("lease not found")
	}
	if !s.SetAuthorized("00:11:22:33:44:55", true) {
		t.Fatal("set authorized failed")
	}
	release := discoverPacket()
	release = release[:len(release)-1]
	release = append(release, OptMsgType, 1, MsgRelease, OptEnd)
	reply, _, err := s.handlePacket(release)
	if err != nil || reply != nil {
		t.Fatalf("release: reply=%v err=%v", reply, err)
	}
	if _, ok := s.LookupLease("00:11:22:33:44:55"); ok {
		t.Fatal("lease still present after release")
	}
}
