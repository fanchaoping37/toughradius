package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"fmt"
	"time"

	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"go.uber.org/zap"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

// forwardResult reports the outcome of an upstream authentication exchange.
type forwardResult struct {
	// Response is the upstream reply packet; nil on transport failure/timeout.
	Response *radius.Packet
	// Upstream is the upstream the request was sent to.
	Upstream *domain.RadiusUpstream
	// Err is the transport/build error, if any.
	Err error
}

// ForwardAuth forwards an Access-Request to the route's upstream pool and
// returns the upstream Access-Accept/Access-Reject. The caller is responsible
// for relaying the response back to the NAS re-signed with the NAS secret.
//
// Credential transformation applies to PAP requests only:
//   - User-Name is rewritten as UserPrefix + local-part + UserSuffix
//   - when MappedPassword is set, the PAP User-Password is replaced
//
// CHAP/MSCHAPv2 are challenge-based: the password cannot be rewritten, so the
// original credentials pass through unchanged (username rewrite still applies).
func (s *Service) ForwardAuth(ctx context.Context, orig *radius.Request, route *domain.RadiusRealmRoute, username string) *forwardResult {
	pool := s.UpstreamsFor(route)
	if len(pool) == 0 {
		return &forwardResult{Err: ErrNoUpstream}
	}
	up := s.Select(route, pool)
	if up == nil {
		return &forwardResult{Err: ErrNoUpstream}
	}

	packet, err := buildUpstreamPacket(orig.Packet, up, route, username)
	if err != nil {
		return &forwardResult{Err: err, Upstream: up}
	}
	if hasEAPMessage(packet) {
		return &forwardResult{Err: ErrEAPNotSupported, Upstream: up}
	}

	resp, err := s.send(ctx, packet, fmt.Sprintf("%s:%d", up.Addr, authPortOf(up)))
	if err != nil {
		app.IncRadiusMetric(app.MetricsRadiusProxyAuthFail)
		zap.L().Warn("radius proxy auth forward failed",
			zap.String("namespace", "proxy"),
			zap.String("upstream", up.Name),
			zap.String("username", username),
			zap.String("realm", route.Realm),
			zap.Error(err),
		)
		return &forwardResult{Err: err, Upstream: up}
	}

	app.IncRadiusMetric(app.MetricsRadiusProxyAuthForward)
	zap.L().Info("radius proxy auth forwarded",
		zap.String("namespace", "proxy"),
		zap.String("upstream", up.Name),
		zap.String("username", username),
		zap.String("realm", route.Realm),
		zap.String("response", resp.Code.String()),
	)
	return &forwardResult{Response: resp, Upstream: up}
}

// ForwardAcct forwards an Accounting-Request copy to the route's upstream
// pool and waits for the Accounting-Response. Failures are logged and
// counted but never bubble up: local accounting must not be disrupted by an
// unreachable upstream.
func (s *Service) ForwardAcct(ctx context.Context, orig *radius.Request, route *domain.RadiusRealmRoute, username string) {
	pool := s.UpstreamsFor(route)
	if len(pool) == 0 {
		return
	}
	s.ForwardAcctTo(ctx, orig, route, s.Select(route, pool), username)
}

// ForwardAcctTo forwards an Accounting-Request to a caller-selected upstream.
// Callers that need to correlate a follow-up action (for example a PANABIT
// Stop Disconnect-Request) with the exact upstream used for accounting should
// select the upstream once via UpstreamsFor/Select and use this method, instead
// of letting ForwardAcct select a second (possibly different) pool member.
func (s *Service) ForwardAcctTo(ctx context.Context, orig *radius.Request, route *domain.RadiusRealmRoute, up *domain.RadiusUpstream, username string) {
	if up == nil {
		return
	}

	packet, err := buildUpstreamPacket(orig.Packet, up, route, username)
	if err != nil || hasEAPMessage(packet) {
		app.IncRadiusMetric(app.MetricsRadiusProxyAcctFail)
		return
	}

	if _, err := s.send(ctx, packet, fmt.Sprintf("%s:%d", up.Addr, acctPortOf(up))); err != nil {
		app.IncRadiusMetric(app.MetricsRadiusProxyAcctFail)
		zap.L().Warn("radius proxy acct forward failed",
			zap.String("namespace", "proxy"),
			zap.String("upstream", up.Name),
			zap.String("username", username),
			zap.Error(err),
		)
		return
	}
	app.IncRadiusMetric(app.MetricsRadiusProxyAcctForward)
	zap.L().Info("radius proxy acct forwarded",
		zap.String("namespace", "proxy"),
		zap.String("upstream", up.Name),
		zap.String("username", username),
	)
}

// ReplicatePacket sends a fire-and-forget copy of a request packet to every
// role=replicate upstream. The copy carries the profile/user-group Class
// enrichment when enrichClass is non-empty, so downstream servers receive
// user plan/group information alongside the original attributes.
func (s *Service) ReplicatePacket(orig *radius.Packet, enrichClass string) {
	targets := s.ReplicateTargets()
	if len(targets) == 0 {
		return
	}
	for _, up := range targets {
		packet, err := buildReplicaPacket(orig, up, enrichClass)
		if err != nil {
			app.IncRadiusMetric(app.MetricsRadiusProxyReplicateFail)
			continue
		}
		target := fmt.Sprintf("%s:%d", up.Addr, authPortOf(up))
		go func(p *radius.Packet, name, addr string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := s.send(ctx, p, addr); err != nil {
				app.IncRadiusMetric(app.MetricsRadiusProxyReplicateFail)
				zap.L().Warn("radius replicate send failed",
					zap.String("namespace", "proxy"),
					zap.String("upstream", name),
					zap.Error(err),
				)
				return
			}
			app.IncRadiusMetric(app.MetricsRadiusProxyReplicate)
		}(packet, up.Name, target)
	}
}

// send performs one upstream exchange using radius.Client. The client
// retransmits once per second while the context deadline bounds the whole
// exchange, and verifies the upstream Response Authenticator.
func (s *Service) send(ctx context.Context, packet *radius.Packet, addr string) (*radius.Packet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.exchange != nil {
		attemptCtx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		return s.exchange(attemptCtx, packet, addr)
	}
	client := &radius.Client{Retry: time.Second, MaxPacketErrors: 5}
	attemptCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return client.Exchange(attemptCtx, packet, addr)
}

// buildUpstreamPacket clones a request packet for an upstream, applying the
// shared secret, credential transformation and Message-Authenticator.
func buildUpstreamPacket(orig *radius.Packet, up *domain.RadiusUpstream, route *domain.RadiusRealmRoute, username string) (*radius.Packet, error) {
	secret := []byte(up.Secret)
	packet := radius.New(orig.Code, secret)

	// Copy every attribute verbatim (including VSAs), then rewrite identity.
	for _, attr := range orig.Attributes {
		if attr.Type == rfc2865.UserPassword_Type ||
			attr.Type == rfc2869.MessageAuthenticator_Type ||
			attr.Type == rfc2865.UserName_Type {
			continue // rewritten / re-signed below
		}
		packet.Add(attr.Type, attr.Attribute)
	}

	localPart, _ := SplitUsername(username)
	rewritten := route.UserPrefix + localPart + route.UserSuffix
	if rewritten == "" {
		rewritten = username
	}
	if err := rfc2865.UserName_SetString(packet, rewritten); err != nil {
		return nil, fmt.Errorf("proxy: set user-name: %w", err)
	}

	// PAP credential transformation: only when the original packet carried a
	// User-Password attribute (CHAP/MSCHAPv2 cannot rewrite passwords). The
	// plaintext is recovered with the NAS secret carried on the request packet.
	// Encryption uses the packet's random Access-Request authenticator, which
	// is kept as the wire authenticator.
	if UserPassword_Has(orig) {
		pwd := route.MappedPassword
		if pwd == "" {
			pwd = rfc2865.UserPassword_GetString(orig)
		}
		if err := rfc2865.UserPassword_SetString(packet, pwd); err != nil {
			return nil, fmt.Errorf("proxy: set user-password: %w", err)
		}
	}

	// Dial-proxy vendor adaptation (Filter-Id defaults, DRCOM attribute
	// stripping, validated raw VSA injection). It must run before the upstream
	// Request Authenticator and Message-Authenticator are computed below.
	if err := ApplyDialProxyProfile(packet, orig, route); err != nil {
		return nil, fmt.Errorf("proxy: apply dial proxy profile: %w", err)
	}

	// Accounting-Request carries a keyed Request Authenticator
	// (RFC 2866 §3): MD5 over the packet with the authenticator zeroed,
	// computed with the upstream secret. It must be recomputed because both
	// the secret and the attributes (rewritten User-Name) changed.
	if orig.Code == radius.CodeAccountingRequest {
		if err := recomputeAcctAuthenticator(packet); err != nil {
			return nil, err
		}
	}

	if err := signMessageAuthenticator(packet, secret); err != nil {
		return nil, err
	}
	return packet, nil
}

// recomputeAcctAuthenticator derives the RFC 2866 Request Authenticator of an
// Accounting-Request for the upstream shared secret.
func recomputeAcctAuthenticator(packet *radius.Packet) error {
	saved := packet.Authenticator
	packet.Authenticator = [16]byte{}
	wire, err := packet.MarshalBinary()
	if err != nil {
		packet.Authenticator = saved
		return fmt.Errorf("proxy: marshal acct request: %w", err)
	}
	hash := md5.New()
	hash.Write(wire)
	hash.Write(packet.Secret)
	copy(packet.Authenticator[:], hash.Sum(nil))
	return nil
}

// buildReplicaPacket clones a request packet for a replicate target: verbatim
// attributes, optionally an appended Class attribute carrying user group /
// profile information, signed for the replica's shared secret.
func buildReplicaPacket(orig *radius.Packet, up *domain.RadiusUpstream, enrichClass string) (*radius.Packet, error) {
	secret := []byte(up.Secret)
	packet := radius.New(orig.Code, secret)
	for _, attr := range orig.Attributes {
		if attr.Type == rfc2869.MessageAuthenticator_Type {
			continue // re-signed below
		}
		packet.Add(attr.Type, attr.Attribute)
	}
	if enrichClass != "" {
		if err := rfc2865.Class_SetString(packet, enrichClass); err != nil {
			return nil, fmt.Errorf("proxy: set class: %w", err)
		}
	}
	if orig.Code == radius.CodeAccountingRequest {
		if err := recomputeAcctAuthenticator(packet); err != nil {
			return nil, err
		}
	}
	if err := signMessageAuthenticator(packet, secret); err != nil {
		return nil, err
	}
	return packet, nil
}

// UserPassword_Has reports whether the packet carries a PAP User-Password.
func UserPassword_Has(p *radius.Packet) bool {
	if p == nil {
		return false
	}
	for _, attr := range p.Attributes {
		if attr.Type == rfc2865.UserPassword_Type {
			return true
		}
	}
	return false
}

// signMessageAuthenticator computes and inserts the RFC 3579 §3.2
// Message-Authenticator (HMAC-MD5 over the packet with the attribute zeroed
// and the authenticator as it appears on the wire), keyed with the upstream
// shared secret. Callers must have finalized the authenticator beforehand.
func signMessageAuthenticator(packet *radius.Packet, secret []byte) error {
	if err := rfc2869.MessageAuthenticator_Set(packet, make([]byte, 16)); err != nil {
		return fmt.Errorf("proxy: set message-authenticator placeholder: %w", err)
	}
	wire, err := packet.MarshalBinary()
	if err != nil {
		return fmt.Errorf("proxy: marshal for message-authenticator: %w", err)
	}
	mac := hmac.New(md5.New, secret)
	mac.Write(wire)
	if err := rfc2869.MessageAuthenticator_Set(packet, mac.Sum(nil)); err != nil {
		return fmt.Errorf("proxy: set message-authenticator: %w", err)
	}
	return nil
}

// hasEAPMessage reports whether the packet carries an EAP-Message attribute
// (RFC 3579); EAP proxying requires stateful hop handling and is out of scope.
func hasEAPMessage(p *radius.Packet) bool {
	if p == nil {
		return false
	}
	for _, attr := range p.Attributes {
		if attr.Type == rfc2869.EAPMessage_Type {
			return true
		}
	}
	return false
}

func authPortOf(up *domain.RadiusUpstream) int {
	if up.AuthPort <= 0 {
		return 1812
	}
	return up.AuthPort
}

func acctPortOf(up *domain.RadiusUpstream) int {
	if up.AcctPort <= 0 {
		return 1813
	}
	return up.AcctPort
}
