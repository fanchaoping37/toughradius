package portal

import (
	"net"
	"sync"
	"testing"
	"time"
)

const testNASSecret = "nas-portal-secret"

// fakeNAS is a loopback UDP endpoint that behaves like a Portal-capable NAS.
type fakeNAS struct {
	conn       net.PacketConn
	secret     string
	challenge  []byte
	reqID      uint16
	mu         sync.Mutex
	received   []*Packet
	authResult byte
	info       string
}

func newFakeNAS(t *testing.T) *fakeNAS {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNAS{
		conn:       conn,
		secret:     testNASSecret,
		challenge:  testChallenge(),
		reqID:      0x1234,
		authResult: ErrSuccess,
		info:       "welcome",
	}
	go f.serve()
	t.Cleanup(func() { _ = conn.Close() })
	return f
}

func (f *fakeNAS) addr() string { return f.conn.LocalAddr().String() }

func (f *fakeNAS) serve() {
	buf := make([]byte, MaxPacketLen+1)
	for {
		n, remote, err := f.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		req, err := Parse(buf[:n])
		if err != nil {
			continue
		}
		f.mu.Lock()
		f.received = append(f.received, req)
		f.mu.Unlock()

		var resp *Packet
		switch req.Type {
		case TypeReqChallenge:
			resp = NewPacket(TypeAckChallenge, req.AuthType, req.SerialNo, f.reqID, req.UserIP, 0)
			_ = resp.AddAttr(AttrChallenge, f.challenge)
		case TypeReqAuth:
			resp = NewPacket(TypeAckAuth, req.AuthType, req.SerialNo, req.ReqID, req.UserIP, f.authResult)
			if f.info != "" {
				_ = resp.AddStringAttr(AttrTextInfo, f.info)
			}
		case TypeReqLogout:
			resp = NewPacket(TypeAckLogout, 0, req.SerialNo, req.ReqID, req.UserIP, 0)
		case TypeReqInfo:
			resp = NewPacket(TypeAckInfo, 0, req.SerialNo, req.ReqID, req.UserIP, 0)
		default:
			continue // AFF_ACK_AUTH and ACK_NTF_LOGOUT need no reply
		}
		wire, err := resp.EncodeResponse([]byte(f.secret), req.Authenticator)
		if err == nil {
			_, _ = f.conn.WriteTo(wire, remote)
		}
	}
}

func (f *fakeNAS) packets() []*Packet {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Packet(nil), f.received...)
}

func newTestServer(t *testing.T, nas *fakeNAS) *Server {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(ServerConfig{
		BindHost: "127.0.0.1",
		Timeout:  500 * time.Millisecond,
		Retries:  1,
		SecretFor: func(host string) (string, bool) {
			if host == "127.0.0.1" {
				return testNASSecret, true
			}
			return "", false
		},
	})
	s.conn = conn
	go func() { _ = s.serve(conn) }()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestChallengeAuthLogoutFlow(t *testing.T) {
	nas := newFakeNAS(t)
	s := newTestServer(t, nas)
	userIP := net.ParseIP("192.168.10.5")
	nasDev := &NAS{Addr: nas.addr(), Secret: testNASSecret}

	session, err := s.Challenge(nasDev, "alice@campus", userIP)
	if err != nil {
		t.Fatalf("Challenge failed: %v", err)
	}
	if session.ReqID != nas.reqID || len(session.Challenge) != 16 {
		t.Errorf("bad session: %+v", session)
	}
	if got, ok := s.SessionByIP(userIP.String()); !ok || got.SerialNo != session.SerialNo {
		t.Error("session not indexed by user IP")
	}

	errCode, info, err := s.Authenticate(nasDev, userIP, "alice@campus", "campuspwd", false)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}
	if errCode != ErrSuccess || info != "welcome" {
		t.Errorf("auth result = 0x%02x / %q", errCode, info)
	}
	waitFor(t, func() bool {
		for _, p := range nas.packets() {
			if p.Type == TypeAffAckAuth {
				return true
			}
		}
		return false
	})

	if err := s.Logout(nasDev, userIP); err != nil {
		t.Fatalf("Logout failed: %v", err)
	}
	if _, ok := s.SessionByIP(userIP.String()); ok {
		t.Error("session should be removed after logout")
	}

	// Verify CHAP password on the wire is MD5(low ReqID || password || challenge).
	var authPacket *Packet
	for _, p := range nas.packets() {
		if p.Type == TypeReqAuth {
			authPacket = p
			break
		}
	}
	if authPacket == nil {
		t.Fatal("NAS never received REQ_AUTH")
	}
	want, _ := ChapPassword(ChapID(nas.reqID), "campuspwd", testChallenge())
	got, _ := authPacket.GetAttr(AttrChapPassword)
	if string(got) != string(want[:]) {
		t.Errorf("wire CHAP password = %x, want %x", got, want)
	}
}

func TestPAPFlowSkipsChallenge(t *testing.T) {
	nas := newFakeNAS(t)
	s := newTestServer(t, nas)
	// PAP still needs a serial/reqid from a Challenge in this implementation
	// (the session anchors SerialNo). Verify the plaintext password attribute.
	userIP := net.ParseIP("10.1.1.9")
	nasDev := &NAS{Addr: nas.addr()}
	if _, err := s.Challenge(nasDev, "bob", userIP); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Authenticate(nasDev, userIP, "bob", "plaintext", true); err != nil {
		t.Fatal(err)
	}
	for _, p := range nas.packets() {
		if p.Type == TypeReqAuth {
			if p.AuthType != AuthPap {
				t.Error("PAP flow did not set AuthType=1")
			}
			if pwd := p.GetStringAttr(AttrPassword); pwd != "plaintext" {
				t.Errorf("PAP password = %q", pwd)
			}
			return
		}
	}
	t.Error("NAS never received REQ_AUTH")
}

func TestNoSessionAndUnknownNAS(t *testing.T) {
	nas := newFakeNAS(t)
	s := newTestServer(t, nas)
	if _, _, err := s.Authenticate(&NAS{Addr: nas.addr()}, net.ParseIP("1.2.3.4"), "u", "p", false); err != ErrNoSession {
		t.Errorf("expected ErrNoSession, got %v", err)
	}
	if _, err := s.Challenge(&NAS{Addr: "203.0.113.9:2000"}, "u", net.ParseIP("1.2.3.4")); err != ErrNASNotFound {
		t.Errorf("expected ErrNASNotFound, got %v", err)
	}
}

func TestNtfLogoutRemovesSession(t *testing.T) {
	nas := newFakeNAS(t)
	var notified string
	var mu sync.Mutex
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(ServerConfig{
		BindHost: "127.0.0.1", Timeout: 500 * time.Millisecond,
		SecretFor: func(host string) (string, bool) { return testNASSecret, host == "127.0.0.1" },
		OnNtfLogout: func(userIP, username string) {
			mu.Lock()
			notified = userIP + "/" + username
			mu.Unlock()
		},
	})
	s.conn = conn
	go func() { _ = s.serve(conn) }()
	t.Cleanup(func() { _ = s.Close() })

	userIP := net.ParseIP("172.16.0.7")
	if _, err := s.Challenge(&NAS{Addr: nas.addr()}, "carol", userIP); err != nil {
		t.Fatal(err)
	}
	ntf := NewPacket(TypeNtfLogout, 0, 0, 0, userIP, 0)
	_ = ntf.AddStringAttr(AttrUserName, "carol")
	wire, _ := ntf.EncodeRequest([]byte(testNASSecret))
	if _, err := nas.conn.WriteTo(wire, conn.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return notified == "172.16.0.7/carol"
	})
	if _, ok := s.SessionByIP(userIP.String()); ok {
		t.Error("session not removed after NTF_LOGOUT")
	}
}

func TestRequestTimeout(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(ServerConfig{
		BindHost: "127.0.0.1", Timeout: 100 * time.Millisecond, Retries: 1,
		SecretFor: func(host string) (string, bool) { return testNASSecret, true },
	})
	s.conn = conn
	defer s.Close()
	_, err = s.Challenge(&NAS{Addr: "127.0.0.1:19998"}, "u", net.ParseIP("1.1.1.1"))
	if err != ErrRequestTimeout {
		t.Errorf("expected ErrRequestTimeout, got %v", err)
	}
}
