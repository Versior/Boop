package server

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Boop v0.1 runs one process with SQLite, so rate limits live in this process's
// memory. The limits below are deliberately conservative, and they reset when
// the process restarts. Running more than one process multiplies them, which
// docs/API.md states as the supported ceiling.
const (
	// login: ten immediate attempts, then one every six seconds.
	loginBurst  = 10
	loginRefill = 6 * time.Second
	// register: five immediate accounts, then one every two minutes.
	registerBurst  = 5
	registerRefill = 2 * time.Minute
	// comment: five immediate comments per account, then one every twelve
	// seconds. The address budget is deliberately looser, so one noisy account
	// cannot stop a whole shared network from commenting.
	commentBurst    = 5
	commentRefill   = 12 * time.Second
	commentIPBurst  = 30
	commentIPRefill = 2 * time.Second
	// aiBurst and aiRefill are the reserved budget for the AI endpoints of
	// Task 8 (three immediate calls, then one every twenty seconds). Nothing
	// calls them yet: the AI routes do not exist, and this package does not
	// pre-create an unused limiter for them.
	aiBurst  = 3
	aiRefill = 20 * time.Second

	// sweepEvery bounds how often the map is scanned, and idleTTL is how long an
	// untouched key may linger. A bucket that refills completely carries no
	// state, so dropping it is always safe. The sweep runs on the first request
	// after a minute has passed, not on a timer of its own.
	sweepEvery = time.Minute
	idleTTL    = 10 * time.Minute
)

// bucket is one key's remaining tokens.
type bucket struct {
	tokens float64
	seen   time.Time
}

// limiter is a single-process token bucket keyed by an opaque string. Keys are
// swept lazily, so an attacker cycling through emails cannot grow the map
// without bound.
type limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	burst     float64
	refill    time.Duration
	now       func() time.Time
	lastSweep time.Time
}

func newLimiter(burst int, refill time.Duration, now func() time.Time) *limiter {
	return &limiter{
		buckets: make(map[string]*bucket),
		burst:   float64(burst),
		refill:  refill,
		now:     now,
	}
}

// allow consumes one token for the key. When the bucket is empty it reports how
// long the caller must wait, which the HTTP layer turns into a 429 with
// Retry-After.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	entry, ok := l.buckets[key]
	if !ok {
		entry = &bucket{tokens: l.burst}
		l.buckets[key] = entry
	} else if elapsed := now.Sub(entry.seen); elapsed > 0 {
		entry.tokens = min(l.burst, entry.tokens+float64(elapsed)/float64(l.refill))
	}
	entry.seen = now

	if entry.tokens >= 1 {
		entry.tokens--
		return true, 0
	}
	// Time until the next full token. The HTTP layer rounds this up to whole
	// seconds for Retry-After; the value itself stays exact so callers can log it.
	missing := 1 - entry.tokens
	retry := time.Duration(missing * float64(l.refill))
	if retry <= 0 {
		retry = time.Millisecond
	}
	return false, retry
}

// sweepLocked drops buckets that carry no state: full again, or untouched for
// longer than idleTTL. It runs at most once per sweepEvery.
func (l *limiter) sweepLocked(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for key, entry := range l.buckets {
		refilled := entry.tokens + float64(now.Sub(entry.seen))/float64(l.refill)
		if refilled >= l.burst || now.Sub(entry.seen) >= idleTTL {
			delete(l.buckets, key)
		}
	}
}

// size reports how many keys are tracked; tests use it to prove the sweep.
func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// limiters holds the buckets the HTTP layer consults. One bucket per limit, and
// the key decides who shares it.
type limiters struct {
	login     *limiter
	register  *limiter
	comment   *limiter
	commentIP *limiter
}

func newLimiters(now func() time.Time) *limiters {
	return &limiters{
		login:     newLimiter(loginBurst, loginRefill, now),
		register:  newLimiter(registerBurst, registerRefill, now),
		comment:   newLimiter(commentBurst, commentRefill, now),
		commentIP: newLimiter(commentIPBurst, commentIPRefill, now),
	}
}

// ipKey scopes a limit to the connecting address. Boop reads RemoteAddr only:
// a forwarded header would be attacker-controlled unless a reverse proxy is
// configured to overwrite it, which docs/API.md states explicitly.
func ipKey(remoteAddr string) string {
	return "ip:" + remoteHost(remoteAddr)
}

func userKey(id int64) string {
	return "user:" + strconv.FormatInt(id, 10)
}

// emailKey scopes a limit to the presented address. The address is unverified
// input, so the key is its SHA-256: a fixed-length key cannot be inflated by a
// long mailbox name, and the bucket name does not store the address itself.
// Case and surrounding space are normalised away, as auth.NormalizeEmail does.
func emailKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "email:" + hex.EncodeToString(sum[:])
}

// guardRateLimit consumes one token from each key of a limit, in the caller's
// order. The first key that is out of tokens ends the request: a caller who is
// already blocked must not be able to spend, or even create, the keys behind it,
// or a single banned address could keep growing the map with fresh mailboxes.
// Callers that limit by address pass the address first.
func (s *server) guardRateLimit(w http.ResponseWriter, r *http.Request, bucket *limiter, scope string, keys ...string) bool {
	if bucket == nil {
		return true
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if allowed, wait := bucket.allow(scope + ":" + key); !allowed {
			writeRateLimited(w, r, wait)
			return false
		}
	}
	return true
}

// writeRateLimited answers 429 with the exact wait in both a Retry-After header
// and the body, so a client can back off without guessing.
func writeRateLimited(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	seconds := int(math.Ceil(retry.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": map[string]string{
			"code":    "rate_limited",
			"message": "请求过于频繁，请稍后再试",
		},
		"request_id":  requestIDFrom(r.Context()),
		"retry_after": seconds,
	})
}
