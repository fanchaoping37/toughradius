package radiusd

import (
	"context"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"layeh.com/radius"
)

// Acct proxy/replicate integration.
//
// After the local Accounting-Response has been sent to the NAS, two async
// side-channels may run:
//   - route forwarding: when the username matches an enabled realm route with
//     AcctMode both/forward, the Accounting-Request is forwarded upstream
//     (multi-level ISP / roaming billing hand-off).
//   - replication: a verbatim copy goes to every role=replicate upstream.

// acctShouldForward reports whether the route wants this accounting packet
// forwarded upstream. On/Off packets have no username and are never forwarded.
func acctShouldForward(route *domain.RadiusRealmRoute) bool {
	if route == nil {
		return false
	}
	return route.AcctMode == domain.AcctModeForward || route.AcctMode == domain.AcctModeBoth || route.AcctMode == ""
}

// dispatchAcctProxy evaluates forwarding and replication for an accounting
// request. Safe to call synchronously: all upstream I/O happens in the
// submitted task or detached goroutines.
func (s *AcctService) dispatchAcctProxy(r *radius.Request, username string) {
	if s.Proxy == nil {
		return
	}

	// Replication: verbatim copy to every role=replicate upstream.
	s.Proxy.ReplicatePacket(r.Packet, "")

	if username == "" {
		return
	}
	route := s.Proxy.MatchRoute(username)
	if !acctShouldForward(route) {
		return
	}

	s.submitAcctTask(func() {
		s.Proxy.ForwardAcct(context.Background(), r, route, username)
	}, username)
}

// acctProxyRouteRealm resolves the matched realm for logging.
func (s *AcctService) acctProxyRouteRealm(username string) string {
	if s.Proxy == nil || username == "" {
		return ""
	}
	if route := s.Proxy.MatchRoute(username); route != nil {
		return route.Realm
	}
	return ""
}
