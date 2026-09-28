package server

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"boop/internal/ai"
	"boop/internal/settings"
)

const aiTestKey = "sk-test-ai-key-value"

// fakeAI is a stand-in OpenAI-compatible endpoint. It records every call, can
// answer with queued contents, and can hold calls open so a test can prove that
// nothing on the request path waits for the model.
type fakeAI struct {
	server *httptest.Server

	mu      sync.Mutex
	replies []string
	status  int
	raw     string
	calls   []string
	gate    chan struct{}
	arrived chan struct{}
}

func newFakeAI(t *testing.T, replies ...string) *fakeAI {
	t.Helper()
	fake := &fakeAI{replies: replies, arrived: make(chan struct{}, 32)}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeAI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	var payload struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &payload)
	prompt := ""
	for _, item := range payload.Messages {
		prompt += item.Content
	}
	f.calls = append(f.calls, prompt)
	content := ""
	if len(f.replies) > 0 {
		content = f.replies[0]
		if len(f.replies) > 1 {
			f.replies = f.replies[1:]
		}
	}
	status, raw, gate := f.status, f.raw, f.gate
	f.mu.Unlock()

	select {
	case f.arrived <- struct{}{}:
	default:
	}
	if gate != nil {
		<-gate
	}
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

func (f *fakeAI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAI) prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// gateCalls holds every later call until release is called, so a test can prove
// that a request path does not wait for the model. Releasing twice is harmless.
func (f *fakeAI) gateCalls() func() {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.gate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
}

// aiFixture is the owner's browser plus a stand-in AI endpoint and a configured
// encrypted API key.
type aiFixture struct {
	*contentFixture
	fake *fakeAI
}

func newAIFixture(t *testing.T, replies ...string) *aiFixture {
	t.Helper()
	cfg := testConfig()
	cfg.MasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	base := newAuthFixtureWithConfig(t, cfg)
	owner := base.bootstrapOwner(t, "owner@example.com", "遇事开心")
	rec := base.login(t, "owner@example.com", authPassword, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner login: status = %d, body %s", rec.Code, rec.Body.String())
	}

	f := &aiFixture{
		contentFixture: &contentFixture{
			authFixture: base,
			owner:       owner,
			cookie:      sessionCookie(t, rec),
			csrf:        decodeData(t, rec)["csrf_token"].(string),
		},
		fake: newFakeAI(t, replies...),
	}
	if err := settings.Apply(t.Context(), f.db, settingsBox(t, base), settings.Update{
		Values: map[string]any{
			settings.KeyAIEnabled:              true,
			settings.KeyAIBaseURL:              f.fake.server.URL + "/v1",
			settings.KeyAIChatModel:            "test-model",
			settings.KeyAIAuthorStatusTTLHours: 168,
		},
		Secrets: map[string]string{settings.SecretKeyAIAPIKey: aiTestKey},
	}); err != nil {
		t.Fatalf("configure ai: %v", err)
	}
	return f
}

// publishArticle creates one published article, so the author status has content
// to summarize.
func (f *aiFixture) publishArticle(t *testing.T) map[string]any {
	t.Helper()
	return f.createOK(t, map[string]any{
		"type": "article", "status": "published", "title": "海边的下午",
		"body": "今天在海边坐了很久，风把云推得很快。", "excerpt": "海边的一天",
		"tags": []string{"随笔"},
	})
}

// allowManyAI raises the AI budget for tests that are not about rate limiting.
func (f *aiFixture) allowManyAI(t *testing.T) {
	t.Helper()
	f.srv.limiters.ai = newLimiter(10_000, time.Second, time.Now)
}

func (f *aiFixture) aiRequest(t *testing.T, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.post(t, target, body)
}

// waitForStatus polls until the stored author status contains want, or fails.
func (f *aiFixture) waitForStatus(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var value string
		err := f.db.QueryRow(`SELECT value_json FROM ai_cache WHERE cache_key = ?`, ai.StatusCacheKey).Scan(&value)
		if err == nil && strings.Contains(value, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("author status never contained %q", want)
}

// waitForStatus polls until the stored author status contains want, or fails.

// ---------- public author status ----------

func TestAuthorStatusAPIIsPublicAndFallsBack(t *testing.T) {
	f := newAuthFixture(t)
	rec := f.do(t, http.MethodGet, "/api/v1/ai/author-status", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := decodeData(t, rec)
	if data["default"] != true {
		t.Errorf("default = %v, want true", data["default"])
	}
	if text, _ := data["text"].(string); text == "" {
		t.Error("the fallback text must not be empty")
	}
	if data["stale"] != false {
		t.Errorf("stale = %v, want false", data["stale"])
	}
	if generatedAt, _ := data["generated_at"].(string); generatedAt != "" {
		t.Errorf("generated_at = %q, want empty for the handwritten fallback", generatedAt)
	}
}

func TestAuthorStatusAPIServesTheStoredValue(t *testing.T) {
	f := newAIFixture(t, `{"text":"我是站长，写代码也写海边笔记。","topics":["随笔","代码"]}`)
	f.allowManyAI(t)
	f.publishArticle(t)

	rec := f.aiRequest(t, "/api/v1/admin/ai/author-status/regenerate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := decodeData(t, rec)
	if data["text"] != "我是站长，写代码也写海边笔记。" || data["default"] != false {
		t.Errorf("regenerate data = %v, want the generated text", data)
	}
	if generatedAt, _ := data["generated_at"].(string); generatedAt == "" {
		t.Error("regenerate must report generated_at")
	}

	public := decodeData(t, f.do(t, http.MethodGet, "/api/v1/ai/author-status", "", nil, nil))
	if public["text"] != data["text"] {
		t.Errorf("public text = %v, want %v", public["text"], data["text"])
	}
	if public["default"] != false || public["stale"] != false {
		t.Errorf("public = %v, want a fresh stored value", public)
	}
	topics, ok := public["topics"].([]any)
	if !ok || len(topics) != 2 {
		t.Errorf("topics = %v, want the two generated topics", public["topics"])
	}
}

func TestHomePageNeverWaitsForTheModel(t *testing.T) {
	f := newAIFixture(t, `{"text":"后台生成的状态","topics":["随笔"]}`)
	f.publishArticle(t)
	release := f.fake.gateCalls()
	defer release()

	start := time.Now()
	rec := f.do(t, http.MethodGet, "/", "", nil, nil)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("home: status = %d", rec.Code)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("home took %s, want it to ignore the model", elapsed)
	}
	if !strings.Contains(rec.Body.String(), ai.FallbackStatus().Text) {
		t.Error("the home page must render the handwritten fallback while the refresh runs")
	}

	select {
	case <-f.fake.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("no background refresh reached the endpoint")
	}
	release()
	f.waitForStatus(t, "后台生成的状态")

	if calls := f.fake.callCount(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1", calls)
	}
	// The stored value is now what the card renders.
	page := f.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
	if !strings.Contains(page, "后台生成的状态") {
		t.Error("the generated status is not rendered on the home page")
	}
}

func TestConcurrentHomeVisitsStartOneRefresh(t *testing.T) {
	f := newAIFixture(t, `{"text":"只生成一次","topics":["随笔"]}`)
	f.publishArticle(t)
	release := f.fake.gateCalls()
	defer release()

	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			f.handler.ServeHTTP(rec, req)
			codes[index] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, code)
		}
	}

	select {
	case <-f.fake.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("no background refresh reached the endpoint")
	}
	release()
	f.waitForStatus(t, "只生成一次")
	if calls := f.fake.callCount(); calls != 1 {
		t.Errorf("upstream calls = %d, want exactly one refresh", calls)
	}
}

func TestCachedStatusSurvivesAnUnconfiguredSite(t *testing.T) {
	f := newAIFixture(t, `{"text":"缓存里的状态"}`)
	f.allowManyAI(t)
	f.publishArticle(t)
	if rec := f.aiRequest(t, "/api/v1/admin/ai/author-status/regenerate", ""); rec.Code != http.StatusOK {
		t.Fatalf("regenerate: status = %d: %s", rec.Code, rec.Body.String())
	}

	// Turning AI off must not hide what is already stored.
	f.updateSettings(t, settings.KeyAIEnabled, false)
	public := decodeData(t, f.do(t, http.MethodGet, "/api/v1/ai/author-status", "", nil, nil))
	if public["text"] != "缓存里的状态" {
		t.Errorf("public = %v, want the cached value", public)
	}
}

// ---------- owner authorization ----------

func TestAIRoutesRequireTheOwnerAndCSRF(t *testing.T) {
	f := newAIFixture(t, `{"text":"ok"}`)
	readerCookie, readerCSRF := f.readerFixture(t)

	routes := []string{
		"/api/v1/admin/ai/test",
		"/api/v1/admin/ai/author-status/regenerate",
		"/api/v1/admin/ai/assist",
	}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			guest := f.do(t, http.MethodPost, route, `{"action":"summary","body":"正文"}`,
				map[string]string{"Origin": testOrigin}, nil)
			if guest.Code != http.StatusUnauthorized {
				t.Errorf("guest: status = %d, want 401: %s", guest.Code, guest.Body.String())
			}
			reader := f.doHeaders(t, http.MethodPost, route, `{"action":"summary","body":"正文"}`,
				map[string]string{"X-CSRF-Token": readerCSRF}, readerCookie)
			if reader.Code != http.StatusForbidden {
				t.Errorf("reader: status = %d, want 403: %s", reader.Code, reader.Body.String())
			}
			noCSRF := f.doHeaders(t, http.MethodPost, route, `{"action":"summary","body":"正文"}`, nil, f.cookie)
			if noCSRF.Code != http.StatusForbidden {
				t.Errorf("missing CSRF: status = %d, want 403: %s", noCSRF.Code, noCSRF.Body.String())
			}
			if code, _, _ := decodeAPIError(t, noCSRF); code != "csrf_invalid" {
				t.Errorf("missing CSRF: code = %q, want csrf_invalid", code)
			}
		})
	}
	if calls := f.fake.callCount(); calls != 0 {
		t.Errorf("upstream calls = %d, want 0: a refused request must not reach the model", calls)
	}
}

func TestAIRoutesAreRateLimited(t *testing.T) {
	f := newAIFixture(t, `{"text":"ok"}`)
	for i := 0; i < aiBurst; i++ {
		rec := f.aiRequest(t, "/api/v1/admin/ai/test", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := f.aiRequest(t, "/api/v1/admin/ai/test", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
}

func TestAIReportsDisabledAndUnconfigured(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		f := newAIFixture(t, `{"text":"ok"}`)
		f.updateSettings(t, settings.KeyAIEnabled, false)
		rec := f.aiRequest(t, "/api/v1/admin/ai/test", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "ai_disabled" {
			t.Errorf("code = %q, want ai_disabled", code)
		}
	})

	t.Run("unconfigured", func(t *testing.T) {
		f := newAIFixture(t, `{"text":"ok"}`)
		f.updateSettings(t, settings.KeyAIEnabled, true)
		f.updateSettings(t, settings.KeyAIBaseURL, "https://api.example.com/v1")
		f.updateSettings(t, settings.KeyAIChatModel, "test-model")
		if err := settings.Apply(t.Context(), f.db, settingsBox(t, f.authFixture), settings.Update{
			Clear: []string{settings.SecretKeyAIAPIKey},
		}); err != nil {
			t.Fatalf("clear ai key: %v", err)
		}
		rec := f.aiRequest(t, "/api/v1/admin/ai/test", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "ai_unconfigured" {
			t.Errorf("code = %q, want ai_unconfigured", code)
		}
	})
}

// ---------- connectivity test ----------

func TestAITestReportsOnlyTheModel(t *testing.T) {
	f := newAIFixture(t, `pong`)
	rec := f.aiRequest(t, "/api/v1/admin/ai/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := decodeData(t, rec)
	if data["ok"] != true || data["model"] != "test-model" {
		t.Errorf("data = %v, want ok with the configured model", data)
	}
	// The reply and the key never appear in the response.
	for _, secret := range []string{"pong", aiTestKey} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the response leaks %q", secret)
		}
	}
	if calls := f.fake.callCount(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1", calls)
	}
}

// ---------- regenerate ----------

func TestRegenerateReportsBusyWhileARefreshRuns(t *testing.T) {
	f := newAIFixture(t, `{"text":"ok"}`)
	f.publishArticle(t)
	if !f.srv.aiRefresh.TryStart() {
		t.Fatal("the guard slot must be free")
	}
	defer f.srv.aiRefresh.Finish()

	rec := f.aiRequest(t, "/api/v1/admin/ai/author-status/regenerate", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if code, _, _ := decodeAPIError(t, rec); code != "ai_busy" {
		t.Errorf("code = %q, want ai_busy", code)
	}
	if calls := f.fake.callCount(); calls != 0 {
		t.Errorf("upstream calls = %d, want 0: a busy refresh must not start a second call", calls)
	}
}

func TestRegenerateReportsFailuresWithoutLeaking(t *testing.T) {
	f := newAIFixture(t, `{"error":"sk-live-leaked"}`)
	f.publishArticle(t)
	f.fake.mu.Lock()
	f.fake.status = http.StatusInternalServerError
	f.fake.raw = `{"error":"sk-live-secret in the body"}`
	f.fake.mu.Unlock()

	rec := f.aiRequest(t, "/api/v1/admin/ai/author-status/regenerate", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if code, _, _ := decodeAPIError(t, rec); code != "ai_upstream_error" {
		t.Errorf("code = %q, want ai_upstream_error", code)
	}
	for _, secret := range []string{"sk-live-secret", aiTestKey} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the response leaks %q", secret)
		}
	}
	var value, lastError string
	err := f.db.QueryRow(`SELECT value_json, last_error FROM ai_cache WHERE cache_key = ?`,
		ai.StatusCacheKey).Scan(&value, &lastError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read ai_cache: %v", err)
	}
	if strings.Contains(value, "sk-live") || strings.Contains(lastError, "sk-live") {
		t.Errorf("ai_cache holds upstream text: value=%q last_error=%q", value, lastError)
	}
}

func TestRegenerateWithoutContentIsRejected(t *testing.T) {
	f := newAIFixture(t, `{"text":"ok"}`)
	rec := f.aiRequest(t, "/api/v1/admin/ai/author-status/regenerate", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if code, _, _ := decodeAPIError(t, rec); code != "ai_no_content" {
		t.Errorf("code = %q, want ai_no_content", code)
	}
}

// ---------- writing assistant ----------

func TestAssistReturnsBoundedSuggestions(t *testing.T) {
	f := newAIFixture(t,
		`{"summary":"这是一段摘要。"}`,
		`{"tags":["Go","go","随笔"]}`,
		`{"title":"SEO 标题","description":"SEO 描述"}`,
	)
	f.allowManyAI(t)

	steps := []struct {
		action string
		field  string
		want   any
	}{
		{"summary", "summary", "这是一段摘要。"},
		{"tags", "tags", nil},
		{"seo", "seo_title", "SEO 标题"},
	}
	for _, step := range steps {
		payload := marshalJSON(t, map[string]any{"action": step.action, "body": "正文内容", "title": "标题"})
		rec := f.aiRequest(t, "/api/v1/admin/ai/assist", payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", step.action, rec.Code, rec.Body.String())
		}
		data := decodeData(t, rec)
		if data["action"] != step.action {
			t.Errorf("%s: action = %v, want %v", step.action, data["action"], step.action)
		}
		if step.want != nil && data[step.field] != step.want {
			t.Errorf("%s: %s = %v, want %v", step.action, step.field, data[step.field], step.want)
		}
		if step.action == "tags" {
			tags, _ := data["tags"].([]any)
			if len(tags) != 2 {
				t.Errorf("tags = %v, want the case-insensitive dedupe", data["tags"])
			}
		}
	}
	if !strings.Contains(strings.Join(f.fake.prompts(), "\n"), "正文内容") {
		t.Error("the draft never reached the model")
	}
}

func TestAssistRejectsBadRequests(t *testing.T) {
	f := newAIFixture(t, `{"summary":"ok"}`)
	f.allowManyAI(t)

	for name, body := range map[string]string{
		"empty draft":    marshalJSON(t, map[string]any{"action": "summary"}),
		"unknown action": marshalJSON(t, map[string]any{"action": "chat", "body": "正文"}),
		"oversized draft": marshalJSON(t, map[string]any{
			"action": "summary", "body": strings.Repeat("x", ai.MaxInputBytes+1),
		}),
		"unknown field": marshalJSON(t, map[string]any{"action": "summary", "body": "正文", "post_id": 7}),
	} {
		rec := f.aiRequest(t, "/api/v1/admin/ai/assist", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
			continue
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_body" && code != "invalid_action" {
			t.Errorf("%s: code = %q, want invalid_body or invalid_action", name, code)
		}
	}
	if calls := f.fake.callCount(); calls != 0 {
		t.Errorf("upstream calls = %d, want 0", calls)
	}
}

func TestAssistNeverTouchesStoredContent(t *testing.T) {
	f := newAIFixture(t, `{"summary":"建议摘要"}`)
	f.allowManyAI(t)
	post := f.publishArticle(t)

	payload := marshalJSON(t, map[string]any{
		"action": "summary", "title": "海边的下午", "body": "今天在海边坐了很久，风把云推得很快。",
	})
	if rec := f.aiRequest(t, "/api/v1/admin/ai/assist", payload); rec.Code != http.StatusOK {
		t.Fatalf("assist: status = %d: %s", rec.Code, rec.Body.String())
	}

	slug, _ := post["slug"].(string)
	rec := f.do(t, http.MethodGet, "/api/v1/posts/"+slug, "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read post: status = %d", rec.Code)
	}
	data := decodeData(t, rec)
	if data["body"] != "今天在海边坐了很久，风把云推得很快。" {
		t.Errorf("body = %v, want the stored draft untouched", data["body"])
	}
	if data["excerpt"] != "海边的一天" {
		t.Errorf("excerpt = %v, want the stored excerpt untouched", data["excerpt"])
	}
	if tags, _ := data["tags"].([]any); len(tags) != 1 || tags[0] != "随笔" {
		t.Errorf("tags = %v, want the stored tags untouched", data["tags"])
	}
}

// ---------- server wiring ----------

func TestHomeRendersTheAuthorStatusCard(t *testing.T) {
	f := newAIFixture(t, `{"text":"缓存状态","topics":["随笔"]}`)
	f.allowManyAI(t)
	post := f.publishArticle(t)
	if rec := f.aiRequest(t, "/api/v1/admin/ai/author-status/regenerate", ""); rec.Code != http.StatusOK {
		t.Fatalf("regenerate: %s", rec.Body.String())
	}

	// A guest sees the same card: the status is public.
	guest := f.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
	if !strings.Contains(guest, "缓存状态") {
		t.Error("the author status card must render for guests too")
	}

	page := f.do(t, http.MethodGet, "/", "", nil, f.cookie).Body.String()
	if got := strings.Count(page, "AI 作者状态"); got != 2 {
		t.Fatalf("the status card appears %d times, want the right rail and the mobile slot", got)
	}
	if got := strings.Count(page, "缓存状态"); got != 2 {
		t.Errorf("the status text appears %d times, want 2", got)
	}
	mobileSlot := strings.Index(page, `class="status-mobile"`)
	composer := strings.Index(page, "data-composer")
	if mobileSlot < 0 {
		t.Fatal("the mobile status slot is missing")
	}
	if composer < 0 || mobileSlot > composer {
		t.Error("on mobile the status card must render before the quick composer")
	}
	rail := strings.Index(page, `class="rail-right"`)
	if rail < 0 || !strings.Contains(page[rail:], "AI 作者状态") {
		t.Error("the desktop card must live in the right rail")
	}

	// Other pages carry no author status and no AI database work.
	detail := f.do(t, http.MethodGet, "/p/"+post["slug"].(string), "", nil, nil).Body.String()
	if strings.Contains(detail, "AI 作者状态") || strings.Contains(detail, "缓存状态") {
		t.Error("the author status card must not render outside the home page")
	}
	bookmarks := f.do(t, http.MethodGet, "/bookmarks", "", nil, f.cookie).Body.String()
	if strings.Contains(bookmarks, "AI 作者状态") {
		t.Error("the bookmarks page must not render the author status card")
	}
}

func TestComposerAssistantIsOwnerOnly(t *testing.T) {
	t.Run("owner with ai", func(t *testing.T) {
		f := newAIFixture(t)
		page := f.do(t, http.MethodGet, "/", "", nil, f.cookie).Body.String()
		if !strings.Contains(page, "data-ai-assist") {
			t.Error("the owner must get the writing assistant")
		}
		if !strings.Contains(page, `data-ai-action="summary"`) ||
			!strings.Contains(page, `data-ai-action="tags"`) ||
			!strings.Contains(page, `data-ai-action="seo"`) {
			t.Error("the assistant must offer summary, tags and seo")
		}
		if !strings.Contains(page, "data-ai-adopt") {
			t.Error("the assistant must offer an explicit adopt action")
		}
		composer := strings.Index(page, "data-composer")
		assistant := strings.Index(page, "data-ai-assist")
		stream := strings.Index(page, `id="stream"`)
		if assistant < composer || (stream >= 0 && assistant > stream) {
			t.Error("the assistant must stay inline inside the composer")
		}
	})

	t.Run("owner without ai", func(t *testing.T) {
		f := newAIFixture(t)
		f.updateSettings(t, settings.KeyAIEnabled, false)
		page := f.do(t, http.MethodGet, "/", "", nil, f.cookie).Body.String()
		if strings.Contains(page, "data-ai-assist") {
			t.Error("an unusable AI service must not render the assistant control")
		}
	})

	t.Run("reader", func(t *testing.T) {
		f := newAIFixture(t)
		readerCookie, _ := f.readerFixture(t)
		page := f.do(t, http.MethodGet, "/", "", nil, readerCookie).Body.String()
		if strings.Contains(page, "data-ai-assist") || strings.Contains(page, "data-composer") {
			t.Error("a reader must not receive the composer or the assistant")
		}
	})

	t.Run("guest", func(t *testing.T) {
		f := newAIFixture(t)
		page := f.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
		if strings.Contains(page, "data-ai-assist") {
			t.Error("a guest must not receive the assistant")
		}
	})
}

func TestAIWrongMethodsAndUnknownPathsStayJSON(t *testing.T) {
	f := newAIFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/ai/author-status", "", map[string]string{"Origin": testOrigin}, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
	if code, _, _ := decodeAPIError(t, rec); code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", code)
	}

	admin := f.do(t, http.MethodGet, "/api/v1/admin/ai/assist", "", map[string]string{"Origin": testOrigin}, nil)
	if admin.Code != http.StatusMethodNotAllowed {
		t.Fatalf("admin GET: status = %d, want 405: %s", admin.Code, admin.Body.String())
	}
	if code, _, _ := decodeAPIError(t, admin); code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed", code)
	}

	unknown := f.do(t, http.MethodGet, "/api/v1/ai/chat", "", nil, nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", unknown.Code)
	}
	if got := unknown.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
}
