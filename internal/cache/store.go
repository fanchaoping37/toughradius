// Package cache provides an optional Redis-backed key/value store with an
// in-process fallback. Callers use one Store interface; when Redis is not
// configured or unreachable, the local implementation keeps the system
// functional without changing behavior.
package cache

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Store is a minimal TTL key/value store.
type Store interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	// SetNX sets a key only when absent. Used for idempotency and RFC 5080
	// duplicate suppression. Returns true when the key was created.
	SetNX(ctx context.Context, key string, value string, ttl time.Duration) (bool, error)
	Ping(ctx context.Context) error
	Close() error
}

var ErrNotFound = errors.New("cache: not found")

// Config configures the store.
type Config struct {
	Enabled  bool
	Addr     string // host:port
	Password string
	DB       int
	TTL      time.Duration // default TTL when caller passes zero
}

// New returns a Redis Store when cfg.Enabled, otherwise an in-memory Store.
func New(cfg Config) (Store, error) {
	if !cfg.Enabled || cfg.Addr == "" {
		return NewMemory(), nil
	}
	return newRedis(cfg)
}

// --- In-memory implementation ---------------------------------------------

type entry struct {
	value   string
	expires time.Time
}

// MemoryStore is a goroutine-safe in-process TTL store used as a fallback.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]entry
}

func NewMemory() *MemoryStore { return &MemoryStore{data: make(map[string]entry)} }

func (s *MemoryStore) Get(_ context.Context, key string) (string, bool, error) {
	s.mu.RLock()
	e, ok := s.data[key]
	s.mu.RUnlock()
	if !ok {
		return "", false, nil
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		s.mu.Lock()
		delete(s.data, key)
		s.mu.Unlock()
		return "", false, nil
	}
	return e.value, true, nil
}

func (s *MemoryStore) Set(_ context.Context, key, value string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expires time.Time
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	s.data[key] = entry{value: value, expires: expires}
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.data, key)
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) SetNX(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.data[key]; ok && (e.expires.IsZero() || time.Now().Before(e.expires)) {
		return false, nil
	}
	var expires time.Time
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	s.data[key] = entry{value: value, expires: expires}
	return true, nil
}

func (s *MemoryStore) Ping(context.Context) error { return nil }
func (s *MemoryStore) Close() error               { return nil }
