package radiusd

import (
	"fmt"

	"github.com/talkincode/toughradius/v9/internal/app"
	radiuserrors "github.com/talkincode/toughradius/v9/internal/radiusd/errors"
	"github.com/talkincode/toughradius/v9/internal/radiusd/proxy"
	"go.uber.org/zap"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

// StageProxyDispatch proxies non-EAP Access-Requests to an upstream server
// according to the realm route table. It is registered between the rate limit
// stage and vendor parsing: Message-Authenticator validation and brute-force
// throttling still apply, but local user loading is skipped because the
// credentials are evaluated by the upstream.
//
// EAP requests are not forwarded (stateful hop-by-hop handling is out of
// scope) and fall through to the local EAP pipeline.

func (s *AuthService) stageProxyDispatch(ctx *AuthPipelineContext) error {
	if ctx.IsEAP || s.Proxy == nil || !s.Proxy.Enabled() {
		return nil
	}

	route := s.Proxy.MatchRoute(ctx.Username)
	if route == nil {
		return nil
	}

	result := s.Proxy.ForwardAuth(ctx.Context, ctx.Request, route, ctx.Username)
	if result.Err != nil {
		if result.Err == proxy.ErrEAPNotSupported {
			// Fall through to the local pipeline (e.g. EAP inside a
			// realm-matched username stays local).
			return nil
		}
		return radiuserrors.NewAuthErrorWithStage(
			app.MetricsRadiusRejectOther,
			fmt.Sprintf("radius proxy upstream unavailable: %v", result.Err),
			StageProxyDispatch,
		)
	}

	switch result.Response.Code {
	case radius.CodeAccessAccept:
		s.sendProxiedAccept(ctx, result.Response)
	case radius.CodeAccessReject:
		s.sendProxiedReject(ctx, result.Response)
	default:
		// Challenge-style replies are not relayed; treat as reject.
		s.sendProxiedReject(ctx, nil)
	}
	ctx.Stop()
	return nil
}

// sendProxiedAccept relays an upstream Access-Accept to the NAS. Upstream
// reply attributes are copied (minus the upstream Message-Authenticator) and
// the response is re-signed with the NAS shared secret.
func (s *AuthService) sendProxiedAccept(ctx *AuthPipelineContext, upstream *radius.Packet) {
	resp := ctx.Request.Response(radius.CodeAccessAccept)
	for _, attr := range upstream.Attributes {
		if attr.Type == rfc2869.MessageAuthenticator_Type {
			continue
		}
		resp.Add(attr.Type, attr.Attribute)
	}
	s.addResponseMessageAuthenticator(resp, ctx.NAS.Secret)
	s.SendAccept(ctx.Writer, ctx.Request, resp)

	app.IncRadiusMetric(app.MetricsRadiusAccept)
	zap.L().Info("radius proxy auth success",
		zap.String("namespace", "proxy"),
		zap.String("username", ctx.Username),
		zap.String("nasip", ctx.RemoteIP),
		zap.String("realm", s.ProxyRouteRealm(ctx.Username)),
	)
}

// sendProxiedReject relays an upstream Access-Reject to the NAS, preserving
// the upstream Reply-Message when present.
func (s *AuthService) sendProxiedReject(ctx *AuthPipelineContext, upstream *radius.Packet) {
	resp := ctx.Request.Response(radius.CodeAccessReject)
	if upstream != nil {
		if msg := rfc2865.ReplyMessage_GetString(upstream); msg != "" {
			_ = rfc2865.ReplyMessage_SetString(resp, msg) //nolint:errcheck
		}
	}
	s.addResponseMessageAuthenticator(resp, ctx.NAS.Secret)
	if err := ctx.Writer.Write(resp); err != nil {
		zap.L().Error("radius proxy write reject error",
			zap.String("namespace", "proxy"),
			zap.Error(err),
		)
	}

	app.IncRadiusMetric(app.MetricsRadiusRejectPasswdError)
	zap.L().Info("radius proxy auth rejected by upstream",
		zap.String("namespace", "proxy"),
		zap.String("username", ctx.Username),
		zap.String("nasip", ctx.RemoteIP),
	)
}

// ProxyRouteRealm resolves the realm label of the route matching username,
// used for logging only.
func (s *AuthService) ProxyRouteRealm(username string) string {
	if s.Proxy == nil {
		return ""
	}
	if route := s.Proxy.MatchRoute(username); route != nil {
		return route.Realm
	}
	return ""
}

// replicateAuthRequest sends a fire-and-forget copy of an Access-Request to
// every role=replicate upstream. When the local user is known the Class
// attribute is enriched with the profile (user group / plan) name so
// downstream servers receive user-plan information alongside the packet.
// Called from ServeRADIUS after pipeline execution for requests that passed
// NAS validation.
func (s *AuthService) replicateAuthRequest(ctx *AuthPipelineContext) {
	if s.Proxy == nil || ctx.NAS == nil || ctx.IsEAP {
		return
	}
	enrich := ""
	if ctx.User != nil && s.AppContext() != nil {
		if profile, err := s.AppContext().ProfileCache().Get(ctx.User.ProfileId); err == nil && profile != nil {
			enrich = fmt.Sprintf("profile=%s", profile.Name)
		}
	}
	s.Proxy.ReplicatePacket(ctx.Request.Packet, enrich)
}
