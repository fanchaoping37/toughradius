package radiusd

// Short-lived cache of the last upstream Access-Reject reason for dial-proxy
// sessions. DialProxy keeps a per-username failure message for five minutes so
// a NAS that receives a synthesized reject without a Reply-Message can still
// display the original operator error to the user.
//
// The cache is process-local and only decorates the Reply-Message attribute.
// It never influences the Access-Accept/Access-Reject decision, and no password
// is ever stored or logged here.

import (
	"sync"
	"time"
)

const (
	// dialRejectTTL is the lifetime of an upstream rejection reason.
	dialRejectTTL = 5 * time.Minute
	// dialRejectCleanupInterval bounds how long an expired entry may remain
	// in the map when nobody looks it up again.
	dialRejectCleanupInterval = time.Minute
)

type dialRejectEntry struct {
	message   string
	expiresAt time.Time
}

// dialRejectCache is a thread-safe TTL cache keyed by username. Get also
// checks expiration, so a correct reason is never returned after the TTL even
// if the background cleaner has not run yet.
type dialRejectCache struct {
	mu      sync.Mutex
	entries map[string]dialRejectEntry
	ttl     time.Duration
	cleanup time.Duration

	stop      chan struct{}
	closeOnce sync.Once
}

// dialRejectReasons is the process-wide cache used by the proxy auth stage.
var dialRejectReasons = newDialRejectCache()

func newDialRejectCache() *dialRejectCache {
	return newDialRejectCacheWithTTL(dialRejectTTL, dialRejectCleanupInterval)
}

func newDialRejectCacheWithTTL(ttl, cleanup time.Duration) *dialRejectCache {
	if ttl <= 0 {
		ttl = dialRejectTTL
	}
	if cleanup <= 0 {
		cleanup = dialRejectCleanupInterval
	}
	c := &dialRejectCache{
		entries: make(map[string]dialRejectEntry),
		ttl:     ttl,
		cleanup: cleanup,
		stop:    make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

func (c *dialRejectCache) Set(username, message string) {
	if username == "" || message == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[username] = dialRejectEntry{
		message:   message,
		expiresAt: time.Now().Add(c.ttl),
	}
}

func (c *dialRejectCache) Get(username string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[username]
	if !ok {
		return "", false
	}
	if !time.Now().Before(entry.expiresAt) {
		delete(c.entries, username)
		return "", false
	}
	return entry.message, true
}

func (c *dialRejectCache) Delete(username string) {
	c.mu.Lock()
	delete(c.entries, username)
	c.mu.Unlock()
}

// cleanupLoop periodically drops expired entries. It stops on Close so tests
// can create short-lived caches without leaking goroutines.
func (c *dialRejectCache) cleanupLoop() {
	ticker := time.NewTicker(c.cleanup)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			now := time.Now()
			for username, entry := range c.entries {
				if !now.Before(entry.expiresAt) {
					delete(c.entries, username)
				}
			}
			c.mu.Unlock()
		case <-c.stop:
			return
		}
	}
}

func (c *dialRejectCache) Close() {
	c.closeOnce.Do(func() { close(c.stop) })
}
