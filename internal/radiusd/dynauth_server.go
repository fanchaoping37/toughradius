package radiusd

import (
	"context"
	"net"
	"strconv"

	"github.com/talkincode/toughradius/v9/internal/app"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
	"layeh.com/radius/rfc3576"
)

// DynAuthService implements the RFC 5176 Dynamic Authorization Server (DAS)
// role: it accepts CoA-Request / Disconnect-Request packets from upstream
// RADIUS servers (multi-level ISP topology) and relays the operation to the
// owning NAS through CoAService.
//
// Upstream identity resolution: the packet's source IP is matched against
// enabled role=proxy upstreams; the fallback secret comes from the
// TOUGHRADIUS_RADIUS_DYNAUTH_SECRET setting. Packets from unknown clients are
// silently dropped (RFC 5176 does not define a reply to untrusted clients,
// and answering would leak service existence).
type DynAuthService struct {
	*RadiusService
	coa *CoAService
}

func NewDynAuthService(radiusService *RadiusService) *DynAuthService {
	return &DynAuthService{
		RadiusService: radiusService,
		coa:           NewCoAService(radiusService),
	}
}

// dynAuthServer binds the UDP listener and serves packets until shutdown.
type dynAuthServer struct {
	conn    net.PacketConn
	service *DynAuthService
}

// ListenDynAuthServer binds the dynamic-authorization UDP listener. It is a
// no-op when DynAuthEnabled is false or the radius service is disabled.
func ListenDynAuthServer(appCtx app.AppContext, service *DynAuthService) error {
	cfg := appCtx.Config()
	if !cfg.Radiusd.Enabled || !cfg.Radiusd.DynAuthEnabled {
		return nil
	}
	port := cfg.Radiusd.DynAuthPort
	if port <= 0 {
		port = DefaultCoAPort
	}
	addr := net.JoinHostPort(cfg.Radiusd.Host, strconv.Itoa(port))

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	zap.S().Infof("Starting Radius Dynamic Authorization (CoA/DM) server on %s", addr)

	server := &dynAuthServer{conn: conn, service: service}
	buffer := make([]byte, 4096)
	for {
		n, remote, err := conn.ReadFrom(buffer)
		if err != nil {
			zap.L().Error("dynauth read error", zap.Error(err))
			return err
		}
		server.handlePacket(buffer[:n], remote)
	}
}

// handlePacket processes one inbound CoA/Disconnect request.
func (s *dynAuthServer) handlePacket(data []byte, remote net.Addr) {
	secret, ok := s.service.resolveSecret(remoteAddrHost(remote))
	if !ok {
		zap.L().Warn("dynauth request from unknown client dropped",
			zap.String("namespace", "dynauth"),
			zap.String("remote", remoteAddrHost(remote)),
		)
		return
	}

	packet, err := radius.Parse(data, secret)
	if err != nil {
		zap.L().Warn("dynauth packet parse failed",
			zap.String("namespace", "dynauth"),
			zap.String("remote", remoteAddrHost(remote)),
			zap.Error(err),
		)
		return
	}

	if result := s.service.verifyMessageAuthenticator(packet, secret); result != msgAuthValid {
		// RFC 5176 §3.4: a request with an invalid Message-Authenticator MUST
		// be silently discarded. An absent attribute is also rejected here so
		// only signed requests reach the session relay.
		zap.L().Warn("dynauth packet message-authenticator invalid, dropped",
			zap.String("namespace", "dynauth"),
			zap.String("remote", remoteAddrHost(remote)),
			zap.Int("result", int(result)),
		)
		return
	}

	switch packet.Code {
	case radius.CodeDisconnectRequest:
		s.handleDisconnect(packet, secret, remote)
	case radius.CodeCoARequest:
		s.handleCoA(packet, secret, remote)
	default:
		zap.L().Warn("dynauth unsupported packet code dropped",
			zap.String("namespace", "dynauth"),
			zap.String("remote", remoteAddrHost(remote)),
			zap.String("code", packet.Code.String()),
		)
	}
}

// handleDisconnect resolves the targeted session and relays a
// Disconnect-Request to its NAS, replying ACK/NAK to the upstream.
func (s *dynAuthServer) handleDisconnect(packet *radius.Packet, secret []byte, remote net.Addr) {
	target, id, nakCause, ok := s.service.resolveTarget(packet)
	if !ok {
		s.reply(packet, secret, radius.CodeDisconnectNAK, nakCause, remote)
		return
	}
	result, err := s.service.coa.Disconnect(context.Background(), target, id)
	s.relayResult(packet, secret, radius.CodeDisconnectACK, radius.CodeDisconnectNAK, result, err, remote)
}

// handleCoA resolves the targeted session and relays a CoA-Request with the
// authorization-change attributes (Session-Timeout, Filter-Id) to its NAS.
func (s *dynAuthServer) handleCoA(packet *radius.Packet, secret []byte, remote net.Addr) {
	target, id, nakCause, ok := s.service.resolveTarget(packet)
	if !ok {
		s.reply(packet, secret, radius.CodeCoANAK, nakCause, remote)
		return
	}

	var changes []AttributeSetter
	if st := rfc2865.SessionTimeout_Get(packet); st > 0 {
		changes = append(changes, WithSessionTimeout(uint32(st)))
	}
	if fid := rfc2865.FilterID_GetString(packet); fid != "" {
		changes = append(changes, WithFilterID(fid))
	}

	result, err := s.service.coa.CoA(context.Background(), target, id, changes...)
	s.relayResult(packet, secret, radius.CodeCoAACK, radius.CodeCoANAK, result, err, remote)
}

// resolveTarget extracts the session identity from the request, finds the
// matching online session and NAS. When the session cannot be resolved it
// returns the RFC 5176 Error-Cause for the NAK reply.
func (s *DynAuthService) resolveTarget(packet *radius.Packet) (CoATarget, SessionIdentity, rfc3576.ErrorCause, bool) {
	acctSessionID := rfc2866.AcctSessionID_GetString(packet)
	if acctSessionID == "" {
		return CoATarget{}, SessionIdentity{}, rfc3576.ErrorCause_Value_SessionContextNotFound, false
	}

	online, err := s.SessionRepo.GetBySessionId(context.Background(), acctSessionID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return CoATarget{}, SessionIdentity{}, rfc3576.ErrorCause_Value_SessionContextNotFound, false
		}
		return CoATarget{}, SessionIdentity{}, rfc3576.ErrorCause_Value_ResourcesUnavailable, false
	}

	nas, err := s.GetNas(online.NasAddr, online.NasId)
	if err != nil {
		return CoATarget{}, SessionIdentity{}, rfc3576.ErrorCause_Value_SessionContextNotFound, false
	}

	return CoATargetFromNas(nas), SessionIdentityFromOnline(online), 0, true
}

// relayResult maps the NAS-side CoA exchange outcome onto the upstream reply.
func (s *dynAuthServer) relayResult(packet *radius.Packet, secret []byte, ack, nak radius.Code, result *CoAResult, err error, remote net.Addr) {
	if err != nil || (result != nil && !result.Success) {
		app.IncRadiusMetric(app.MetricsRadiusProxyDynAuthRelayFail)
		s.reply(packet, secret, nak, 0, remote)
		return
	}
	app.IncRadiusMetric(app.MetricsRadiusProxyDynAuthRelay)
	s.reply(packet, secret, ack, 0, remote)
}

// reply sends an ACK/NAK response to the dynamic-authorization client. The
// Response Authenticator is computed by packet.Encode (RFC 5176 §3.3) and the
// reply is signed with a Message-Authenticator per RFC 5176 §3.4.
func (s *dynAuthServer) reply(packet *radius.Packet, secret []byte, code radius.Code, cause rfc3576.ErrorCause, remote net.Addr) {
	resp := packet.Response(code)
	if cause != 0 {
		_ = rfc3576.ErrorCause_Set(resp, cause) //nolint:errcheck
	}
	if err := rfc2869.MessageAuthenticator_Set(resp, make([]byte, 16)); err == nil {
		if wire, marshalErr := resp.MarshalBinary(); marshalErr == nil {
			_ = rfc2869.MessageAuthenticator_Set(resp, computeMessageAuthenticator(wire, secret)) //nolint:errcheck
		}
	}
	wire, err := resp.Encode()
	if err != nil {
		zap.L().Error("dynauth reply encode error", zap.Error(err))
		return
	}
	if _, err := s.conn.WriteTo(wire, remote); err != nil {
		zap.L().Error("dynauth reply write error", zap.Error(err))
	}
}

// resolveSecret maps the client source IP onto a shared secret: the matching
// enabled proxy upstream's secret first, then the configured fallback secret.
func (s *DynAuthService) resolveSecret(host string) ([]byte, bool) {
	var upstream domain.RadiusUpstream
	err := s.AppContext().DB().Where("role = ? AND status = ? AND addr = ?",
		domain.UpstreamRoleProxy, "enabled", host).First(&upstream).Error
	if err == nil && upstream.Secret != "" {
		return []byte(upstream.Secret), true
	}

	fallback := s.Config().Radiusd.DynAuthSecret
	if fallback != "" {
		return []byte(fallback), true
	}
	return nil, false
}

func remoteAddrHost(addr net.Addr) string {
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}
