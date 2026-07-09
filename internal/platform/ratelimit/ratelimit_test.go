package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// clockedLimiter returns a limiter whose clock the test controls, so timing
// behaviour is asserted rather than slept for.
func clockedLimiter(t *testing.T, opts Options) (*Limiter, func(time.Duration)) {
	t.Helper()

	l := New(opts)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	return l, func(d time.Duration) { now = now.Add(d) }
}

func TestBurstIsAllowedThenRefused(t *testing.T) {
	t.Parallel()

	l, _ := clockedLimiter(t, Options{Burst: 3, Every: time.Second})

	for i := range 3 {
		if allowed, _ := l.Allow("client"); !allowed {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}

	allowed, retryAfter := l.Allow("client")
	if allowed {
		t.Fatal("a fourth request was allowed with a burst of three")
	}
	if retryAfter <= 0 {
		t.Error("a refusal must say how long to wait")
	}
}

func TestTokensRefillOverTime(t *testing.T) {
	t.Parallel()

	l, advance := clockedLimiter(t, Options{Burst: 2, Every: time.Second})

	l.Allow("client")
	l.Allow("client")
	if allowed, _ := l.Allow("client"); allowed {
		t.Fatal("the bucket should be empty")
	}

	advance(time.Second)
	if allowed, _ := l.Allow("client"); !allowed {
		t.Error("one token should have refilled after one interval")
	}
	if allowed, _ := l.Allow("client"); allowed {
		t.Error("only one token should have refilled")
	}

	advance(10 * time.Second)
	for i := range 2 {
		if allowed, _ := l.Allow("client"); !allowed {
			t.Errorf("refill %d refused; the bucket should be full again", i+1)
		}
	}
	if allowed, _ := l.Allow("client"); allowed {
		t.Error("the bucket refilled beyond its burst")
	}
}

// Exhausting one key must not affect another: locking out every client because
// one attacker hammered login would be a denial of service we performed
// ourselves.
func TestKeysAreIndependent(t *testing.T) {
	t.Parallel()

	l, _ := clockedLimiter(t, Options{Burst: 1, Every: time.Minute})

	if allowed, _ := l.Allow("attacker"); !allowed {
		t.Fatal("first request refused")
	}
	if allowed, _ := l.Allow("attacker"); allowed {
		t.Fatal("the attacker was not limited")
	}
	if allowed, _ := l.Allow("innocent"); !allowed {
		t.Error("an unrelated client was limited by somebody else's traffic")
	}
}

func TestRetryAfterShrinksAsTokensRefill(t *testing.T) {
	t.Parallel()

	l, advance := clockedLimiter(t, Options{Burst: 1, Every: 10 * time.Second})

	l.Allow("client")
	_, first := l.Allow("client")

	advance(5 * time.Second)
	_, second := l.Allow("client")

	if second >= first {
		t.Errorf("retryAfter did not shrink: %v then %v", first, second)
	}
	if second <= 0 {
		t.Error("retryAfter went non-positive while still refusing")
	}
}

// A limiter that can be exhausted by the traffic it exists to limit is not one.
func TestMaxKeysBoundsMemory(t *testing.T) {
	t.Parallel()

	const max = 16
	l, _ := clockedLimiter(t, Options{Burst: 1, Every: time.Hour, MaxKeys: max})

	for i := range max * 8 {
		l.Allow(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}

	if l.Len() > max {
		t.Errorf("limiter holds %d keys, want at most %d", l.Len(), max)
	}
}

// A bucket that has refilled completely is indistinguishable from one that never
// existed, so it may be dropped.
func TestFullBucketsArePurged(t *testing.T) {
	t.Parallel()

	l, advance := clockedLimiter(t, Options{Burst: 2, Every: time.Second})

	l.Allow("a")
	l.Allow("b")
	if l.Len() != 2 {
		t.Fatalf("len = %d, want 2", l.Len())
	}

	// Long enough for both to refill, and past the sweep interval.
	advance(time.Minute)
	l.Allow("c")

	if l.Len() > 1 {
		t.Errorf("len = %d; refilled buckets should have been purged", l.Len())
	}
}

func TestConcurrentAllowIsSafe(t *testing.T) {
	t.Parallel()

	l := New(Options{Burst: 100, Every: time.Millisecond})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				l.Allow("shared")
			}
		}()
	}
	wg.Wait() // the race detector is the assertion
}

// A burst of zero would silently mean "allow nothing"; a zero interval would
// divide by zero. Both are defaulted.
func TestZeroOptionsAreDefaulted(t *testing.T) {
	t.Parallel()

	l := New(Options{})
	if allowed, _ := l.Allow("client"); !allowed {
		t.Error("a zero-valued limiter refused the first request")
	}
}
