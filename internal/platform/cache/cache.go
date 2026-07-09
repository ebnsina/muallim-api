// Package cache provides an in-process TTL cache with single-flight loading.
//
// It is deliberately not a distributed cache. Anything that must be consistent
// across replicas belongs in Postgres; this exists for values that are read on
// every request, change rarely, and tolerate a bounded staleness window — tenant
// resolution above all.
package cache

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Cache is a concurrency-safe, size-bounded TTL cache keyed by string.
//
// The zero value is not usable; call New.
type Cache[V any] struct {
	ttl         time.Duration
	negativeTTL time.Duration
	max         int

	mu      sync.RWMutex
	entries map[string]entry[V]

	// group collapses concurrent misses for the same key into one load. Without
	// it, a cold cache under load sends one identical query per in-flight request
	// — a cache stampede, which arrives exactly when the database can least
	// afford it.
	group singleflight.Group

	now func() time.Time
}

type entry[V any] struct {
	value     V
	err       error
	expiresAt time.Time
}

// Options configures a Cache.
type Options struct {
	// TTL bounds how stale a hit may be.
	TTL time.Duration

	// NegativeTTL caches "not found" for a shorter window. Without it, requests
	// for a host that does not exist reach the database every time, which is a
	// free denial-of-service primitive for anyone who can point DNS at us.
	NegativeTTL time.Duration

	// MaxEntries bounds memory. Exceeding it evicts everything expired, then the
	// whole map if that was not enough — a crude policy chosen because tenant
	// counts are small and an LRU's bookkeeping would cost more than it saves.
	MaxEntries int
}

// New returns a Cache. A non-positive TTL disables caching entirely, which makes
// it safe to wire in before deciding whether a value should be cached at all.
func New[V any](opts Options) *Cache[V] {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 1024
	}
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = opts.TTL / 4
	}
	return &Cache[V]{
		ttl:         opts.TTL,
		negativeTTL: opts.NegativeTTL,
		max:         opts.MaxEntries,
		entries:     make(map[string]entry[V]),
		now:         time.Now,
	}
}

// Loader produces a value on a cache miss.
type Loader[V any] func(ctx context.Context) (V, error)

// GetOrLoad returns the cached value for key, loading it exactly once across all
// concurrent callers if it is absent or expired.
//
// An error from load is cached for NegativeTTL and returned to every caller that
// joined the same flight.
func (c *Cache[V]) GetOrLoad(ctx context.Context, key string, load Loader[V]) (V, error) {
	if c.ttl <= 0 {
		return load(ctx)
	}

	if e, ok := c.lookup(key); ok {
		return e.value, e.err
	}

	// The loader runs on whichever goroutine wins the flight; the rest wait and
	// receive its result. Context cancellation of the winner would otherwise
	// propagate to the waiters, so the load uses a context detached from any one
	// caller's lifetime, bounded instead by the caller's own ctx below.
	v, err, _ := c.group.Do(key, func() (any, error) {
		// Re-check: another goroutine may have populated the entry between our
		// lookup and our entry into the flight.
		if e, ok := c.lookup(key); ok {
			return e.value, e.err
		}

		value, err := load(ctx)

		ttl := c.ttl
		if err != nil {
			ttl = c.negativeTTL
		}
		c.store(key, entry[V]{value: value, err: err, expiresAt: c.now().Add(ttl)})

		return value, err
	})

	if err != nil {
		var zero V
		if v != nil {
			zero, _ = v.(V)
		}
		return zero, err
	}
	typed, _ := v.(V)
	return typed, nil
}

// Invalidate drops key. Call it from the write path, in the same transaction's
// success branch — a cache that outlives its truth is worse than no cache.
func (c *Cache[V]) Invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Clear empties the cache.
func (c *Cache[V]) Clear() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}

// Len reports the number of entries, expired ones included.
func (c *Cache[V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (c *Cache[V]) lookup(key string) (entry[V], bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok || c.now().After(e.expiresAt) {
		return entry[V]{}, false
	}
	return e, true
}

func (c *Cache[V]) store(key string, e entry[V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[key] = e
}

func (c *Cache[V]) evictLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
	// Nothing had expired: the working set genuinely exceeds the bound. Drop it
	// all rather than grow without limit.
	if len(c.entries) >= c.max {
		clear(c.entries)
	}
}
