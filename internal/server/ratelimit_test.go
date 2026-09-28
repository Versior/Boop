package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock so bucket refill is deterministic.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 28, 6, 2, 3, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestLimiterAllowsTheBurstThenRefuses(t *testing.T) {
	clock := newFakeClock()
	bucket := newLimiter(3, time.Second, clock.Now)

	for i := 0; i < 3; i++ {
		if allowed, retry := bucket.allow("reader"); !allowed {
			t.Fatalf("request %d was refused with retry %v", i+1, retry)
		}
	}
	allowed, retry := bucket.allow("reader")
	if allowed {
		t.Fatal("the fourth request should be refused")
	}
	if retry <= 0 || retry > time.Second {
		t.Errorf("retry after = %v, want (0, 1s]", retry)
	}
}

func TestLimiterRecoversOverTime(t *testing.T) {
	clock := newFakeClock()
	bucket := newLimiter(2, time.Second, clock.Now)
	bucket.allow("reader")
	bucket.allow("reader")
	if allowed, _ := bucket.allow("reader"); allowed {
		t.Fatal("the burst should be spent")
	}

	clock.advance(time.Second)
	if allowed, _ := bucket.allow("reader"); !allowed {
		t.Fatal("one refill interval should return exactly one token")
	}
	if allowed, _ := bucket.allow("reader"); allowed {
		t.Fatal("a single refill must not return two tokens")
	}

	clock.advance(2 * time.Second)
	if allowed, _ := bucket.allow("reader"); !allowed {
		t.Fatal("two refill intervals should be usable")
	}
	if allowed, _ := bucket.allow("reader"); !allowed {
		t.Fatal("the second stored token should be usable")
	}
	if allowed, _ := bucket.allow("reader"); allowed {
		t.Fatal("the bucket must never exceed its burst")
	}
}

func TestLimiterRetryAfterShrinks(t *testing.T) {
	clock := newFakeClock()
	bucket := newLimiter(1, 800*time.Millisecond, clock.Now)
	bucket.allow("reader")

	_, first := bucket.allow("reader")
	clock.advance(400 * time.Millisecond)
	_, second := bucket.allow("reader")
	if second >= first {
		t.Errorf("retry after did not shrink: %v then %v", first, second)
	}
	if second <= 0 || second > 400*time.Millisecond {
		t.Errorf("retry after = %v, want (0, 400ms]", second)
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	clock := newFakeClock()
	bucket := newLimiter(1, time.Minute, clock.Now)

	if allowed, _ := bucket.allow("ip:203.0.113.7"); !allowed {
		t.Fatal("first key refused")
	}
	if allowed, _ := bucket.allow("ip:203.0.113.7"); allowed {
		t.Fatal("the same key must be spent")
	}
	if allowed, _ := bucket.allow("email:reader@example.com"); !allowed {
		t.Fatal("another key must have its own bucket")
	}
}

func TestLimiterSweepsIdleKeys(t *testing.T) {
	clock := newFakeClock()
	bucket := newLimiter(2, time.Second, clock.Now)

	for i := 0; i < 500; i++ {
		bucket.allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	if bucket.size() < 400 {
		t.Fatalf("expected the keys to be tracked, got %d", bucket.size())
	}

	// Idle keys whose tokens have refilled carry no state and must be dropped.
	clock.advance(11 * time.Minute)
	bucket.allow("fresh")
	if size := bucket.size(); size > 2 {
		t.Errorf("bucket size = %d after the sweep, want the idle keys gone", size)
	}

	// A key that is still being denied keeps its state.
	clock.advance(0)
	spent := newLimiter(1, time.Hour, clock.Now)
	spent.allow("hot")
	spent.allow("hot")
	clock.advance(2 * time.Minute)
	spent.allow("another")
	if allowed, _ := spent.allow("hot"); allowed {
		t.Error("the sweep dropped a key that should still be limited")
	}
}

// TestLimiterIsSafeForConcurrentUse exercises the mutex: the burst must be an
// exact ceiling, never a race-dependent number.
func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	clock := newFakeClock()
	bucket := newLimiter(50, time.Hour, clock.Now)

	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := bucket.allow("shared"); ok {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Errorf("allowed = %d, want exactly the burst of 50", allowed)
	}
}
