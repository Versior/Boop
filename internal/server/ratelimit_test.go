package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestGuardRateLimitStopsAtTheFirstBlockedKey pins the review finding: once the
// first key (the address) is out of tokens, the keys behind it must not be spent
// or even created, so a blocked address cannot grow the map with fresh mailboxes.
func TestGuardRateLimitStopsAtTheFirstBlockedKey(t *testing.T) {
	clock := newFakeClock()
	s := &server{limiters: newLimiters(clock.Now)}
	login := s.limiters.login
	ip := ipKey("203.0.113.7:41000")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	for i := 0; i < loginBurst; i++ {
		if !s.guardRateLimit(recorder, request, login, "login", ip, emailKey("same@example.com")) {
			t.Fatalf("attempt %d was refused", i+1)
		}
	}
	before := login.size()

	fresh := emailKey("fresh@example.com")
	recorder = httptest.NewRecorder()
	if s.guardRateLimit(recorder, request, login, "login", ip, fresh) {
		t.Fatal("the blocked address must be refused")
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
	if login.size() != before {
		t.Errorf("bucket count = %d, want %d: the email key behind a blocked address must not be created", login.size(), before)
	}
	login.mu.Lock()
	_, created := login.buckets["login:"+fresh]
	login.mu.Unlock()
	if created {
		t.Error("the email key behind a blocked address was created")
	}
	if _, created := login.buckets["login:"+emailKey("same@example.com")]; !created {
		t.Error("the first key's own bucket should still be tracked")
	}

	// Once the address has refilled, a different mailbox is usable again.
	clock.advance(loginRefill * loginBurst)
	recorder = httptest.NewRecorder()
	if !s.guardRateLimit(recorder, request, login, "login", ip, fresh) {
		t.Fatalf("the address should be usable again after a full refill: %s", recorder.Body.String())
	}
	if _, created := login.buckets["login:"+fresh]; !created {
		t.Error("an allowed request must consume its email key")
	}
}

// TestEmailKeyIsFixedLengthAndNormalised keeps the bucket name from storing or
// amplifying the presented address.
func TestEmailKeyIsFixedLengthAndNormalised(t *testing.T) {
	canonical := emailKey("reader@example.com")
	for _, same := range []string{" reader@example.com ", "READER@EXAMPLE.COM", "\tReader@Example.Com\n"} {
		if got := emailKey(same); got != canonical {
			t.Errorf("emailKey(%q) = %q, want %q", same, got, canonical)
		}
	}

	if other := emailKey("other@example.com"); other == canonical {
		t.Error("different addresses must not share a bucket")
	}
	if strings.Contains(canonical, "@") || strings.Contains(canonical, "example.com") {
		t.Errorf("the key must not carry the address: %q", canonical)
	}

	// A near-limit address must not produce a longer key than a short one.
	long := emailKey(strings.Repeat("a", 60_000) + "@example.com")
	if len(long) != len(canonical) {
		t.Errorf("key length = %d, want the fixed %d", len(long), len(canonical))
	}
	if len(long) > 128 {
		t.Errorf("key = %d bytes, want a fixed-size hash", len(long))
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
