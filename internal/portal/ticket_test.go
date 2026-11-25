package portal

import (
	"strings"
	"testing"
	"time"
)

func TestTicketManagerIssuesAndConsumesOnce(t *testing.T) {
	m := NewTicketManager("server-secret", time.Minute)
	token, err := m.Issue("alice@campus", "10.9.0.8", "aa-bb-cc", 12)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := m.Verify(token, "alice@campus", "10.9.0.8", "aa-bb-cc")
	if err != nil || serial != 12 {
		t.Fatalf("verify: serial=%d err=%v", serial, err)
	}
	if _, err := m.Verify(token, "alice@campus", "10.9.0.8", "aa-bb-cc"); err == nil {
		t.Fatal("replayed ticket must be rejected")
	}
}

func TestTicketManagerRejectsTamperingAndContextMismatch(t *testing.T) {
	m := NewTicketManager("server-secret", time.Minute)
	token, _ := m.Issue("alice", "10.9.0.8", "mac-1", 1)
	parts := strings.Split(token, ".")
	tampered := parts[0] + "." + strings.Repeat("0", len(parts[1]))
	if _, err := m.Verify(tampered, "alice", "10.9.0.8", "mac-1"); err == nil {
		t.Fatal("tampered signature accepted")
	}
	if _, err := m.Verify(token, "alice", "10.9.0.9", "mac-1"); err == nil {
		t.Fatal("ticket bound to a different IP accepted")
	}
	if _, err := m.Verify(token, "alice", "10.9.0.8", "mac-2"); err == nil {
		t.Fatal("ticket bound to a different MAC accepted")
	}
}

func TestTicketManagerExpires(t *testing.T) {
	m := NewTicketManager("server-secret", time.Minute)
	m.ttl = time.Nanosecond
	token, _ := m.Issue("alice", "10.9.0.8", "", 1)
	time.Sleep(2 * time.Millisecond)
	if _, err := m.Verify(token, "alice", "10.9.0.8", ""); err == nil {
		t.Fatal("expired ticket accepted")
	}
}

func TestServiceRequiresOneTimeTicket(t *testing.T) {
	nas := newFakeNAS(t)
	s := newTestServer(t, nas)
	if err := s.RegisterNAS(&NAS{Addr: nas.addr(), Secret: testNASSecret}); err != nil {
		t.Fatal(err)
	}
	service := NewService(s)
	service.Tickets = NewTicketManager("server-secret", time.Minute)
	service.RequireTicket = true

	if _, err := service.Authenticate(AuthRequest{
		NASAddr: nas.addr(), Username: "alice", Password: "pwd", UserIP: "10.9.0.8",
	}); err == nil {
		t.Fatal("auth without auth_ticket must be rejected")
	}

	challenge, err := service.Challenge(ChallengeRequest{
		NASAddr: nas.addr(), Username: "alice", UserIP: "10.9.0.8", MAC: "mac-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Ticket == "" {
		t.Fatal("challenge must issue an auth_ticket")
	}
	authReq := AuthRequest{
		NASAddr: nas.addr(), Username: "alice", Password: "pwd",
		UserIP: "10.9.0.8", MAC: "mac-1", Ticket: challenge.Ticket,
	}
	if _, err := service.Authenticate(authReq); err != nil {
		t.Fatalf("auth with ticket failed: %v", err)
	}
	if _, err := service.Authenticate(authReq); err == nil {
		t.Fatal("replayed auth_ticket must be rejected")
	}
}
