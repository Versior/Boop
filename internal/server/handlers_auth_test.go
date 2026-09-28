package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boop/internal/auth"
	"boop/internal/config"
	"boop/internal/settings"
	"boop/internal/store"
)

const (
	testOrigin      = "http://localhost:8080"
	authPassword    = "correct horse battery"
	sessionCookieID = "boop_session"
)

type authFixture struct {
	srv     *server
	db      *sql.DB
	handler http.Handler
	cfg     config.Config
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	return newAuthFixtureWithConfig(t, testConfig())
}

func newAuthFixtureWithConfig(t *testing.T, cfg config.Config) *authFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	if err := settings.Seed(context.Background(), db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	srv, err := newServer(cfg, db, discardLogger())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return &authFixture{srv: srv, db: db, handler: srv.handler(), cfg: cfg}
}

// do issues a request with an explicit Origin header, body and cookie.
func (f *authFixture) do(t *testing.T, method, target, body string, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// postSameOrigin is the normal browser write: same-origin JSON body.
func (f *authFixture) postSameOrigin(t *testing.T, target, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	merged := map[string]string{"Origin": testOrigin}
	for key, value := range headers {
		merged[key] = value
	}
	return f.do(t, http.MethodPost, target, body, merged, cookie)
}

func (f *authFixture) register(t *testing.T, email, name string) *httptest.ResponseRecorder {
	t.Helper()
	return f.postSameOrigin(t, "/api/v1/auth/register",
		`{"email":"`+email+`","password":"`+authPassword+`","display_name":"`+name+`"}`, nil, nil)
}

func (f *authFixture) login(t *testing.T, email, password string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return f.loginWith(t, email, password, cookie, "")
}

// loginWith signs in with an explicit CSRF token, which an already signed-in
// browser must send because such a request is authenticated.
func (f *authFixture) loginWith(t *testing.T, email, password string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if csrf != "" {
		headers["X-CSRF-Token"] = csrf
	}
	return f.postSameOrigin(t, "/api/v1/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`, cookie, headers)
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response body %q is not JSON: %v", rec.Body.String(), err)
	}
	return payload
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	data, ok := decodeEnvelope(t, rec)["data"].(map[string]any)
	if !ok {
		t.Fatalf("response %q has no data object", rec.Body.String())
	}
	return data
}

func decodeAPIError(t *testing.T, rec *httptest.ResponseRecorder) (code, message, requestID string) {
	t.Helper()
	payload := decodeEnvelope(t, rec)
	envelope, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("response %q has no error object", rec.Body.String())
	}
	code, _ = envelope["code"].(string)
	message, _ = envelope["message"].(string)
	requestID, _ = payload["request_id"].(string)
	if code == "" || message == "" {
		t.Fatalf("error envelope %q is incomplete", rec.Body.String())
	}
	if requestID == "" {
		t.Fatalf("error envelope %q has no request_id", rec.Body.String())
	}
	return code, message, requestID
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookieID {
			return cookie
		}
	}
	t.Fatalf("response has no %s cookie: %v", sessionCookieID, rec.Header().Values("Set-Cookie"))
	return nil
}

func (f *authFixture) countRows(t *testing.T, table string) int {
	t.Helper()
	var count int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func (f *authFixture) bootstrapOwner(t *testing.T, email, name string) auth.User {
	t.Helper()
	owner, err := auth.BootstrapOwner(context.Background(), f.db, email, name, authPassword)
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	return owner
}

func TestRegisterCreatesReaderSessionAndCookie(t *testing.T) {
	f := newAuthFixture(t)
	rec := f.register(t, " Reader@Example.com ", " 读者甲 ")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
	}
	data := decodeData(t, rec)
	user, ok := data["user"].(map[string]any)
	if !ok {
		t.Fatalf("data has no user object: %v", data)
	}
	if user["role"] != auth.RoleReader {
		t.Errorf("role = %v, want %q", user["role"], auth.RoleReader)
	}
	if user["email"] != "reader@example.com" {
		t.Errorf("email = %v, want the normalized address", user["email"])
	}
	if user["display_name"] != "读者甲" {
		t.Errorf("display_name = %v, want the trimmed name", user["display_name"])
	}
	csrf, _ := data["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("data has no csrf_token")
	}
	if strings.Contains(rec.Body.String(), "$2") {
		t.Error("the password hash leaked into the response")
	}

	cookie := sessionCookie(t, rec)
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.Secure {
		t.Error("Secure = true while BOOP_SECURE_COOKIES defaults to false")
	}
	if cookie.Value == "" || cookie.MaxAge <= 0 {
		t.Errorf("cookie value/max-age = %q/%d, want a session token and a positive max-age", cookie.Value, cookie.MaxAge)
	}

	if users := f.countRows(t, "users"); users != 1 {
		t.Errorf("users = %d, want 1", users)
	}
	if sessions := f.countRows(t, "sessions"); sessions != 1 {
		t.Errorf("sessions = %d, want 1", sessions)
	}

	// The CSRF token returned at sign-in authorises unsafe requests.
	logout := f.postSameOrigin(t, "/api/v1/auth/logout", "", cookie, map[string]string{"X-CSRF-Token": csrf})
	if logout.Code != http.StatusOK {
		t.Fatalf("logout with the issued CSRF token: status = %d, body %s", logout.Code, logout.Body.String())
	}
}

func TestSessionCookieIsSecureWhenConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.SecureCookies = true
	f := newAuthFixtureWithConfig(t, cfg)

	rec := f.register(t, "reader@example.com", "读者甲")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if cookie := sessionCookie(t, rec); !cookie.Secure {
		t.Error("Secure = false while BOOP_SECURE_COOKIES is true")
	}
}

func TestRegisterRejectsDisabledRegistration(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	if err := settings.Set(ctx, f.db, settings.KeyAuthRegistrationEnabled, false); err != nil {
		t.Fatalf("settings.Set: %v", err)
	}

	rec := f.register(t, "reader@example.com", "读者甲")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	code, _, _ := decodeAPIError(t, rec)
	if code != "registration_disabled" {
		t.Errorf("code = %q, want registration_disabled", code)
	}
	if users := f.countRows(t, "users"); users != 0 {
		t.Errorf("users = %d, want none created", users)
	}

	// Signing in with an existing account still works while registration is off.
	f2 := newAuthFixture(t)
	f2.bootstrapOwner(t, "owner@example.com", "站长")
	if err := settings.Set(ctx, f2.db, settings.KeyAuthRegistrationEnabled, false); err != nil {
		t.Fatalf("settings.Set: %v", err)
	}
	if rec := f2.login(t, "owner@example.com", authPassword, nil); rec.Code != http.StatusOK {
		t.Errorf("owner login while registration is disabled: status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	f := newAuthFixture(t)
	if rec := f.register(t, "reader@example.com", "读者甲"); rec.Code != http.StatusCreated {
		t.Fatalf("first registration: status = %d", rec.Code)
	}

	for _, candidate := range []string{"reader@example.com", "READER@example.com"} {
		rec := f.register(t, candidate, "读者乙")
		if rec.Code != http.StatusConflict {
			t.Fatalf("second registration with %s: status = %d, want 409", candidate, rec.Code)
		}
		code, _, _ := decodeAPIError(t, rec)
		if code != "email_taken" {
			t.Errorf("code = %q, want email_taken", code)
		}
	}
	if users := f.countRows(t, "users"); users != 1 {
		t.Errorf("users = %d, want 1", users)
	}
}

func TestRegisterValidatesInput(t *testing.T) {
	valid := func(email, password, name string) string {
		return `{"email":"` + email + `","password":"` + password + `","display_name":"` + name + `"}`
	}
	tests := []struct {
		name string
		body string
		code string
	}{
		{"invalid email", valid("not-an-email", authPassword, "读者"), "invalid_email"},
		{"missing local part", valid("@example.com", authPassword, "读者"), "invalid_email"},
		{"password too short", valid("reader@example.com", strings.Repeat("a", 9), "读者"), "invalid_password"},
		{"password too long", valid("reader@example.com", strings.Repeat("a", 73), "读者"), "invalid_password"},
		{"empty password", valid("reader@example.com", "", "读者"), "invalid_password"},
		{"empty display name", valid("reader@example.com", authPassword, "   "), "invalid_display_name"},
		{"unknown field", `{"email":"reader@example.com","password":"` + authPassword + `","display_name":"读者","role":"owner"}`, "invalid_body"},
		{"not json", `email=reader@example.com`, "invalid_body"},
		{"trailing content", valid("reader@example.com", authPassword, "读者") + `{"email":"x@example.com"}`, "invalid_body"},
		{"empty body", "", "invalid_body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t)
			rec := f.postSameOrigin(t, "/api/v1/auth/register", tt.body, nil, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			code, _, _ := decodeAPIError(t, rec)
			if code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
			if users := f.countRows(t, "users"); users != 0 {
				t.Errorf("users = %d, want none created", users)
			}
		})
	}
}

func TestAuthWriteRejectsOversizedBody(t *testing.T) {
	cfg := testConfig()
	cfg.MaxUploadMB = 1
	f := newAuthFixtureWithConfig(t, cfg)

	huge := `{"email":"reader@example.com","password":"` + strings.Repeat("a", 1<<20) + `"}`
	rec := f.postSameOrigin(t, "/api/v1/auth/register", huge, nil, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	code, _, _ := decodeAPIError(t, rec)
	if code != "payload_too_large" {
		t.Errorf("code = %q, want payload_too_large", code)
	}
}

func TestUnsafeAuthRequestsRequireSameOriginEvidence(t *testing.T) {
	f := newAuthFixture(t)
	body := `{"email":"reader@example.com","password":"` + authPassword + `","display_name":"读者"}`

	noOrigin := f.do(t, http.MethodPost, "/api/v1/auth/register", body, nil, nil)
	if noOrigin.Code != http.StatusForbidden {
		t.Fatalf("request without Origin/Referer: status = %d, want 403", noOrigin.Code)
	}
	if code, _, _ := decodeAPIError(t, noOrigin); code != "origin_required" {
		t.Errorf("code = %q, want origin_required", code)
	}

	foreign := f.do(t, http.MethodPost, "/api/v1/auth/register", body,
		map[string]string{"Origin": "https://evil.example"}, nil)
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin: status = %d, want 403", foreign.Code)
	}
	if code, _, _ := decodeAPIError(t, foreign); code != "origin_mismatch" {
		t.Errorf("code = %q, want origin_mismatch", code)
	}

	badReferer := f.do(t, http.MethodPost, "/api/v1/auth/register", body,
		map[string]string{"Referer": "https://evil.example/login"}, nil)
	if badReferer.Code != http.StatusForbidden {
		t.Fatalf("foreign Referer: status = %d, want 403", badReferer.Code)
	}

	sameReferer := f.do(t, http.MethodPost, "/api/v1/auth/register", body,
		map[string]string{"Referer": testOrigin + "/register"}, nil)
	if sameReferer.Code != http.StatusCreated {
		t.Fatalf("same-origin Referer: status = %d, want 201: %s", sameReferer.Code, sameReferer.Body.String())
	}
	if users := f.countRows(t, "users"); users != 1 {
		t.Errorf("users = %d, want only the same-origin registration to succeed", users)
	}
}

func TestLoginRotatesSessionAndRejectsTheOldCookie(t *testing.T) {
	f := newAuthFixture(t)
	f.bootstrapOwner(t, "owner@example.com", "站长")

	first := f.login(t, "owner@example.com", authPassword, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("login: status = %d, body %s", first.Code, first.Body.String())
	}
	user, _ := decodeData(t, first)["user"].(map[string]any)
	if user["role"] != auth.RoleOwner {
		t.Errorf("role = %v, want %q", user["role"], auth.RoleOwner)
	}
	firstCookie := sessionCookie(t, first)
	firstCSRF, _ := decodeData(t, first)["csrf_token"].(string)

	me := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, firstCookie)
	if me.Code != http.StatusOK {
		t.Fatalf("me with the first session: status = %d", me.Code)
	}

	// Re-authenticating while already signed in is itself an unsafe request:
	// without the session's CSRF token it must not be able to rotate anything.
	silentSwap := f.login(t, "owner@example.com", authPassword, firstCookie)
	if silentSwap.Code != http.StatusForbidden {
		t.Fatalf("login without a CSRF token while signed in: status = %d, want 403", silentSwap.Code)
	}
	if sessions := f.countRows(t, "sessions"); sessions != 1 {
		t.Fatalf("sessions = %d, want the original session untouched", sessions)
	}

	second := f.loginWith(t, "owner@example.com", authPassword, firstCookie, firstCSRF)
	if second.Code != http.StatusOK {
		t.Fatalf("second login: status = %d, body %s", second.Code, second.Body.String())
	}
	secondCookie := sessionCookie(t, second)
	if secondCookie.Value == firstCookie.Value {
		t.Fatal("login reused the previous session token")
	}

	if sessions := f.countRows(t, "sessions"); sessions != 1 {
		t.Errorf("sessions = %d, want exactly one after rotation", sessions)
	}
	reused := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, firstCookie)
	if reused.Code != http.StatusUnauthorized {
		t.Fatalf("me with the rotated-away cookie: status = %d, want 401", reused.Code)
	}
	if fresh := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, secondCookie); fresh.Code != http.StatusOK {
		t.Errorf("me with the new cookie: status = %d, want 200", fresh.Code)
	}
}

func TestLoginFailuresShareOneShape(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	f.bootstrapOwner(t, "owner@example.com", "站长")
	if _, err := auth.CreateReader(ctx, f.db, "disabled@example.com", "停用者", authPassword); err != nil {
		t.Fatalf("CreateReader: %v", err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE email = ?`, "disabled@example.com"); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	attempts := map[string]struct{ email, password string }{
		"unknown email":  {"nobody@example.com", authPassword},
		"wrong password": {"owner@example.com", "wrong password value"},
		"disabled user":  {"disabled@example.com", authPassword},
	}

	shapes := make(map[string]string, len(attempts))
	for name, attempt := range attempts {
		t.Run(name, func(t *testing.T) {
			rec := f.login(t, attempt.email, attempt.password, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			code, message, _ := decodeAPIError(t, rec)
			if code != "invalid_credentials" {
				t.Errorf("code = %q, want invalid_credentials", code)
			}
			shapes[name] = code + "|" + message
			if len(rec.Result().Cookies()) != 0 {
				t.Error("a failed login set a cookie")
			}
		})
	}
	for name, shape := range shapes {
		if shape != shapes["unknown email"] {
			t.Errorf("%s failure shape %q differs from %q", name, shape, shapes["unknown email"])
		}
	}
	if sessions := f.countRows(t, "sessions"); sessions != 0 {
		t.Errorf("sessions = %d, want none after failed logins", sessions)
	}
}

func TestLoginValidatesInput(t *testing.T) {
	f := newAuthFixture(t)
	f.bootstrapOwner(t, "owner@example.com", "站长")

	for _, body := range []string{`{}`, `{"email":"owner@example.com"}`, `{"password":"` + authPassword + `"}`, ``, `{`} {
		rec := f.postSameOrigin(t, "/api/v1/auth/login", body, nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
			continue
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_body" {
			t.Errorf("body %q: code = %q, want invalid_body", body, code)
		}
	}
}

func TestMeReturnsUserAndCSRFToken(t *testing.T) {
	f := newAuthFixture(t)
	f.register(t, "reader@example.com", "读者甲")

	anon := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, nil)
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("guest status = %d, want 401", anon.Code)
	}
	if code, _, _ := decodeAPIError(t, anon); code != "unauthorized" {
		t.Errorf("guest code = %q, want unauthorized", code)
	}
}

func TestLogoutDeletesSessionAndClearsCookie(t *testing.T) {
	f := newAuthFixture(t)
	registered := f.register(t, "reader@example.com", "读者甲")
	cookie := sessionCookie(t, registered)
	csrf, _ := decodeData(t, registered)["csrf_token"].(string)

	rec := f.postSameOrigin(t, "/api/v1/auth/logout", "", cookie, map[string]string{"X-CSRF-Token": csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body %s", rec.Code, rec.Body.String())
	}
	data := decodeData(t, rec)
	if data["logged_out"] != true {
		t.Errorf("data = %v, want logged_out true", data)
	}
	cleared := sessionCookie(t, rec)
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("logout cookie = %q with max-age %d, want it cleared", cleared.Value, cleared.MaxAge)
	}
	if sessions := f.countRows(t, "sessions"); sessions != 0 {
		t.Errorf("sessions = %d, want 0 after logout", sessions)
	}

	after := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, cookie)
	if after.Code != http.StatusUnauthorized {
		t.Errorf("me after logout: status = %d, want 401", after.Code)
	}

	// Signing out again is harmless and stays JSON.
	again := f.postSameOrigin(t, "/api/v1/auth/logout", "", cookie, map[string]string{"X-CSRF-Token": csrf})
	if again.Code != http.StatusOK {
		t.Errorf("second logout: status = %d, want 200", again.Code)
	}
}

func TestUnsafeMethodsRequireCSRFToken(t *testing.T) {
	f := newAuthFixture(t)
	first := f.register(t, "reader@example.com", "读者甲")
	firstCookie := sessionCookie(t, first)
	firstCSRF, _ := decodeData(t, first)["csrf_token"].(string)

	second := f.register(t, "other@example.com", "读者乙")
	secondCSRF, _ := decodeData(t, second)["csrf_token"].(string)

	tests := []struct {
		name  string
		token string
		want  int
	}{
		{"missing token", "", http.StatusForbidden},
		{"wrong token", "not-the-token", http.StatusForbidden},
		{"another session's token", secondCSRF, http.StatusForbidden},
		{"correct token", firstCSRF, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{}
			if tt.token != "" {
				headers["X-CSRF-Token"] = tt.token
			}
			rec := f.postSameOrigin(t, "/api/v1/auth/logout", "", firstCookie, headers)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusForbidden {
				if code, _, _ := decodeAPIError(t, rec); code != "csrf_invalid" {
					t.Errorf("code = %q, want csrf_invalid", code)
				}
				if sessions := f.countRows(t, "sessions"); sessions != 2 {
					t.Fatalf("sessions = %d, want both sessions kept", sessions)
				}
			}
		})
	}
}

func TestAuthenticatedWritesStillCheckOrigin(t *testing.T) {
	f := newAuthFixture(t)
	registered := f.register(t, "reader@example.com", "读者甲")
	cookie := sessionCookie(t, registered)
	csrf, _ := decodeData(t, registered)["csrf_token"].(string)

	crossOrigin := f.do(t, http.MethodPost, "/api/v1/auth/logout", "",
		map[string]string{"Origin": "https://evil.example", "X-CSRF-Token": csrf}, cookie)
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout: status = %d, want 403", crossOrigin.Code)
	}
	if code, _, _ := decodeAPIError(t, crossOrigin); code != "origin_mismatch" {
		t.Errorf("code = %q, want origin_mismatch", code)
	}
	if sessions := f.countRows(t, "sessions"); sessions != 1 {
		t.Fatal("the cross-origin request destroyed the session")
	}

	// The CSRF token alone is enough when no Origin or Referer is present.
	noOrigin := f.do(t, http.MethodPost, "/api/v1/auth/logout", "",
		map[string]string{"X-CSRF-Token": csrf}, cookie)
	if noOrigin.Code != http.StatusOK {
		t.Fatalf("token-only logout: status = %d, body %s", noOrigin.Code, noOrigin.Body.String())
	}
}

func TestSafeMethodsSkipCSRF(t *testing.T) {
	f := newAuthFixture(t)
	registered := f.register(t, "reader@example.com", "读者甲")
	cookie := sessionCookie(t, registered)

	rec := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	data := decodeData(t, rec)
	if data["csrf_token"] == "" {
		t.Error("me did not return a CSRF token")
	}

	// HEAD is safe as well.
	if head := f.do(t, http.MethodHead, "/api/v1/auth/me", "", nil, cookie); head.Code != http.StatusOK {
		t.Errorf("HEAD status = %d, want 200", head.Code)
	}
}

func TestDisabledUserSessionIsRejected(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	registered := f.register(t, "reader@example.com", "读者甲")
	cookie := sessionCookie(t, registered)

	if _, err := f.db.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE email = ?`, "reader@example.com"); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	rec := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code, _, _ := decodeAPIError(t, rec); code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", code)
	}
	if sessions := f.countRows(t, "sessions"); sessions != 0 {
		t.Errorf("sessions = %d, want the disabled user's session removed", sessions)
	}
	if cleared := sessionCookie(t, rec); cleared.Value != "" {
		t.Error("the rejected cookie was not cleared")
	}
}

func TestRegistrationCannotEscalateToOwner(t *testing.T) {
	f := newAuthFixture(t)
	rec := f.postSameOrigin(t, "/api/v1/auth/register",
		`{"email":"reader@example.com","password":"`+authPassword+`","display_name":"读者","role":"owner"}`, nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var owners int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM users WHERE role = 'owner'`).Scan(&owners); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if owners != 0 {
		t.Fatal("public registration created an owner account")
	}
}

func TestOwnerAndReaderRolesStayIsolated(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	f.bootstrapOwner(t, "owner@example.com", "站长")
	if _, err := auth.CreateReader(ctx, f.db, "reader@example.com", "读者甲", authPassword); err != nil {
		t.Fatalf("CreateReader: %v", err)
	}

	roles := map[string]string{}
	for _, account := range []string{"owner@example.com", "reader@example.com"} {
		rec := f.login(t, account, authPassword, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("login %s: status = %d", account, rec.Code)
		}
		user, _ := decodeData(t, rec)["user"].(map[string]any)
		role, _ := user["role"].(string)
		roles[account] = role

		me := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, sessionCookie(t, rec))
		meUser, _ := decodeData(t, me)["user"].(map[string]any)
		if meUser["role"] != role {
			t.Errorf("%s: me role = %v, login role = %q", account, meUser["role"], role)
		}
	}
	if roles["owner@example.com"] != auth.RoleOwner {
		t.Errorf("owner role = %q", roles["owner@example.com"])
	}
	if roles["reader@example.com"] != auth.RoleReader {
		t.Errorf("reader role = %q", roles["reader@example.com"])
	}
}

func TestAuthAPIAlwaysAnswersJSON(t *testing.T) {
	f := newAuthFixture(t)

	tests := []struct {
		name   string
		method string
		target string
		body   string
		status int
		code   string
	}{
		{"unknown auth path", http.MethodGet, "/api/v1/auth/nope", "", http.StatusNotFound, "not_found"},
		{"wrong method on login", http.MethodGet, "/api/v1/auth/login", "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"wrong method on me", http.MethodDelete, "/api/v1/auth/me", "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"guest me", http.MethodGet, "/api/v1/auth/me", "", http.StatusUnauthorized, "unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, tt.method, tt.target, tt.body, map[string]string{"Origin": testOrigin}, nil)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != jsonContentType {
				t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
			}
			if code, _, _ := decodeAPIError(t, rec); code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
			if strings.Contains(rec.Body.String(), "<html") {
				t.Error("the API answered with an HTML page")
			}
		})
	}
}

func TestAuthPagesRenderForms(t *testing.T) {
	f := newAuthFixture(t)
	f.bootstrapOwner(t, "owner@example.com", "站长")

	for _, page := range []struct{ path, form, fields string }{
		{"/login", `action="/api/v1/auth/login"`, "password"},
		{"/register", `action="/api/v1/auth/register"`, "display_name"},
	} {
		t.Run(page.path, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, page.path, "", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != htmlContentType {
				t.Errorf("Content-Type = %q, want %q", got, htmlContentType)
			}
			body := rec.Body.String()
			if !strings.Contains(body, page.form) {
				t.Errorf("page does not post to %s", page.form)
			}
			if !strings.Contains(body, `name="`+page.fields+`"`) {
				t.Errorf("page has no %s field", page.fields)
			}
			if !strings.Contains(body, `name="email"`) {
				t.Error("page has no email field")
			}
			if !strings.Contains(body, "<noscript>") {
				t.Error("page does not explain that submitting needs JavaScript")
			}
			if strings.Contains(body, `aria-current="page"`) {
				t.Error("an auth page marked a navigation item as current")
			}
		})
	}
}

func TestRegisterPageReflectsRegistrationSetting(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()

	open := f.do(t, http.MethodGet, "/register", "", nil, nil)
	if !strings.Contains(open.Body.String(), `action="/api/v1/auth/register"`) {
		t.Fatal("registration is enabled but the form is missing")
	}

	if err := settings.Set(ctx, f.db, settings.KeyAuthRegistrationEnabled, false); err != nil {
		t.Fatalf("settings.Set: %v", err)
	}
	closed := f.do(t, http.MethodGet, "/register", "", nil, nil)
	if closed.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an explanatory notice", closed.Code)
	}
	body := closed.Body.String()
	if strings.Contains(body, `action="/api/v1/auth/register"`) {
		t.Error("the registration form is still rendered while registration is disabled")
	}
	if !strings.Contains(body, "关闭") {
		t.Error("the page does not explain that registration is closed")
	}
}

func TestAuthPagesRedirectSignedInUsers(t *testing.T) {
	f := newAuthFixture(t)
	registered := f.register(t, "reader@example.com", "读者甲")
	cookie := sessionCookie(t, registered)

	for _, path := range []string{"/login", "/register"} {
		rec := f.do(t, http.MethodGet, path, "", nil, cookie)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s for a signed-in user: status = %d, want 303", path, rec.Code)
			continue
		}
		if location := rec.Header().Get("Location"); location != "/" {
			t.Errorf("%s redirect = %q, want /", path, location)
		}
	}
}

func TestSessionCookieSurvivesUntilExpiry(t *testing.T) {
	f := newAuthFixture(t)
	registered := f.register(t, "reader@example.com", "读者甲")
	cookie := sessionCookie(t, registered)

	var expiresAt string
	if err := f.db.QueryRowContext(context.Background(), `SELECT expires_at FROM sessions`).Scan(&expiresAt); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", expiresAt, err)
	}
	if ttl := time.Until(expiry); ttl > auth.DefaultSessionTTL || ttl < auth.DefaultSessionTTL-time.Minute {
		t.Errorf("session ttl = %s, want about %s", ttl, auth.DefaultSessionTTL)
	}
	if cookie.MaxAge != int(auth.DefaultSessionTTL.Seconds()) {
		t.Errorf("cookie max-age = %d, want %d", cookie.MaxAge, int(auth.DefaultSessionTTL.Seconds()))
	}
}
