// Package ratelimit provides an in-process token-bucket limiter.
//
// It is deliberately per-process, not distributed. Behind N replicas a client
// gets N times the budget, which is fine: this exists to stop one address from
// exhausting a server, not to meter a paid API. A distributed limiter needs a
// shared store on the hot path of every request, and that is a worse trade until
// there is a reason to make it.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a concurrency-safe collection of token buckets, keyed by string.
type Limiter struct {
	rate      float64 // tokens per second
	burst     float64
	sweep     time.Duration
	maxKeys   int
	now       func() time.Time
	mu        sync.Mutex
	buckets   map[string]*bucket
	lastPurge time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Options configures a Limiter.
type Options struct {
	// Burst is the bucket size: how many requests may arrive at once.
	Burst int

	// Every is the interval at which one token is replenished. Burst 5 with Every
	// 12s means five immediate attempts, then one every twelve seconds.
	Every time.Duration

	// MaxKeys bounds memory. An attacker cycling source addresses would otherwise
	// grow the map without limit — a rate limiter that can be exhausted by the
	// traffic it exists to limit is not one.
	MaxKeys int
}

// New returns a Limiter.
func New(opts Options) *Limiter {
	if opts.Burst <= 0 {
		opts.Burst = 1
	}
	if opts.Every <= 0 {
		opts.Every = time.Second
	}
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = 16384
	}

	return &Limiter{
		rate:    1 / opts.Every.Seconds(),
		burst:   float64(opts.Burst),
		sweep:   opts.Every * time.Duration(opts.Burst) * 2,
		maxKeys: opts.MaxKeys,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
}

// Allow consumes a token for key. It reports whether the request may proceed and,
// when it may not, how long the caller should wait before retrying.
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.purgeLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		// A new key starts full, minus the request that created it.
		l.buckets[key] = &bucket{tokens: l.burst - 1, last: now}
		return true, 0
	}

	// Refill for the time elapsed, capped at the bucket size.
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now

	if b.tokens < 1 {
		// Time until one whole token exists.
		wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
		return false, wait
	}

	b.tokens--
	return true, 0
}

// purgeLocked drops buckets that have had time to refill completely, since a full
// bucket is indistinguishable from an absent one.
func (l *Limiter) purgeLocked(now time.Time) {
	if now.Sub(l.lastPurge) < l.sweep && len(l.buckets) < l.maxKeys {
		return
	}
	l.lastPurge = now

	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, key)
		}
	}

	// Still over the bound: the working set is genuinely large, or someone is
	// cycling keys. Drop everything rather than grow without limit. The cost is
	// that a handful of throttled clients get a fresh budget; the alternative is
	// running out of memory.
	if len(l.buckets) >= l.maxKeys {
		clear(l.buckets)
	}
}

// Len reports the number of live buckets.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
