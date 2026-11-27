package radiusd

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/radiusd/proxy"
	"go.uber.org/zap"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
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

	// PANABIT Stop additionally disconnects the session at the selected
	// upstream. Detect it here (outside the task) so the choice is based on
	// the request attributes, not on any local session state: the accounting
	// ingress already deduplicates packets, so no second dedup layer is needed.
	panabitStop := route.DialVendor == proxy.DialVendorPanabit &&
		rfc2866.AcctStatusType_Get(r.Packet) == rfc2866.AcctStatusType_Value_Stop

	s.submitAcctTask(func() {
		ctx := context.Background()
		pool := s.Proxy.UpstreamsFor(route)
		if len(pool) == 0 {
			return
		}
		// Select once so accounting and the follow-up Disconnect-Request use
		// the same upstream (round_robin/weight would otherwise advance twice).
		up := s.Proxy.Select(route, pool)
		s.Proxy.ForwardAcctTo(ctx, r, route, up, username)
		if panabitStop {
			s.sendPanabitStopDisconnect(ctx, r, up, username)
		}
	}, username)
}

// dialDisconnectTimeout bounds one fire-and-forget Disconnect-Request to an
// upstream AAA. A slow or unreachable upstream must never hold an accounting
// worker or affect the local accounting result.
const dialDisconnectTimeout = 5 * time.Second

// dialDisconnectExchange is overridable in tests; production uses a plain
// radius.Client exchange to the upstream's dynamic-authorization port.
var dialDisconnectExchange = func(ctx context.Context, packet *radius.Packet, addr string) (*radius.Packet, error) {
	client := &radius.Client{Retry: 0, MaxPacketErrors: 1}
	return client.Exchange(ctx, packet, addr)
}

// dialDisconnectAttrTypes is the RFC 5176 §3 identification set copied from
// the original accounting request. Credentials and Message-Authenticator are
// deliberately excluded: only plaintext session identifiers are replayed.
var dialDisconnectAttrTypes = []radius.Type{
	rfc2865.UserName_Type,
	rfc2865.NASIPAddress_Type,
	rfc2865.NASPort_Type,
	rfc2865.FramedIPAddress_Type,
	rfc2865.CalledStationID_Type,
	rfc2865.CallingStationID_Type,
	rfc2865.NASIdentifier_Type,
	rfc2866.AcctSessionID_Type,
	rfc2869.NASPortID_Type,
}

// buildDialDisconnectPacket builds a signed Disconnect-Request for the
// upstream AAA. The upstream shared secret signs the packet and the upstream
// address is the target: it is never used to operate the local NAS.
func buildDialDisconnectPacket(orig *radius.Packet, secret string) (*radius.Packet, error) {
	packet := radius.New(radius.CodeDisconnectRequest, []byte(secret))
	for _, typ := range dialDisconnectAttrTypes {
		if value, ok := orig.Attributes.Lookup(typ); ok && len(value) > 0 {
			packet.Attributes.Set(typ, append(radius.Attribute(nil), value...))
		}
	}
	if err := setMessageAuthenticator(packet, secret); err != nil {
		return nil, fmt.Errorf("dial: sign panabit disconnect: %w", err)
	}
	return packet, nil
}

// sendPanabitStopDisconnect emits a Disconnect-Request to the CoA port of the
// upstream chosen for this accounting packet (default 3799). It runs in a
// detached goroutine: failures are only logged and metered and can never
// block or alter local accounting. Upstream dynauth relay counters are reused
// because there is no dedicated PANABIT counter.
func (s *AcctService) sendPanabitStopDisconnect(ctx context.Context, r *radius.Request, up *domain.RadiusUpstream, username string) {
	packet, err := buildDialDisconnectPacket(r.Packet, up.Secret)
	if err != nil {
		zap.L().Warn("radius dial panabit stop disconnect build failed",
			zap.String("namespace", "proxy"),
			zap.String("upstream", up.Name),
			zap.String("username", username),
			zap.Error(err),
		)
		app.IncRadiusMetric(app.MetricsRadiusProxyDynAuthRelayFail)
		return
	}

	port := up.CoaPort
	if port <= 0 {
		port = DefaultCoAPort
	}
	addr := net.JoinHostPort(up.Addr, strconv.Itoa(port))
	sessionID := rfc2866.AcctSessionID_GetString(r.Packet)

	go func() {
		attemptCtx, cancel := context.WithTimeout(ctx, dialDisconnectTimeout)
		defer cancel()
		resp, err := dialDisconnectExchange(attemptCtx, packet, addr)
		if err != nil {
			zap.L().Warn("radius dial panabit stop disconnect failed",
				zap.String("namespace", "proxy"),
				zap.String("upstream", up.Name),
				zap.String("target", addr),
				zap.String("username", username),
				zap.String("acct_session_id", sessionID),
				zap.Error(err),
			)
			app.IncRadiusMetric(app.MetricsRadiusProxyDynAuthRelayFail)
			return
		}
		if resp == nil || resp.Code != radius.CodeDisconnectACK {
			code := ""
			if resp != nil {
				code = resp.Code.String()
			}
			zap.L().Warn("radius dial panabit stop disconnect not acknowledged",
				zap.String("namespace", "proxy"),
				zap.String("upstream", up.Name),
				zap.String("target", addr),
				zap.String("username", username),
				zap.String("acct_session_id", sessionID),
				zap.String("response", code),
			)
			app.IncRadiusMetric(app.MetricsRadiusProxyDynAuthRelayFail)
			return
		}
		app.IncRadiusMetric(app.MetricsRadiusProxyDynAuthRelay)
	}()
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
