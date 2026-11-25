package portal

import (
	"testing"
)

func TestServiceFlowAndResultMapping(t *testing.T) {
	nas := newFakeNAS(t)
	s := newTestServer(t, nas)
	if err := s.RegisterNAS(&NAS{Addr: nas.addr(), Secret: testNASSecret}); err != nil {
		t.Fatal(err)
	}
	service := NewService(s)

	challenge, err := service.Challenge(ChallengeRequest{
		NASAddr: nas.addr(), Username: "alice@campus", UserIP: "10.9.0.8",
	})
	if err != nil {
		t.Fatalf("Challenge failed: %v", err)
	}
	if challenge.ReqID != nas.reqID || challenge.Serial == 0 {
		t.Errorf("bad challenge result: %+v", challenge)
	}

	auth, err := service.Authenticate(AuthRequest{
		NASAddr: nas.addr(), Username: "alice@campus", Password: "pwd", UserIP: "10.9.0.8",
	})
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}
	if auth.Code != "ACCEPT" || !auth.Authenticated || auth.ErrCode != ErrSuccess {
		t.Errorf("bad auth result: %+v", auth)
	}

	if err := service.Logout(LogoutRequest{NASAddr: nas.addr(), UserIP: "10.9.0.8"}); err != nil {
		t.Fatalf("Logout failed: %v", err)
	}
}

func TestServiceRejectAndValidation(t *testing.T) {
	nas := newFakeNAS(t)
	nas.authResult = ErrReject
	nas.info = "wrong password"
	s := newTestServer(t, nas)
	_ = s.RegisterNAS(&NAS{Addr: nas.addr(), Secret: testNASSecret})
	service := NewService(s)

	if _, err := service.Challenge(ChallengeRequest{NASAddr: nas.addr(), Username: "u", UserIP: "not-an-ip"}); err == nil {
		t.Error("expected error for invalid IP")
	}
	if _, err := service.Authenticate(AuthRequest{NASAddr: nas.addr(), Username: "u", UserIP: "1.2.3.4"}); err == nil {
		t.Error("expected error for missing password")
	}
	if _, err := service.Challenge(ChallengeRequest{NASAddr: nas.addr(), Username: "u", UserIP: "1.2.3.4"}); err != nil {
		t.Fatal(err)
	}
	auth, err := service.Authenticate(AuthRequest{
		NASAddr: nas.addr(), Username: "u", Password: "bad", UserIP: "1.2.3.4", AuthMode: "chap",
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth.Code != "REJECT" || auth.Authenticated || auth.NASInfo != "wrong password" {
		t.Errorf("bad reject result: %+v", auth)
	}
	if _, err := service.Authenticate(AuthRequest{
		NASAddr: nas.addr(), Username: "u", Password: "p", UserIP: "1.2.3.4", AuthMode: "bogus",
	}); err == nil {
		t.Error("expected error for unsupported auth_mode")
	}
}
