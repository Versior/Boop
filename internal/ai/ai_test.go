package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"boop/internal/store"
)

// testAPIKey is the fake key every test config carries. A test asserts that it
// never appears in an error or a log.
const testAPIKey = "sk-test-api-key-value"

// upstream is a stand-in OpenAI-compatible endpoint. It records what it was
// asked, can slow down, block, fail or answer with raw bytes.
type upstream struct {
	server *httptest.Server

	mu        sync.Mutex
	replies   []string // message contents, consumed in order; the last repeats
	raw       string   // when set, the whole response body
	status    int      // when non-zero, every answer uses this HTTP status
	delay     time.Duration
	calls     int
	active    int
	maxActive int
	paths     []string
	prompts   []string
	auth      string
}

func newUpstream(t *testing.T, replies ...string) *upstream {
	t.Helper()
	u := &upstream{replies: replies}
	u.server = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	u.mu.Lock()
	u.calls++
	u.active++
	if u.active > u.maxActive {
		u.maxActive = u.active
	}
	u.paths = append(u.paths, r.URL.Path)
	u.auth = r.Header.Get("Authorization")
	var payload struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &payload)
	for _, item := range payload.Messages {
		u.prompts = append(u.prompts, item.Content)
	}
	content := ""
	if len(u.replies) > 0 {
		content = u.replies[0]
		if len(u.replies) > 1 {
			u.replies = u.replies[1:]
		}
	}
	raw, status, delay := u.raw, u.status, u.delay
	u.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	defer func() {
		u.mu.Lock()
		u.active--
		u.mu.Unlock()
	}()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, raw)
		return
	}
	if raw != "" {
		_, _ = io.WriteString(w, raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":`+strconv.Quote(content)+`}}]}`)
}

// config points a Config at this endpoint.
func (u *upstream) config() Config {
	return Config{BaseURL: u.server.URL + "/v1", Model: "test-model", APIKey: testAPIKey, TTL: time.Hour}
}

func (u *upstream) counts() (calls, maxActive int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, u.maxActive
}

func (u *upstream) requests() (paths, prompts []string, auth string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...), append([]string(nil), u.prompts...), u.auth
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

// insertPost writes one post directly: the tests own the content they summarize.
func insertPost(t *testing.T, db *sql.DB, slug, kind, status, title, body, stamp string) {
	t.Helper()
	var published any
	if status == "published" {
		published = stamp
	}
	_, err := db.Exec(
		`INSERT INTO posts(slug, type, status, title, body_markdown, body_html, excerpt, published_at, created_at, updated_at)
		 VALUES(?,?,?,?,?,'','',?,?,?)`, slug, kind, status, title, body, published, stamp, stamp)
	if err != nil {
		t.Fatalf("insert post %s: %v", slug, err)
	}
}

// seedCache writes one ai_cache row with explicit timestamps.
func seedCache(t *testing.T, db *sql.DB, text string, topics []string, sourceUpdatedAt string, generatedAt, expiresAt time.Time) {
	t.Helper()
	raw, err := json.Marshal(storedStatus{Text: text, Topics: topics})
	if err != nil {
		t.Fatalf("marshal cached value: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO ai_cache(cache_key, value_json, source_updated_at, generated_at, expires_at)
		 VALUES(?,?,?,?,?)`,
		StatusCacheKey, string(raw), sourceUpdatedAt, stamp(generatedAt), stamp(expiresAt))
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}
}

func readCacheRow(t *testing.T, db *sql.DB) (value, sourceUpdatedAt, lastError string, found bool) {
	t.Helper()
	err := db.QueryRow(`SELECT value_json, source_updated_at, last_error FROM ai_cache WHERE cache_key = ?`, StatusCacheKey).
		Scan(&value, &sourceUpdatedAt, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false
	}
	if err != nil {
		t.Fatalf("read cache row: %v", err)
	}
	return value, sourceUpdatedAt, lastError, true
}

// ---------- client ----------

func TestCompleteJoinsTheConfiguredBaseURL(t *testing.T) {
	u := newUpstream(t, `{"text":"ok"}`)
	for _, base := range []string{u.server.URL + "/v1", u.server.URL + "/v1/"} {
		cfg := u.config()
		cfg.BaseURL = base
		if _, err := complete(t.Context(), cfg, []message{{Role: "user", Content: "hi"}}, 10); err != nil {
			t.Fatalf("complete(%q): %v", base, err)
		}
	}
	paths, _, auth := u.requests()
	for _, path := range paths {
		if path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", path)
		}
	}
	if auth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q, want the bearer key", auth)
	}
}

func TestCompleteRejectsAnIncompleteConfiguration(t *testing.T) {
	u := newUpstream(t, `{"text":"ok"}`)
	for name, cfg := range map[string]Config{
		"no base url": {Model: "m", APIKey: testAPIKey},
		"no model":    {BaseURL: u.server.URL, APIKey: testAPIKey},
		"no key":      {BaseURL: u.server.URL, Model: "m"},
	} {
		if _, err := complete(t.Context(), cfg, []message{{Role: "user", Content: "hi"}}, 10); !errors.Is(err, ErrUnconfigured) {
			t.Errorf("%s: err = %v, want unconfigured", name, err)
		}
	}
	if calls, _ := u.counts(); calls != 0 {
		t.Errorf("calls = %d, want 0: an unusable configuration must not reach the network", calls)
	}
}

func TestCompleteStopsAtTheTimeout(t *testing.T) {
	u := newUpstream(t, `{"text":"late"}`)
	u.mu.Lock()
	u.delay = 500 * time.Millisecond
	u.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := complete(ctx, u.config(), []message{{Role: "user", Content: "hi"}}, 10)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("complete took %s, want the caller's deadline", elapsed)
	}
}

func TestCompleteCapsTheResponseSize(t *testing.T) {
	u := newUpstream(t)
	u.mu.Lock()
	u.raw = strings.Repeat("a", maxResponseBytes+64)
	u.mu.Unlock()

	_, err := complete(t.Context(), u.config(), []message{{Role: "user", Content: "hi"}}, 10)
	if !errors.Is(err, ErrInvalidReply) {
		t.Fatalf("err = %v, want invalid reply", err)
	}
}

func TestCompleteRedactsUpstreamFailures(t *testing.T) {
	const body = `{"error":"sk-live-secret leaked in the body"}`
	u := newUpstream(t)
	u.mu.Lock()
	u.status = http.StatusInternalServerError
	u.raw = body
	u.mu.Unlock()

	_, err := complete(t.Context(), u.config(), []message{{Role: "user", Content: "a private prompt"}}, 10)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want upstream", err)
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v, want the upstream status 500", err)
	}
	for _, secret := range []string{"sk-live-secret", "leaked", "a private prompt", testAPIKey} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q carries %q", err.Error(), secret)
		}
	}
	if Code(err) != CodeUpstream {
		t.Errorf("code = %q, want %q", Code(err), CodeUpstream)
	}
}

func TestCompleteRejectsMalformedReplies(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":      `not-json`,
		"no choices":    `{"choices":[]}`,
		"empty content": `{"choices":[{"message":{"content":"  "}}]}`,
	} {
		u := newUpstream(t)
		u.mu.Lock()
		u.raw = raw
		u.mu.Unlock()
		if _, err := complete(t.Context(), u.config(), []message{{Role: "user", Content: "hi"}}, 10); !errors.Is(err, ErrInvalidReply) {
			t.Errorf("%s: err = %v, want invalid reply", name, err)
		}
	}
}

func TestCompleteRejectsAnOversizedPrompt(t *testing.T) {
	u := newUpstream(t, `{"text":"ok"}`)
	_, err := complete(t.Context(), u.config(), []message{{Role: "user", Content: strings.Repeat("x", maxPromptBytes+1)}}, 10)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want invalid input", err)
	}
	if calls, _ := u.counts(); calls != 0 {
		t.Errorf("calls = %d, want 0", calls)
	}
}

func TestCompleteLimitsConcurrency(t *testing.T) {
	u := newUpstream(t, `{"text":"ok"}`)
	u.mu.Lock()
	u.delay = 40 * time.Millisecond
	u.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = complete(t.Context(), u.config(), []message{{Role: "user", Content: "hi"}}, 10)
		}()
	}
	wg.Wait()

	calls, maxActive := u.counts()
	if calls != 6 {
		t.Fatalf("calls = %d, want 6", calls)
	}
	if maxActive > maxConcurrentCalls {
		t.Errorf("max concurrent calls = %d, want at most %d", maxActive, maxConcurrentCalls)
	}
}

// ---------- author status ----------

func TestDecideServesAFreshCache(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	insertPost(t, db, "a", "article", "published", "标题", "正文", stamp(now.Add(-48*time.Hour)))
	seedCache(t, db, "旧状态", []string{"随笔"}, stamp(now.Add(-48*time.Hour)), now.Add(-time.Hour), now.Add(time.Hour))

	status, refresh, err := Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if refresh {
		t.Error("a fresh cache must not ask for a refresh")
	}
	if status.Text != "旧状态" || status.Stale || status.Default {
		t.Errorf("status = %+v, want the cached value and no stale flag", status)
	}
	if len(status.Topics) != 1 || status.Topics[0] != "随笔" {
		t.Errorf("topics = %v, want [随笔]", status.Topics)
	}
}

func TestDecideReusesAnExpiredCacheWhenTheContentIsUnchanged(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	revision := stamp(now.Add(-72 * time.Hour))
	insertPost(t, db, "a", "article", "published", "标题", "正文", revision)
	seedCache(t, db, "旧状态", nil, revision, now.Add(-48*time.Hour), now.Add(-time.Hour))

	status, refresh, err := Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if refresh {
		t.Error("expired but unchanged content must not trigger a call")
	}
	if status.Text != "旧状态" || !status.Stale {
		t.Errorf("status = %+v, want the old value marked stale", status)
	}
}

func TestDecideAsksForARefreshWhenContentIsNewer(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	old := stamp(now.Add(-72 * time.Hour))
	insertPost(t, db, "a", "article", "published", "标题", "正文", old)
	seedCache(t, db, "旧状态", nil, old, now.Add(-48*time.Hour), now.Add(-time.Hour))
	insertPost(t, db, "b", "moment", "published", "", "新动态", stamp(now.Add(-time.Hour)))

	status, refresh, err := Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !refresh {
		t.Error("newer published content must ask for a refresh")
	}
	if status.Text != "旧状态" || !status.Stale {
		t.Errorf("status = %+v, want the old value until the refresh lands", status)
	}
}

func TestDecideReusesTheCacheWhenNothingIsPublished(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	insertPost(t, db, "draft", "article", "draft", "草稿", "未发布", stamp(now))
	seedCache(t, db, "旧状态", nil, stamp(now.Add(-96*time.Hour)), now.Add(-48*time.Hour), now.Add(-time.Hour))

	status, refresh, err := Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if refresh {
		t.Error("nothing published means nothing to summarize")
	}
	if status.Text != "旧状态" {
		t.Errorf("text = %q, want the cached value", status.Text)
	}
}

func TestDecideFallsBackWithoutACache(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	status, refresh, err := Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if refresh {
		t.Error("an empty site must not ask for a refresh")
	}
	if !status.Default || status.Text != fallbackText {
		t.Errorf("status = %+v, want the handwritten fallback", status)
	}

	insertPost(t, db, "a", "article", "published", "标题", "正文", stamp(now))
	status, refresh, err = Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !refresh {
		t.Error("published content without a cache must ask for a refresh")
	}
	if !status.Default {
		t.Errorf("status = %+v, want the fallback to render immediately", status)
	}
}

func TestDecideKeepsTheFallbackWhenTheStoredValueIsUnusable(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO ai_cache(cache_key, value_json, source_updated_at, generated_at, expires_at)
		VALUES(?, 'not-json', ?, ?, ?)`, StatusCacheKey, stamp(now), stamp(now), stamp(now.Add(time.Hour))); err != nil {
		t.Fatalf("seed broken cache: %v", err)
	}

	status, refresh, err := Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !status.Default {
		t.Errorf("status = %+v, want the fallback", status)
	}
	if refresh {
		t.Error("no published content exists, so no refresh is wanted")
	}
}

func TestRefreshStoresABoundedStatus(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	insertPost(t, db, "a", "article", "published", "标题", "正文", stamp(now.Add(-2*time.Hour)))
	u := newUpstream(t, `{"text":"`+strings.Repeat("好", 400)+`","topics":["a","A","b","c","d","e","f"]}`)

	status, err := Refresh(t.Context(), db, u.config(), testLogger(), now)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := len([]rune(status.Text)); got != maxStatusRunes {
		t.Errorf("text runes = %d, want %d", got, maxStatusRunes)
	}
	if len(status.Topics) != maxStatusTopics {
		t.Errorf("topics = %v, want at most %d", status.Topics, maxStatusTopics)
	}
	for i, topic := range status.Topics {
		if topic != []string{"a", "b", "c", "d", "e"}[i] {
			t.Errorf("topics = %v, want the case-insensitive dedupe to keep the first spelling", status.Topics)
			break
		}
	}

	value, sourceUpdatedAt, lastError, found := readCacheRow(t, db)
	if !found {
		t.Fatal("refresh did not store a row")
	}
	if lastError != "" {
		t.Errorf("last_error = %q, want empty", lastError)
	}
	if sourceUpdatedAt != stamp(now.Add(-2*time.Hour)) {
		t.Errorf("source_updated_at = %q, want the newest published revision", sourceUpdatedAt)
	}
	if !strings.Contains(value, `"text"`) || !strings.Contains(value, `"topics"`) {
		t.Errorf("stored value = %q, want the text and topics fields", value)
	}

	// A second visit now reuses the stored value without another call.
	cached, refresh, err := Decide(t.Context(), db, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if refresh || cached.Text != status.Text || cached.Default {
		t.Errorf("status = %+v refresh = %v, want the stored value", cached, refresh)
	}
}

func TestRefreshKeepsThePreviousValueOnFailure(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	insertPost(t, db, "a", "article", "published", "标题", "正文", stamp(now))
	seedCache(t, db, "旧状态", nil, stamp(now.Add(-72*time.Hour)), now.Add(-48*time.Hour), now.Add(-time.Hour))
	u := newUpstream(t)
	u.mu.Lock()
	u.status = http.StatusBadGateway
	u.raw = `{"error":"upstream exploded"}`
	u.mu.Unlock()

	if _, err := Refresh(t.Context(), db, u.config(), testLogger(), now); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want upstream", err)
	}
	value, sourceUpdatedAt, lastError, found := readCacheRow(t, db)
	if !found {
		t.Fatal("the previous row must survive a failed refresh")
	}
	if !strings.Contains(value, "旧状态") || sourceUpdatedAt != stamp(now.Add(-72*time.Hour)) {
		t.Errorf("value = %q source = %q, want the previous value untouched", value, sourceUpdatedAt)
	}
	if lastError != CodeUpstream {
		t.Errorf("last_error = %q, want %q", lastError, CodeUpstream)
	}

	// The read path still serves the old value and can ask for a retry.
	status, refresh, err := Decide(t.Context(), db, now)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if status.Text != "旧状态" || !status.Stale || !refresh {
		t.Errorf("status = %+v refresh = %v, want the previous value with a retry", status, refresh)
	}
}

func TestRefreshRecordsTheCodeOfAnInvalidReply(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	insertPost(t, db, "a", "article", "published", "标题", "正文", stamp(now))
	seedCache(t, db, "旧状态", nil, stamp(now), now.Add(-time.Hour), now.Add(time.Hour))
	u := newUpstream(t, `{"text":"ok","extra":true}`)

	if _, err := Refresh(t.Context(), db, u.config(), testLogger(), now); !errors.Is(err, ErrInvalidReply) {
		t.Fatalf("err = %v, want invalid reply", err)
	}
	if _, _, lastError, _ := readCacheRow(t, db); lastError != CodeInvalidReply {
		t.Errorf("last_error = %q, want %q", lastError, CodeInvalidReply)
	}
}

func TestRefreshRejectsAnEmptySite(t *testing.T) {
	db := testDB(t)
	u := newUpstream(t, `{"text":"ok"}`)
	if _, err := Refresh(t.Context(), db, u.config(), testLogger(), time.Now()); !errors.Is(err, ErrNoContent) {
		t.Fatalf("err = %v, want no content", err)
	}
	if calls, _ := u.counts(); calls != 0 {
		t.Errorf("calls = %d, want 0", calls)
	}
	if _, _, _, found := readCacheRow(t, db); found {
		t.Error("a failed refresh must not create a row")
	}
}

func TestRefreshBoundsTheSnapshot(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	insertPost(t, db, "draft", "article", "draft", "草稿标题", "草稿正文", stamp(now))
	insertPost(t, db, "gone", "article", "published", "已删除标题", "已删除正文", stamp(now))
	if _, err := db.Exec(`UPDATE posts SET deleted_at = ? WHERE slug = 'gone'`, stamp(now)); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	for i := 0; i < 40; i++ {
		insertPost(t, db, fmt.Sprintf("p%02d", i), "article", "published", fmt.Sprintf("标题%d", i),
			strings.Repeat("正", 1500), stamp(now.Add(-time.Duration(i)*time.Hour)))
	}
	u := newUpstream(t, `{"text":"状态","topics":["a"]}`)

	if _, err := Refresh(t.Context(), db, u.config(), testLogger(), now); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	_, prompts, _ := u.requests()
	if len(prompts) == 0 {
		t.Fatal("no prompt was sent")
	}
	prompt := strings.Join(prompts, "\n")
	if len(prompt) > maxSnapshotBytes+2<<10 {
		t.Errorf("prompt = %d bytes, want the snapshot budget plus the template", len(prompt))
	}
	if strings.Contains(prompt, "草稿标题") || strings.Contains(prompt, "已删除标题") {
		t.Error("the snapshot must contain published, undeleted content only")
	}
	if got := strings.Count(prompt, "\n- ["); got > maxSnapshotPosts {
		t.Errorf("snapshot holds %d posts, want at most %d", got, maxSnapshotPosts)
	}
}

func TestGuardIsSingleFlight(t *testing.T) {
	var guard Guard
	if !guard.TryStart() {
		t.Fatal("the first claim must succeed")
	}
	if guard.TryStart() {
		t.Error("a second claim must be refused while one is running")
	}
	guard.Finish()
	if !guard.TryStart() {
		t.Error("the slot must be free again after Finish")
	}
	guard.Finish()
}

func TestRefreshInBackgroundIsSingleFlight(t *testing.T) {
	db := testDB(t)
	now := time.Now()
	insertPost(t, db, "a", "article", "published", "标题", "正文", stamp(now))
	u := newUpstream(t, `{"text":"后台状态","topics":["随笔"]}`)
	u.mu.Lock()
	u.delay = 60 * time.Millisecond
	u.mu.Unlock()

	var guard Guard
	started := 0
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if guard.RefreshInBackground(db, u.config(), testLogger()) {
				started++
			}
		}()
	}
	wg.Wait()
	if started != 1 {
		t.Errorf("started = %d, want exactly one refresh", started)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if value, _, _, found := readCacheRow(t, db); found && strings.Contains(value, "后台状态") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if value, _, _, found := readCacheRow(t, db); !found || !strings.Contains(value, "后台状态") {
		t.Fatalf("the background refresh did not store its result: %q", value)
	}
	if calls, _ := u.counts(); calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// ---------- writing assistant ----------

func TestAssistBoundsEverySuggestion(t *testing.T) {
	u := newUpstream(t,
		`{"summary":"`+strings.Repeat("好", 400)+`"}`,
		`{"tags":["Go","go","  ","超长标签`+strings.Repeat("长", 40)+`","a","b","c","d","e","f"]}`,
		`{"title":"`+strings.Repeat("题", 200)+`","description":"`+strings.Repeat("述", 400)+`"}`,
	)
	cfg := u.config()
	draft := Draft{Title: "标题", Body: "正文"}

	summary, err := Assist(t.Context(), cfg, ActionSummary, draft)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got := len([]rune(summary.Summary)); got != maxSummaryRunes {
		t.Errorf("summary runes = %d, want %d", got, maxSummaryRunes)
	}

	tags, err := Assist(t.Context(), cfg, ActionTags, draft)
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	if len(tags.Tags) != maxTags {
		t.Errorf("tags = %v, want at most %d", tags.Tags, maxTags)
	}
	for _, tag := range tags.Tags {
		if tag == "" || len([]rune(tag)) > maxTagRunes {
			t.Errorf("tag %q is out of bounds", tag)
		}
	}
	if tags.Tags[0] != "Go" || tags.Tags[1] == "go" {
		t.Errorf("tags = %v, want a case-insensitive dedupe that keeps the first spelling", tags.Tags)
	}

	seo, err := Assist(t.Context(), cfg, ActionSEO, draft)
	if err != nil {
		t.Fatalf("seo: %v", err)
	}
	if len([]rune(seo.SEOTitle)) != maxSEOTitleRunes || len([]rune(seo.SEODescription)) != maxSEODescriptionRunes {
		t.Errorf("seo = %+v, want both fields truncated to their bounds", seo)
	}
	if summary.Action != ActionSummary || tags.Action != ActionTags || seo.Action != ActionSEO {
		t.Error("a suggestion must report the action it answered")
	}
}

func TestAssistRejectsUnusableDraftsBeforeCalling(t *testing.T) {
	u := newUpstream(t, `{"summary":"ok"}`)
	cfg := u.config()

	for name, draft := range map[string]Draft{
		"empty":     {},
		"blank":     {Title: "  ", Body: "\n"},
		"oversized": {Body: strings.Repeat("x", MaxInputBytes+1)},
	} {
		if _, err := Assist(t.Context(), cfg, ActionSummary, draft); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want invalid input", name, err)
		}
	}
	if _, err := Assist(t.Context(), cfg, "chat", Draft{Body: "正文"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown action: err = %v, want invalid input", err)
	}
	if calls, _ := u.counts(); calls != 0 {
		t.Errorf("calls = %d, want 0", calls)
	}
}

func TestAssistRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for name, reply := range map[string]string{
		"unknown field": `{"summary":"ok","post_id":7}`,
		"trailing json": `{"summary":"ok"}{"summary":"again"}`,
		"array":         `["ok"]`,
	} {
		u := newUpstream(t, reply)
		if _, err := Assist(t.Context(), u.config(), ActionSummary, Draft{Body: "正文"}); !errors.Is(err, ErrInvalidReply) {
			t.Errorf("%s: err = %v, want invalid reply", name, err)
		}
	}
}

func TestAssistPromptCarriesTheDraftAndTheAction(t *testing.T) {
	u := newUpstream(t, `{"tags":["随笔"]}`)
	draft := Draft{Title: "标题", Body: "正文内容", Excerpt: "摘要", Tags: []string{"旧标签"}}
	if _, err := Assist(t.Context(), u.config(), ActionTags, draft); err != nil {
		t.Fatalf("Assist: %v", err)
	}
	_, prompts, _ := u.requests()
	prompt := strings.Join(prompts, "\n")
	for _, want := range []string{"标题", "正文内容", "摘要", "旧标签", "tags"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not mention %q: %s", want, prompt)
		}
	}
}
