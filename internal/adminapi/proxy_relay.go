package adminapi

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/talkincode/toughradius/v9/internal/domain"
	"github.com/talkincode/toughradius/v9/internal/radiusd"
	"github.com/talkincode/toughradius/v9/internal/radiusd/proxy"
	"go.uber.org/zap"
)

// relayDisconnectUpstream sends a best-effort Disconnect-Request to the
// upstream server when a kicked session belongs to a proxied realm
// (multi-level ISP scenario, CoA protocol method). The upstream username is
// reconstructed with the route's credential transformation (campus → carrier
// mapping), so the upstream can match its own session. Failures are logged
// and never affect the local disconnect result.
//
// The upstream relay runs synchronously in the caller's context but is
// intended to be invoked from a goroutine by DisconnectOnlineSession.
func relayDisconnectUpstream(c echo.Context, session *domain.RadiusOnline) {
	if session == nil || session.Username == "" {
		return
	}
	appCtx := GetAppContext(c)
	if appCtx == nil || !appCtx.Config().Radiusd.ProxyEnabled {
		return // proxy subsystem disabled: nothing to relay
	}

	proxySvc := proxy.NewService(appCtx, 5*time.Second, 2)
	route := proxySvc.MatchRoute(session.Username)
	if route == nil {
		return
	}
	pool := proxySvc.UpstreamsFor(route)
	if len(pool) == 0 {
		return
	}
	up := pool[0]

	_, localPart := proxy.SplitUsername(session.Username)
	upstreamUsername := route.UserPrefix + localPart + route.UserSuffix
	if upstreamUsername == "" {
		upstreamUsername = session.Username
	}

	identity := radiusd.SessionIdentity{
		Username:       upstreamUsername,
		NasIP:          session.NasAddr,
		NasIdentifier:  session.NasId,
		AcctSessionID:  session.AcctSessionId,
		FramedIP:       session.FramedIpaddr,
		CallingStation: session.MacAddr,
		NasPortID:      session.NasPortId,
	}
	target := radiusd.CoATarget{Addr: up.Addr, Secret: up.Secret, Port: up.CoaPort}

	coa := radiusd.NewCoAService(nil)
	result, err := coa.Disconnect(context.Background(), target, identity)
	if err != nil {
		zap.L().Warn("upstream disconnect relay build failed",
			zap.String("namespace", "proxy"),
			zap.String("upstream", up.Name),
			zap.String("username", upstreamUsername),
			zap.Error(err),
		)
		return
	}
	zap.L().Info("upstream disconnect relay sent",
		zap.String("namespace", "proxy"),
		zap.String("upstream", up.Name),
		zap.String("username", upstreamUsername),
		zap.String("target", result.Target),
		zap.Bool("success", result.Success),
	)
}
