package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ebnsina/lms-api/internal/platform/cache"
)

func TestGetOrLoadCachesHits(t *testing.T) {
	t.Parallel()

	c := cache.New[string](cache.Options{TTL: time.Minute})
	var loads atomic.Int64

	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "value", nil
	}

	for range 5 {
		got, err := c.GetOrLoad(t.Context(), "k", load)
		if err != nil || got != "value" {
			t.Fatalf("GetOrLoad = %q, %v", got, err)
		}
	}

	if loads.Load() != 1 {
		t.Errorf("loader ran %d times, want 1", loads.Load())
	}
}

// The reason this cache exists. On a cold start behind a load balancer, every
// in-flight request misses at once; without single-flight each one issues an
// identical query, and the stampede lands exactly when the database is coldest.
func TestGetOrLoadCollapsesConcurrentMisses(t *testing.T) {
	t.Parallel()

	c := cache.New[int](cache.Options{TTL: time.Minute})

	var loads atomic.Int64
	release := make(chan struct{})

	load := func(context.Context) (int, error) {
		loads.Add(1)
		<-release // hold the flight open so every goroutine piles onto it
		return 42, nil
	}

	const goroutines = 50
	var wg sync.WaitGroup
	results := make([]int, goroutines)

	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(context.Background(), "hot", load)
			if err != nil {
				t.Errorf("GetOrLoad: %v", err)
				return
			}
			results[i] = v
		}()
	}

	// Give every goroutine time to arrive at the flight before letting it finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := loads.Load(); got != 1 {
		t.Errorf("%d concurrent misses caused %d loads, want exactly 1", goroutines, got)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("goroutine %d got %d, want 42 — every waiter must receive the flight's result", i, v)
		}
	}
}

func TestGetOrLoadExpires(t *testing.T) {
	t.Parallel()

	c := cache.New[string](cache.Options{TTL: 30 * time.Millisecond})
	var loads atomic.Int64

	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "v", nil
	}

	if _, err := c.GetOrLoad(t.Context(), "k", load); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := c.GetOrLoad(t.Context(), "k", load); err != nil {
		t.Fatal(err)
	}

	if loads.Load() != 2 {
		t.Errorf("loader ran %d times, want 2 — the entry should have expired", loads.Load())
	}
}

// Without negative caching, requests for a host nobody registered reach the
// database every time — a free denial-of-service primitive for anyone who can
// point DNS at us.
func TestGetOrLoadCachesErrorsBriefly(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("not found")
	c := cache.New[string](cache.Options{TTL: time.Minute, NegativeTTL: time.Minute})

	var loads atomic.Int64
	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "", wantErr
	}

	for range 3 {
		_, err := c.GetOrLoad(t.Context(), "missing", load)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	}

	if loads.Load() != 1 {
		t.Errorf("loader ran %d times for a failing key, want 1", loads.Load())
	}
}

func TestInvalidateForcesReload(t *testing.T) {
	t.Parallel()

	c := cache.New[string](cache.Options{TTL: time.Minute})
	var loads atomic.Int64

	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "v", nil
	}

	if _, err := c.GetOrLoad(t.Context(), "k", load); err != nil {
		t.Fatal(err)
	}
	c.Invalidate("k")
	if _, err := c.GetOrLoad(t.Context(), "k", load); err != nil {
		t.Fatal(err)
	}

	if loads.Load() != 2 {
		t.Errorf("loader ran %d times, want 2 after invalidation", loads.Load())
	}
}

// A zero TTL disables caching, which is what a test wants when every call must
// reach the repository.
func TestZeroTTLBypassesTheCache(t *testing.T) {
	t.Parallel()

	c := cache.New[string](cache.Options{TTL: 0})
	var loads atomic.Int64

	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "v", nil
	}

	for range 3 {
		if _, err := c.GetOrLoad(t.Context(), "k", load); err != nil {
			t.Fatal(err)
		}
	}

	if loads.Load() != 3 {
		t.Errorf("loader ran %d times, want 3 — a zero TTL must not cache", loads.Load())
	}
	if c.Len() != 0 {
		t.Errorf("cache holds %d entries, want 0", c.Len())
	}
}

func TestMaxEntriesBoundsMemory(t *testing.T) {
	t.Parallel()

	const max = 8
	c := cache.New[int](cache.Options{TTL: time.Minute, MaxEntries: max})

	for i := range max * 4 {
		key := string(rune('a' + i%26))
		if _, err := c.GetOrLoad(t.Context(), key+string(rune('0'+i/26)), func(context.Context) (int, error) {
			return i, nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if c.Len() > max {
		t.Errorf("cache holds %d entries, want at most %d", c.Len(), max)
	}
}
