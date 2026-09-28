package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"boop/internal/auth"
	"boop/internal/secretbox"
	"boop/internal/settings"
)

// fakeGitHubServer stands in for github.com. The flow is exercised over real HTTP
// against this server, so no interface or mock layer is needed.
type fakeGitHubServer struct {
	server *httptest.Server
	token  string
	// exchangeStatus and profileStatus steer the two endpoints.
	exchangeStatus int
	profileStatus  int
	// identity is what /user plus /user/emails report.
	profileID int64
	login     string
	name      string
	email     string
	verified  bool
	// seenClientID and seenSecret record what the token endpoint received.
	seenClientID string
	seenSecret   string
}

func newFakeGitHubServer(t *testing.T) *fakeGitHubServer {
	t.Helper()
	fake := &fakeGitHubServer{
		token:     "gho_server_test_token",
		profileID: 777,
		login:     "octocat",
		name:      "Monalisa Octocat",
		email:     "octocat@example.com",
		verified:  true,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if fake.exchangeStatus != 0 && fake.exchangeStatus != http.StatusOK {
			w.WriteHeader(fake.exchangeStatus)
			return
		}
		var body struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Code         string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fake.seenClientID, fake.seenSecret = body.ClientID, body.ClientSecret
		writeJSON(w, http.StatusOK, map[string]any{"access_token": fake.token, "token_type": "bearer"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if fake.profileStatus != 0 && fake.profileStatus != http.StatusOK {
			w.WriteHeader(fake.profileStatus)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": fake.profileID, "login": fake.login, "name": fake.name, "email": fake.email,
		})
	})
	mux.HandleFunc("GET /user/emails", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"email": fake.email, "primary": true, "verified": fake.verified},
		})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// api points the server's GitHub client at this fake.
func (f *fakeGitHubServer) api() auth.GitHubAPI {
	return auth.GitHubAPI{HTTP: f.server.Client(), OAuthBase: f.server.URL, APIBase: f.server.URL}
}

type githubFixture struct {
	*authFixture
	fake *fakeGitHubServer
}

func (f *githubFixture) start(t *testing.T, returnTo string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/auth/github/start"
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	return f.do(t, http.MethodGet, target, "", nil, nil)
}

// startFlow runs /auth/github/start and returns the state plus the cookie that
// binds it to this browser.
func (f *githubFixture) startFlow(t *testing.T, returnTo string) (string, *http.Cookie) {
	t.Helper()
	rec := f.start(t, returnTo)
	if rec.Code != http.StatusFound {
		t.Fatalf("start: status = %d, want 302: %s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("start Location: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("start did not send a state")
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == oauthStateCookie {
			return state, cookie
		}
	}
	t.Fatalf("start did not set the %s cookie", oauthStateCookie)
	return "", nil
}

func (f *githubFixture) callback(t *testing.T, state, code string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	target := "/auth/github/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(code)
	return f.do(t, http.MethodGet, target, "", nil, cookie)
}

// configuredGitHubFixture stores the client pair and points the flow at the fake.
func configuredGitHubFixture(t *testing.T) *githubFixture {
	t.Helper()
	f := &githubFixture{authFixture: newSettingsFixture(t), fake: newFakeGitHubServer(t)}
	f.srv.github = f.fake.api()
	if err := settings.Apply(t.Context(), f.db, settingsBox(t, f.authFixture), settings.Update{
		Secrets: map[string]string{
			settings.SecretKeyGitHubClientID:     settingsClientID,
			settings.SecretKeyGitHubClientSecret: settingsClientSecret,
		},
	}); err != nil {
		t.Fatalf("store the GitHub client pair: %v", err)
	}
	return f
}

// githubEntryMarker is the GitHub sign-in entry the two auth pages share.
const githubEntryMarker = `href="/auth/github/start?return_to=%2F"`

// TestAuthPagesOfferTheSameGitHubEntry pins the entry rules: the sign-in page and
// the registration page render the same link whenever the stored client pair
// works, and closing public registration keeps it on the registration page so an
// already bound GitHub account can still sign in.
func TestAuthPagesOfferTheSameGitHubEntry(t *testing.T) {
	f := configuredGitHubFixture(t)

	for _, target := range []string{"/login", "/register"} {
		rec := f.do(t, http.MethodGet, target, "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", target, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, githubEntryMarker) {
			t.Errorf("%s does not offer the GitHub entry", target)
		}
		if !strings.Contains(body, "使用 GitHub 登录") {
			t.Errorf("%s does not label the GitHub entry", target)
		}
	}

	f.updateSettings(t, settings.KeyAuthRegistrationEnabled, false)
	rec := f.do(t, http.MethodGet, "/register", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("closed register page: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "已关闭公开注册") {
		t.Error("the closed registration state is not rendered")
	}
	if !strings.Contains(body, githubEntryMarker) {
		t.Error("the GitHub entry disappeared when registration was closed")
	}
}

// TestGitHubEntryIsHiddenWhenTheStoredPairIsUnusable covers the health rule: the
// entry is only offered when both stored values actually decrypt and are
// non-empty, so a wrong master key or a half-configured site hides it and
// /auth/github/start fails safely.
func TestGitHubEntryIsHiddenWhenTheStoredPairIsUnusable(t *testing.T) {
	ctx := t.Context()

	t.Run("wrong master key", func(t *testing.T) {
		f := newSettingsFixture(t)
		// The rows are written with another key: they are intact for whoever wrote
		// them and unreadable for this process.
		other, err := secretbox.New(bytes.Repeat([]byte{9}, secretbox.KeyBytes))
		if err != nil {
			t.Fatalf("secretbox.New: %v", err)
		}
		if err := settings.Apply(ctx, f.db, other, settings.Update{Secrets: map[string]string{
			settings.SecretKeyGitHubClientID:     settingsClientID,
			settings.SecretKeyGitHubClientSecret: settingsClientSecret,
		}}); err != nil {
			t.Fatalf("store the pair with another key: %v", err)
		}

		var logs bytes.Buffer
		f.srv.logger = slog.New(slog.NewJSONHandler(&logs, nil))

		for _, target := range []string{"/login", "/register"} {
			rec := f.do(t, http.MethodGet, target, "", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", target, rec.Code)
			}
			if strings.Contains(rec.Body.String(), githubEntryMarker) {
				t.Errorf("%s still offers the GitHub entry", target)
			}
		}

		rec := f.do(t, http.MethodGet, "/auth/github/start", "", nil, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("start: status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		// This is a browser navigation, so the answer is the shared status page.
		if !strings.Contains(rec.Body.String(), "服务器内部错误") {
			t.Errorf("start: body = %q", rec.Body.String())
		}
		for _, secret := range []string{settingsClientID, settingsClientSecret, f.cfg.MasterKey} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Error("the failed start leaked a stored value")
			}
		}
		for _, cookie := range rec.Result().Cookies() {
			if cookie.Name == sessionCookieID && cookie.Value != "" {
				t.Error("a failed start issued a session")
			}
		}
		if links := f.countRows(t, "oauth_accounts"); links != 0 {
			t.Errorf("oauth_accounts = %d, want none", links)
		}

		// The hidden entry is recorded without echoing any stored value.
		logged := logs.String()
		if !strings.Contains(logged, "github sign-in entry hidden") {
			t.Errorf("no redacted warning was logged: %s", logged)
		}
		for _, secret := range []string{settingsClientID, settingsClientSecret, f.cfg.MasterKey} {
			if strings.Contains(logged, secret) {
				t.Error("the warning leaked a stored value")
			}
		}
	})

	t.Run("only half the pair stored", func(t *testing.T) {
		f := newSettingsFixture(t)
		if err := settings.Apply(ctx, f.db, settingsBox(t, f), settings.Update{
			Secrets: map[string]string{settings.SecretKeyGitHubClientID: settingsClientID},
		}); err != nil {
			t.Fatalf("store the client id: %v", err)
		}
		for _, target := range []string{"/login", "/register"} {
			rec := f.do(t, http.MethodGet, target, "", nil, nil)
			if strings.Contains(rec.Body.String(), githubEntryMarker) {
				t.Errorf("%s offers the entry with only half the pair", target)
			}
		}
		rec := f.do(t, http.MethodGet, "/auth/github/start", "", nil, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("start: status = %d, want 503: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no master key", func(t *testing.T) {
		f := newAuthFixture(t)
		for _, target := range []string{"/login", "/register"} {
			rec := f.do(t, http.MethodGet, target, "", nil, nil)
			if strings.Contains(rec.Body.String(), githubEntryMarker) {
				t.Errorf("%s offers the entry without a master key", target)
			}
		}
	})
}

func TestGitHubStartRequiresConfiguration(t *testing.T) {
	t.Run("no master key", func(t *testing.T) {
		f := newAuthFixture(t)
		rec := f.do(t, http.MethodGet, "/auth/github/start", "", nil, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "BOOP_MASTER_KEY") {
			t.Errorf("body = %q, want it to name the missing configuration", rec.Body.String())
		}
	})

	t.Run("no stored pair", func(t *testing.T) {
		f := newSettingsFixture(t)
		rec := f.do(t, http.MethodGet, "/auth/github/start", "", nil, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "尚未配置 GitHub 登录") {
			t.Errorf("body = %q", rec.Body.String())
		}
	})
}

// TestGitHubFailureMapping pins the documented answers for the callback
// failures, including the link conflict: an identity that belongs to another
// account is refused instead of being turned into a session.
func TestGitHubFailureMapping(t *testing.T) {
	srv := testServer(t, discardLogger())

	for _, tt := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"link conflict", auth.ErrGitHubLinkConflict, http.StatusConflict, "oauth_conflict"},
		{"unverified email", auth.ErrEmailUnverified, http.StatusForbidden, "email_unverified"},
		{"disabled account", auth.ErrAccountDisabled, http.StatusForbidden, "account_disabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				srv.writeGitHubFailure(w, r, "github sign-in", tt.err)
			}))
			rec := do(t, handler, http.MethodGet, "/api/v1/auth/me", nil)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			if code, _, _ := decodeAPIError(t, rec); code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
		})
	}
}

func TestGitHubStartRedirectsWithAStateBoundToTheBrowser(t *testing.T) {
	f := configuredGitHubFixture(t)

	rec := f.start(t, "/bookmarks")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	fakeHost := strings.TrimPrefix(f.fake.server.URL, "http://")
	if location.Host != fakeHost {
		t.Errorf("Location = %q, want the fake GitHub host %q", location, fakeHost)
	}
	if got := location.Query().Get("client_id"); got != settingsClientID {
		t.Errorf("client_id = %q, want the stored (decrypted) value", got)
	}
	if got := location.Query().Get("redirect_uri"); got != testOrigin+oauthCallbackPath {
		t.Errorf("redirect_uri = %q", got)
	}

	var stateCookie *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == oauthStateCookie {
			stateCookie = cookie
		}
	}
	if stateCookie == nil {
		t.Fatal("the start response has no state cookie")
	}
	if !stateCookie.HttpOnly {
		t.Error("the state cookie must be HttpOnly")
	}
	if stateCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", stateCookie.SameSite)
	}
	if stateCookie.Path != "/" {
		t.Errorf("Path = %q, want /", stateCookie.Path)
	}
	// The query state and the cookie carry the same random value, which differs
	// on every start.
	state := location.Query().Get("state")
	if state != stateCookie.Value {
		t.Errorf("cookie = %q, query = %q", stateCookie.Value, state)
	}
	if len(state) != 43 {
		t.Errorf("state %q is %d characters, want 43 (32 bytes base64url)", state, len(state))
	}
	if other, _ := f.startFlow(t, ""); other == state {
		t.Error("two starts produced the same state")
	}
}

func TestGitHubCallbackRejectsUnusableStates(t *testing.T) {
	f := configuredGitHubFixture(t)

	t.Run("no state at all", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/auth/github/callback", "", nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "登录请求已失效") {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("state without the browser cookie", func(t *testing.T) {
		state, _ := f.startFlow(t, "")
		rec := f.callback(t, state, "a-code", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if f.countRows(t, "users") != 0 {
			t.Error("a refused callback created an account")
		}
	})

	t.Run("cookie that does not match the state", func(t *testing.T) {
		state, cookie := f.startFlow(t, "")
		other, _ := f.startFlow(t, "")
		cookie.Value = other
		rec := f.callback(t, state, "a-code", cookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if rec.Header().Get("Location") != "" {
			t.Error("a refused callback answered with a redirect")
		}
	})

	t.Run("unknown state", func(t *testing.T) {
		_, cookie := f.startFlow(t, "")
		rec := f.callback(t, "unknown-state-value", "a-code", cookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("an expired state", func(t *testing.T) {
		clock := newFakeClock()
		f.srv.oauthStates.now = clock.Now
		state, cookie := f.startFlow(t, "")
		before := f.srv.oauthStates.size()
		clock.advance(oauthStateTTL + time.Minute)

		rec := f.callback(t, state, "a-code", cookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if size := f.srv.oauthStates.size(); size >= before {
			t.Errorf("tracked states = %d, want the expired one swept from %d", size, before)
		}
	})

	t.Run("a missing code", func(t *testing.T) {
		state, cookie := f.startFlow(t, "")
		rec := f.callback(t, state, "", cookie)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "未返回授权码") {
			t.Errorf("body = %q", rec.Body.String())
		}
	})
}

func TestOAuthStateIsSingleUseAndExpires(t *testing.T) {
	clock := newFakeClock()
	states := newOAuthStates(clock.Now)

	state, expiresAt, err := states.create("/bookmarks")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !expiresAt.After(clock.Now()) {
		t.Errorf("expiresAt = %v, want a future instant", expiresAt)
	}
	if got := states.size(); got != 1 {
		t.Fatalf("size = %d, want 1", got)
	}
	entry, ok := states.consume(state)
	if !ok || entry.returnTo != "/bookmarks" {
		t.Fatalf("consume = %+v, %v", entry, ok)
	}
	if _, ok := states.consume(state); ok {
		t.Error("the same state was accepted twice")
	}
	if states.size() != 0 {
		t.Errorf("size = %d, want the spent state gone", states.size())
	}

	state, _, err = states.create("/")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clock.advance(oauthStateTTL + time.Second)
	if _, ok := states.consume(state); ok {
		t.Error("an expired state was accepted")
	}
	if states.size() != 0 {
		t.Errorf("size = %d, want expired entries dropped", states.size())
	}
}

func TestGitHubCallbackSignsInAnExistingAccount(t *testing.T) {
	f := configuredGitHubFixture(t)
	owner := f.bootstrapOwner(t, "octocat@example.com", "遇事开心")
	state, cookie := f.startFlow(t, "")

	rec := f.callback(t, state, "the-code", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/" {
		t.Errorf("Location = %q, want /", location)
	}
	if f.fake.seenClientID != settingsClientID || f.fake.seenSecret != settingsClientSecret {
		t.Errorf("GitHub received client_id = %q, secret = %q", f.fake.seenClientID, f.fake.seenSecret)
	}
	if links := f.countRows(t, "oauth_accounts"); links != 1 {
		t.Errorf("oauth_accounts = %d, want 1", links)
	}

	// The session is real: /api/v1/auth/me answers with the account.
	session := sessionCookie(t, rec)
	me := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, session)
	if me.Code != http.StatusOK {
		t.Fatalf("me: status = %d, want 200: %s", me.Code, me.Body.String())
	}
	if got := decodeData(t, me)["user"].(map[string]any)["id"].(float64); int64(got) != owner.ID {
		t.Errorf("me user = %v, want %d", got, owner.ID)
	}

	// The state is spent, so replaying the same callback cannot sign in again.
	replay := f.callback(t, state, "the-code", cookie)
	if replay.Code != http.StatusForbidden {
		t.Fatalf("replay: status = %d, want 403", replay.Code)
	}
	for _, c := range replay.Result().Cookies() {
		if c.Name == sessionCookieID && c.Value != "" {
			t.Error("a replayed callback issued a session")
		}
	}
}

// TestGitHubCallbackBindsWithRegistrationDisabled is the documented requirement:
// closing public registration keeps existing accounts usable through GitHub.
func TestGitHubCallbackBindsWithRegistrationDisabled(t *testing.T) {
	f := configuredGitHubFixture(t)
	owner := f.bootstrapOwner(t, "reader@example.com", "读者甲")
	f.updateSettings(t, settings.KeyAuthRegistrationEnabled, false)
	// The fake GitHub vouches for the address of the existing account.
	f.fake.email = "reader@example.com"

	state, cookie := f.startFlow(t, "/bookmarks")
	rec := f.callback(t, state, "the-code", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/bookmarks" {
		t.Errorf("Location = %q, want the validated return_to", location)
	}
	session := sessionCookie(t, rec)
	me := f.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, session)
	if me.Code != http.StatusOK {
		t.Fatalf("me: status = %d: %s", me.Code, me.Body.String())
	}
	if got := decodeData(t, me)["user"].(map[string]any)["id"].(float64); int64(got) != owner.ID {
		t.Errorf("signed in as %v, want %d", got, owner.ID)
	}
}

func TestGitHubCallbackRefusesUnverifiedCollisionAndNewAccounts(t *testing.T) {
	f := configuredGitHubFixture(t)
	f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	f.fake.email = "reader@example.com"

	t.Run("unverified address matching an account", func(t *testing.T) {
		f.fake.verified = false
		state, cookie := f.startFlow(t, "")
		rec := f.callback(t, state, "the-code", cookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "未提供已验证的邮箱") {
			t.Errorf("body = %q", rec.Body.String())
		}
		if links := f.countRows(t, "oauth_accounts"); links != 0 {
			t.Errorf("oauth_accounts = %d, want none", links)
		}
	})

	t.Run("new account while registration is closed", func(t *testing.T) {
		f.fake.verified = true
		f.updateSettings(t, settings.KeyAuthRegistrationEnabled, false)

		state, cookie := f.startFlow(t, "")
		rec := f.callback(t, state, "the-code", cookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "已关闭公开注册") {
			t.Errorf("body = %q", rec.Body.String())
		}
		if users := f.countRows(t, "users"); users != 1 {
			t.Errorf("users = %d, want only the fixture's owner", users)
		}
	})
}

func TestGitHubCallbackNeverRedirectsOffSite(t *testing.T) {
	// A target Go cleans into a same-origin path is documented here: the redirect
	// stays on this host, which is what the boundary requires. The keys are never
	// empty: Go reuses an empty subtest name's temporary directory, which would
	// share one database between subtests.
	targets := map[string]string{
		"https://evil.example/steal": "/",
		"//evil.example/steal":       "/",
		`/\evil.example`:             "/",
		"javascript:alert(1)":        "/",
		"no return_to at all":        "/",
		"/safe/../..//evil.example":  "/evil.example",
		"/p/ok":                      "/p/ok",
	}
	for caseName, want := range targets {
		returnTo := caseName
		if caseName == "no return_to at all" {
			returnTo = ""
		}
		t.Run(caseName, func(t *testing.T) {
			f := configuredGitHubFixture(t)
			f.bootstrapOwner(t, "octocat@example.com", "遇事开心")
			state, cookie := f.startFlow(t, returnTo)

			rec := f.callback(t, state, "the-code", cookie)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
			}
			location := rec.Header().Get("Location")
			parsed, err := url.Parse(location)
			if err != nil {
				t.Fatalf("Location %q: %v", location, err)
			}
			if parsed.Scheme != "" || parsed.Host != "" {
				t.Errorf("Location = %q, want a same-origin path", location)
			}
			if !strings.HasPrefix(location, "/") || strings.HasPrefix(location, "//") || strings.Contains(location, "\\") {
				t.Errorf("Location = %q, want a safe path", location)
			}
			if location != want {
				t.Errorf("Location = %q, want %q", location, want)
			}
		})
	}
}

func TestGitHubCallbackFailsSafelyWhenGitHubIsUnavailable(t *testing.T) {
	f := configuredGitHubFixture(t)
	f.bootstrapOwner(t, "octocat@example.com", "遇事开心")
	f.fake.exchangeStatus = http.StatusInternalServerError

	state, cookie := f.startFlow(t, "")
	rec := f.callback(t, state, "the-code", cookie)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "GitHub 暂时不可用") {
		t.Errorf("body = %q", body)
	}
	for _, secret := range []string{settingsClientSecret, settingsClientID, f.fake.token} {
		if strings.Contains(body, secret) {
			t.Errorf("the failure page leaks %q", secret)
		}
	}
	if links := f.countRows(t, "oauth_accounts"); links != 0 {
		t.Errorf("oauth_accounts = %d, want none", links)
	}

	// A failing profile lookup is the same class of failure.
	f.fake.exchangeStatus = 0
	f.fake.profileStatus = http.StatusForbidden
	state, cookie = f.startFlow(t, "")
	if rec := f.callback(t, state, "the-code", cookie); rec.Code != http.StatusBadGateway {
		t.Errorf("profile failure: status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubStartIsRateLimitedByAddress(t *testing.T) {
	f := configuredGitHubFixture(t)

	for i := 0; i < oauthBurst; i++ {
		if rec := f.start(t, ""); rec.Code != http.StatusFound {
			t.Fatalf("attempt %d: status = %d, want 302", i+1, rec.Code)
		}
	}
	rec := f.start(t, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the 429 answer has no Retry-After header")
	}
	// The callback keeps its own budget, so a busy sign-in start cannot block a
	// callback that is already in flight.
	if cb := f.callback(t, "unknown", "code", nil); cb.Code != http.StatusForbidden {
		t.Errorf("callback status = %d, want 403: the two endpoints share no budget", cb.Code)
	}
}

// TestOAuthStatesHaveACeiling keeps memory bounded even when many addresses start
// flows at the same time.
func TestOAuthStatesHaveACeiling(t *testing.T) {
	clock := newFakeClock()
	states := newOAuthStates(clock.Now)

	for i := 0; i < maxOAuthStates; i++ {
		if _, _, err := states.create("/"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, _, err := states.create("/"); !errors.Is(err, ErrOAuthBusy) {
		t.Fatalf("error = %v, want ErrOAuthBusy", err)
	}

	// Once the pending flows expire, the sweep frees room again.
	clock.advance(oauthStateTTL + time.Second)
	if _, _, err := states.create("/"); err != nil {
		t.Fatalf("create after expiry: %v", err)
	}
	if size := states.size(); size != 1 {
		t.Errorf("size = %d, want only the fresh state", size)
	}
}

func TestGitHubCallbackIsRateLimitedByAddress(t *testing.T) {
	f := configuredGitHubFixture(t)

	// The limiter is spent before the state is examined, so a wrong state still
	// counts: that is what stops an address from looping the outbound call.
	for i := 0; i < oauthBurst; i++ {
		if rec := f.callback(t, "unknown-state", "the-code", nil); rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status = %d, want 403", i+1, rec.Code)
		}
	}
	rec := f.callback(t, "unknown-state", "the-code", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the 429 answer has no Retry-After header")
	}
	if !strings.Contains(rec.Body.String(), "请求过于频繁") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestSafeReturnTo(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"/", "/"},
		{"/bookmarks", "/bookmarks"},
		{"/p/seaside?x=1", "/p/seaside?x=1"},
		{"  /bookmarks  ", "/bookmarks"},
		{"", "/"},
		{"https://evil.example/x", "/"},
		{"//evil.example/x", "/"},
		{`/\evil.example`, "/"},
		{"/\\evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"/x" + strings.Repeat("y", maxReturnToBytes), "/"},
		{"/x\r\nLocation: https://evil.example", "/"},
	}
	for _, tt := range tests {
		if got := safeReturnTo(tt.raw); got != tt.want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

// TestGitHubSignInRoleIsAlwaysReader is a sanity check that GitHub can never
// grant the owner role.
func TestGitHubSignInRoleIsAlwaysReader(t *testing.T) {
	f := configuredGitHubFixture(t)
	f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	f.fake.email = "reader@example.com"

	state, cookie := f.startFlow(t, "")
	if rec := f.callback(t, state, "the-code", cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	var role string
	if err := f.db.QueryRow(`SELECT role FROM users WHERE email = ?`, "reader@example.com").Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != "reader" {
		t.Errorf("role = %q, want reader", role)
	}
}
