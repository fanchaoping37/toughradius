// Package proxy implements the RADIUS proxy / replication subsystem.
//
// Responsibilities:
//   - realm route matching (username@realm → route)
//   - upstream pool selection (single / round_robin / weight)
//   - upstream forwarding of Access-Request and Accounting-Request packets
//   - fire-and-forget replication of request copies ("抄送")
//   - PAP credential transformation (campus account → carrier account)
//
// The package is deliberately independent of the radiusd service layer so it
// can be unit-tested in isolation; callers inject a database/config provider
// (satisfied by app.AppContext).
package proxy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/talkincode/toughradius/v9/internal/domain"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"layeh.com/radius"
)

// Provider is the minimal application context the proxy service needs.
// app.AppContext satisfies it.
type Provider interface {
	DB() *gorm.DB
}

// ErrEAPNotSupported reports that a request carries EAP-Message attributes,
// which the proxy subsystem does not forward (v1 scope: PAP/CHAP/MSCHAPv2).
var ErrEAPNotSupported = fmt.Errorf("proxy: eap requests are not forwarded")

// ErrNoUpstream reports that a matched route has no usable enabled upstream.
var ErrNoUpstream = fmt.Errorf("proxy: route has no enabled upstream")

// Service owns the proxy subsystem state: DB-backed caches of upstreams and
// realm routes, and the round-robin counter shared by all pool selections.
type Service struct {
	provider Provider

	mu        sync.RWMutex
	upstreams map[int64]*domain.RadiusUpstream
	routes    []*domain.RadiusRealmRoute
	loadedAt  time.Time

	rrCounter atomic.Uint64

	// exchange is overridable in tests; default is a radius.Client exchange
	// configured with the service timeout and retry budget.
	exchange func(ctx context.Context, packet *radius.Packet, addr string) (*radius.Packet, error)

	timeout time.Duration
	retry   int
}

// NewService creates a proxy Service. Cache entries are lazily loaded on the
// first route match and refreshed every cacheTTL.
func NewService(provider Provider, timeout time.Duration, retry int) *Service {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if retry < 0 {
		retry = 0
	}
	s := &Service{
		provider:  provider,
		upstreams: map[int64]*domain.RadiusUpstream{},
		timeout:   timeout,
		retry:     retry,
	}
	return s
}

const cacheTTL = 30 * time.Second

// refresh reloads enabled upstreams and realm routes from the database.
func (s *Service) refresh() {
	if s.provider == nil {
		return // test mode: state is set directly on the service
	}
	s.mu.RLock()
	fresh := time.Since(s.loadedAt) < cacheTTL
	s.mu.RUnlock()
	if fresh {
		return
	}
	db := s.provider.DB()
	if db == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.loadedAt) < cacheTTL {
		return
	}

	ups := []*domain.RadiusUpstream{}
	if err := db.Where("status = ?", "enabled").Find(&ups).Error; err != nil {
		zap.L().Warn("proxy: load upstreams failed", zap.Error(err))
		return
	}
	routes := []*domain.RadiusRealmRoute{}
	if err := db.Where("status = ?", "enabled").Order("id ASC").Find(&routes).Error; err != nil {
		zap.L().Warn("proxy: load realm routes failed", zap.Error(err))
		return
	}

	next := make(map[int64]*domain.RadiusUpstream, len(ups))
	for _, u := range ups {
		next[u.ID] = u
	}
	s.upstreams = next
	s.routes = routes
	s.loadedAt = time.Now()
}

// Enabled reports whether any upstream or route is configured, i.e. whether
// the proxy path needs to be evaluated at all.
func (s *Service) Enabled() bool {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.routes) > 0 || len(s.upstreams) > 0
}

// MatchRoute resolves the realm route for a username. The realm is the
// suffix after the last "@". Routes are matched case-insensitively; the
// special realm "*" acts as the catch-all for unmatched (or realm-less)
// usernames. Returns nil when no route applies.
func (s *Service) MatchRoute(username string) *domain.RadiusRealmRoute {
	s.refresh()
	_, realm := SplitUsername(username)

	s.mu.RLock()
	defer s.mu.RUnlock()

	var fallback *domain.RadiusRealmRoute
	for _, r := range s.routes {
		if r.Realm == "*" {
			fallback = r
			continue
		}
		if strings.EqualFold(r.Realm, realm) && realm != "" {
			// First matching route wins (id ASC order).
			return r
		}
	}
	return fallback
}

// UpstreamsFor resolves the enabled upstream pool of a route, preserving the
// order of UpstreamIds.
func (s *Service) UpstreamsFor(route *domain.RadiusRealmRoute) []*domain.RadiusUpstream {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := ParseIDs(route.UpstreamIds)
	result := make([]*domain.RadiusUpstream, 0, len(ids))
	for _, id := range ids {
		if u, ok := s.upstreams[id]; ok && u.Role == domain.UpstreamRoleProxy && u.Status == "enabled" {
			result = append(result, u)
		}
	}
	return result
}

// ReplicateTargets returns all enabled upstreams with role=replicate.
func (s *Service) ReplicateTargets() []*domain.RadiusUpstream {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*domain.RadiusUpstream, 0)
	for _, u := range s.upstreams {
		if u.Role == domain.UpstreamRoleReplicate && u.Status == "enabled" {
			result = append(result, u)
		}
	}
	return result
}

// Select picks one upstream from the pool according to the route strategy.
func (s *Service) Select(route *domain.RadiusRealmRoute, pool []*domain.RadiusUpstream) *domain.RadiusUpstream {
	if len(pool) == 0 {
		return nil
	}
	switch route.Strategy {
	case domain.RealmStrategyRoundRobin:
		return pool[int(s.rrCounter.Add(1))%len(pool)]
	case domain.RealmStrategyWeight:
		// Expand the pool by weight and round-robin over the expansion.
		expanded := make([]*domain.RadiusUpstream, 0, len(pool))
		for _, u := range pool {
			w := u.Weight
			if w <= 0 {
				w = 1
			}
			for i := 0; i < w; i++ {
				expanded = append(expanded, u)
			}
		}
		return expanded[int(s.rrCounter.Add(1))%len(expanded)]
	default: // single
		return pool[0]
	}
}

// SplitUsername splits "user@realm" into ("user", "realm"). A username
// without "@" yields (username, "").
func SplitUsername(username string) (string, string) {
	if idx := strings.LastIndex(username, "@"); idx >= 0 {
		return username[:idx], username[idx+1:]
	}
	return username, ""
}

// ParseIDs parses a comma-separated int64 list, skipping invalid entries.
func ParseIDs(csv string) []int64 {
	parts := strings.Split(csv, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}
