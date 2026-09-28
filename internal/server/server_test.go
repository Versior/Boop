package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boop/internal/config"
)

func testConfig() config.Config {
	return config.Config{
		Addr:        "127.0.0.1:8080",
		DataDir:     "./data",
		BaseURL:     "http://localhost:8080",
		MaxUploadMB: 10,
		LogLevel:    "error",
	}
}

func testServer(t *testing.T, logger *slog.Logger) *server {
	t.Helper()
	srv, err := newServer(testConfig(), nil, logger)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return srv
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func do(t *testing.T, handler http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHealthzReportsLiveness(t *testing.T) {
	rec := do(t, testServer(t, discardLogger()).handler(), http.MethodGet, "/healthz", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok\n" {
		t.Errorf("body = %q, want %q", got, "ok\n")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	handler := testServer(t, discardLogger()).handler()

	for _, target := range []string{"/healthz", "/", "/missing-page", "/api/v1/posts"} {
		rec := do(t, handler, http.MethodGet, target, nil)
		t.Run(target, func(t *testing.T) {
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			csp := rec.Header().Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("CSP %q missing %q", csp, want)
				}
			}
			if strings.Contains(csp, "unsafe-inline") {
				t.Errorf("CSP %q allows inline script/style", csp)
			}
			if got := rec.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
				t.Errorf("Referrer-Policy = %q", got)
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want DENY", got)
			}
		})
	}
}

func TestRequestIDIsGeneratedAndUnique(t *testing.T) {
	handler := testServer(t, discardLogger()).handler()

	first := do(t, handler, http.MethodGet, "/healthz", nil).Header().Get("X-Request-ID")
	second := do(t, handler, http.MethodGet, "/healthz", nil).Header().Get("X-Request-ID")

	if len(first) != 32 {
		t.Fatalf("X-Request-ID = %q, want 32 hex characters", first)
	}
	if first == second {
		t.Errorf("two requests shared request id %q", first)
	}
}

func TestRequestIDAppearsInErrorBodies(t *testing.T) {
	handler := testServer(t, discardLogger()).handler()

	api := do(t, handler, http.MethodGet, "/api/v1/nope", nil)
	if api.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", api.Code)
	}
	if got := api.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON", got)
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(api.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if payload.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want not_found", payload.Error.Code)
	}
	if payload.RequestID == "" || payload.RequestID != api.Header().Get("X-Request-ID") {
		t.Errorf("request_id = %q, header = %q", payload.RequestID, api.Header().Get("X-Request-ID"))
	}

	page := do(t, handler, http.MethodGet, "/missing-page", nil)
	if page.Code != http.StatusNotFound {
		t.Fatalf("page status = %d, want 404", page.Code)
	}
	if got := page.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("page Content-Type = %q, want HTML", got)
	}
	if !strings.Contains(page.Body.String(), page.Header().Get("X-Request-ID")) {
		t.Error("HTML 404 body does not contain the request id")
	}
}

func TestHomeRendersEmbeddedShell(t *testing.T) {
	rec := do(t, testServer(t, discardLogger()).handler(), http.MethodGet, "/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<html lang="zh-CN"`,
		`class="app"`,
		`class="rail-left"`,
		`class="rail-right"`,
		`class="bottom-nav"`,
		`class="topbar"`,
		`<link rel="stylesheet" href="/static/app.css">`,
		`<script src="/static/app.js"></script>`,
		`action="/search"`,
		`href="/?type=article"`,
		`href="/?type=photo"`,
		`href="/bookmarks"`,
		`href="/login"`,
		`data-theme-toggle`,
		`aria-label="主导航"`,
		`aria-label="移动端导航"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home page is missing %s", want)
		}
	}
	if strings.Contains(body, "unsafe-inline") {
		t.Error("unexpected inline marker in rendered page")
	}
	// 左栏主导航与移动端底部导航各标记一次当前项。
	if got := strings.Count(body, `aria-current="page"`); got != 2 {
		t.Errorf("aria-current occurrences = %d, want 2", got)
	}
}

func TestHomeMarksActiveFilter(t *testing.T) {
	handler := testServer(t, discardLogger()).handler()

	for _, tt := range []struct {
		target string
		active string
	}{
		{"/", "首页"},
		{"/?type=article", "文章"},
		{"/?type=photo", "摄影"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			body := do(t, handler, http.MethodGet, tt.target, nil).Body.String()
			if got := strings.Count(body, `aria-current="page"`); got != 2 {
				t.Fatalf("%s marks %d active navigation items, want 2", tt.target, got)
			}
			if !strings.Contains(body, tt.active) {
				t.Errorf("%s does not render the %s label", tt.target, tt.active)
			}
		})
	}

	if rec := do(t, handler, http.MethodGet, "/?type=video", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("status for unknown type = %d, want 400", rec.Code)
	}
}

func TestStaticAssetsServeDeterministicContentTypes(t *testing.T) {
	handler := testServer(t, discardLogger()).handler()

	css := do(t, handler, http.MethodGet, "/static/app.css", nil)
	if css.Code != http.StatusOK {
		t.Fatalf("app.css status = %d", css.Code)
	}
	if got := css.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Errorf("app.css Content-Type = %q", got)
	}
	if !strings.Contains(css.Body.String(), "--accent") {
		t.Error("app.css body does not contain the design tokens")
	}

	js := do(t, handler, http.MethodGet, "/static/app.js", nil)
	if js.Code != http.StatusOK {
		t.Fatalf("app.js status = %d", js.Code)
	}
	if got := js.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("app.js Content-Type = %q", got)
	}

	if rec := do(t, handler, http.MethodGet, "/static/missing.css", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset status = %d, want 404", rec.Code)
	}
}

func TestStaticHandlerRejectsTraversal(t *testing.T) {
	srv := testServer(t, discardLogger())

	for _, path := range []string{"/static/../go.mod", "/static/..%2fgo.mod", "/static/", "/static"} {
		req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
		req.URL.Path = path
		rec := httptest.NewRecorder()
		srv.handleStatic(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q status = %d, want 404", path, rec.Code)
		}
	}
}

func TestRecoveryTurnsPanicIntoInternalError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	rec := do(t, withRequestID(securityHeaders(recoverPanic(panicking, logger))), http.MethodGet, "/api/v1/posts", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"internal_error"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), "boom") {
		t.Errorf("panic was not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"request_id"`) {
		t.Errorf("panic log lacks the request id: %s", logs.String())
	}
}

func TestRecoveryRendersHTMLForPages(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	rec := do(t, recoverPanic(panicking, discardLogger()), http.MethodGet, "/p/slug", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want HTML", got)
	}
}

func TestAccessLogWritesJSONAndSkipsHealthChecks(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := testServer(t, logger).handler()

	do(t, handler, http.MethodGet, "/healthz", nil)
	do(t, handler, http.MethodGet, "/readyz", nil)
	if logs.Len() != 0 {
		t.Errorf("health checks were logged: %s", logs.String())
	}

	do(t, handler, http.MethodGet, "/", nil)
	line := strings.TrimSpace(logs.String())
	if line == "" {
		t.Fatal("request to / was not logged")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, line)
	}
	for _, key := range []string{"msg", "method", "path", "status", "duration_ms", "bytes", "request_id", "remote_addr"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("log entry missing %q: %s", key, line)
		}
	}
	if entry["path"] != "/" || entry["method"] != http.MethodGet {
		t.Errorf("unexpected log entry: %s", line)
	}
	if status, ok := entry["status"].(float64); !ok || status != http.StatusOK {
		t.Errorf("status = %v, want 200", entry["status"])
	}
}

func TestAccessLogRecordsErrorStatus(t *testing.T) {
	var logs bytes.Buffer
	handler := testServer(t, slog.New(slog.NewJSONHandler(&logs, nil))).handler()

	do(t, handler, http.MethodGet, "/missing-page", nil)

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if status, _ := entry["status"].(float64); status != http.StatusNotFound {
		t.Errorf("status = %v, want 404", entry["status"])
	}
}

func TestBodyLimitAppliesToRequestBodies(t *testing.T) {
	const limit = 1024
	var readErr error
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := limitBody(probe, limit)

	body := bytes.Repeat([]byte("a"), limit+1)
	rec := do(t, handler, http.MethodPost, "/api/v1/admin/posts", bytes.NewReader(body))
	if readErr == nil {
		t.Fatal("handler read the oversized body without an error")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(readErr, &maxErr) {
		t.Fatalf("read error = %v (%T), want *http.MaxBytesError", readErr, readErr)
	}
	if maxErr.Limit != limit {
		t.Errorf("limit = %d, want %d", maxErr.Limit, limit)
	}
	_ = rec

	readErr = nil
	if rec := do(t, handler, http.MethodPost, "/api/v1/admin/posts", strings.NewReader("small")); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if readErr != nil {
		t.Fatalf("small body failed: %v", readErr)
	}
}

func TestNewRegistersEmbeddedPages(t *testing.T) {
	// The embedded templates must parse; newServer is the fail-fast boundary.
	srv, err := newServer(testConfig(), nil, discardLogger())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	if _, ok := srv.pages["home"]; !ok {
		t.Fatal("home page template was not registered")
	}
}
