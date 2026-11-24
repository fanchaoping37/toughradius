package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

func TestSplitUsername(t *testing.T) {
	cases := []struct {
		in, user, realm string
	}{
		{"alice", "alice", ""},
		{"alice@isp.com", "alice", "isp.com"},
		{"a@b@c", "a@b", "c"},
		{"@realm", "", "realm"},
	}
	for _, c := range cases {
		u, r := SplitUsername(c.in)
		if u != c.user || r != c.realm {
			t.Errorf("SplitUsername(%q) = (%q,%q), want (%q,%q)", c.in, u, r, c.user, c.realm)
		}
	}
}

func TestParseIDs(t *testing.T) {
	ids := ParseIDs(" 1, 2,x,3,0,-4,")
	want := []int64{1, 2, 3}
	if len(ids) != len(want) {
		t.Fatalf("ParseIDs = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ParseIDs = %v, want %v", ids, want)
		}
	}
}

func newTestService() *Service {
	return NewService(nil, 0, 0)
}

func TestMatchRouteRealm(t *testing.T) {
	s := newTestService()
	s.mu.Lock()
	s.routes = []*domain.RadiusRealmRoute{
		{Realm: "isp1.com", Status: "enabled", UpstreamIds: "1"},
		{Realm: "*", Status: "enabled", UpstreamIds: "2"},
	}
	s.upstreams = map[int64]*domain.RadiusUpstream{}
	s.loadedAt = timeNow()
	s.mu.Unlock()

	if r := s.MatchRoute("alice@ISP1.COM"); r == nil || r.Realm != "isp1.com" {
		t.Errorf("expected case-insensitive realm match, got %+v", r)
	}
	if r := s.MatchRoute("alice"); r == nil || r.Realm != "*" {
		t.Errorf("expected catch-all route, got %+v", r)
	}
	s.mu.Lock()
	s.routes = s.routes[:1] // drop catch-all
	s.mu.Unlock()
	if r := s.MatchRoute("alice"); r != nil {
		t.Errorf("expected nil route for realm-less username without catch-all, got %+v", r)
	}
}

func TestSelectStrategies(t *testing.T) {
	s := newTestService()
	pool := []*domain.RadiusUpstream{
		{ID: 1, Name: "a"},
		{ID: 2, Name: "b"},
		{ID: 3, Name: "c"},
	}
	route := &domain.RadiusRealmRoute{Strategy: domain.RealmStrategySingle}
	if up := s.Select(route, pool); up.ID != 1 {
		t.Errorf("single strategy should pick first upstream, got %d", up.ID)
	}
	route.Strategy = domain.RealmStrategyRoundRobin
	seen := map[int64]bool{}
	for i := 0; i < 3; i++ {
		seen[s.Select(route, pool).ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("round robin should rotate across all upstreams, saw %v", seen)
	}
	// Weight: upstream a has weight 3 → appears 3 times per cycle of 5.
	poolW := []*domain.RadiusUpstream{{ID: 1, Weight: 3}, {ID: 2, Weight: 2}}
	route.Strategy = domain.RealmStrategyWeight
	counts := map[int64]int{}
	for i := 0; i < 5; i++ {
		counts[s.Select(route, poolW).ID]++
	}
	if counts[1] != 3 || counts[2] != 2 {
		t.Errorf("weighted distribution = %v, want {1:3 2:2}", counts)
	}
}

func TestUpstreamsForFiltersRoleAndEnabled(t *testing.T) {
	s := newTestService()
	s.mu.Lock()
	s.upstreams = map[int64]*domain.RadiusUpstream{
		1: {ID: 1, Role: domain.UpstreamRoleProxy, Status: "enabled"},
		2: {ID: 2, Role: domain.UpstreamRoleReplicate, Status: "enabled"},
		3: {ID: 3, Role: domain.UpstreamRoleProxy, Status: "disabled"},
	}
	s.loadedAt = timeNow()
	s.mu.Unlock()
	route := &domain.RadiusRealmRoute{UpstreamIds: "1,2,3"}
	pool := s.UpstreamsFor(route)
	if len(pool) != 1 || pool[0].ID != 1 {
		t.Errorf("expected only enabled proxy upstream 1, got %+v", pool)
	}
}

func TestForwardAuthCredentialTransform(t *testing.T) {
	var captured *radius.Packet
	var capturedAddr string
	s := newTestService()
	s.exchange = func(ctx context.Context, p *radius.Packet, addr string) (*radius.Packet, error) {
		captured = p
		capturedAddr = addr
		resp := radius.New(radius.CodeAccessAccept, []byte("upsecret"))
		return resp, nil
	}
	s.mu.Lock()
	s.upstreams = map[int64]*domain.RadiusUpstream{
		7: {ID: 7, Name: "carrier", Addr: "10.0.0.1", AuthPort: 1812, AcctPort: 1813, Secret: "upsecret", Role: domain.UpstreamRoleProxy, Status: "enabled"},
	}
	s.routes = []*domain.RadiusRealmRoute{
		{Realm: "campus.edu", Strategy: "single", UpstreamIds: "7", UserPrefix: "isp_", MappedPassword: "carrierpwd", Status: "enabled"},
	}
	s.loadedAt = timeNow()
	s.mu.Unlock()

	orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
	orig.Secret = []byte("nassecret")
	_ = rfc2865.UserName_SetString(orig, "alice@campus.edu")
	_ = rfc2865.UserPassword_SetString(orig, "campuspwd")

	route := s.MatchRoute("alice@campus.edu")
	res := s.ForwardAuth(context.Background(), &radius.Request{Packet: orig}, route, "alice@campus.edu")
	if res.Err != nil {
		t.Fatalf("ForwardAuth error: %v", res.Err)
	}
	if res.Response.Code != radius.CodeAccessAccept {
		t.Errorf("expected Access-Accept, got %v", res.Response.Code)
	}
	if capturedAddr != "10.0.0.1:1812" {
		t.Errorf("upstream addr = %q, want 10.0.0.1:1812", capturedAddr)
	}
	uname, _ := rfc2865.UserName_LookupString(captured)
	if uname != "isp_alice" {
		t.Errorf("rewritten username = %q, want isp_alice", uname)
	}
	pwd := rfc2865.UserPassword_GetString(captured)
	if pwd != "carrierpwd" {
		t.Errorf("mapped password = %q, want carrierpwd", pwd)
	}
}

func TestForwardAuthEAPNotForwarded(t *testing.T) {
	s := newTestService()
	s.mu.Lock()
	s.upstreams = map[int64]*domain.RadiusUpstream{
		7: {ID: 7, Secret: "upsecret", Role: domain.UpstreamRoleProxy, Status: "enabled"},
	}
	s.routes = []*domain.RadiusRealmRoute{{Realm: "*", UpstreamIds: "7", Status: "enabled"}}
	s.loadedAt = timeNow()
	s.mu.Unlock()

	orig := radius.New(radius.CodeAccessRequest, []byte("nassecret"))
	orig.Secret = []byte("nassecret")
	_ = rfc2865.UserName_SetString(orig, "alice")
	_ = rfc2869.EAPMessage_Set(orig, []byte{0x02, 0x00, 0x00, 0x04})

	route := s.MatchRoute("alice")
	res := s.ForwardAuth(context.Background(), &radius.Request{Packet: orig}, route, "alice")
	if !errors.Is(res.Err, ErrEAPNotSupported) {
		t.Errorf("expected ErrEAPNotSupported, got %v", res.Err)
	}
}

func TestForwardAuthNoUpstream(t *testing.T) {
	s := newTestService()
	route := &domain.RadiusRealmRoute{UpstreamIds: "99", Status: "enabled"}
	orig := radius.New(radius.CodeAccessRequest, []byte("s"))
	_ = rfc2865.UserName_SetString(orig, "bob")
	res := s.ForwardAuth(context.Background(), &radius.Request{Packet: orig}, route, "bob")
	if !errors.Is(res.Err, ErrNoUpstream) {
		t.Errorf("expected ErrNoUpstream, got %v", res.Err)
	}
}

// timeNow returns a fresh timestamp to mark cache entries as loaded.
func timeNow() time.Time { return time.Now() }
