package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"boop/internal/auth"
	"boop/internal/settings"
)

// GitHub sign-in (docs/API.md 认证, docs/PRODUCT.md §5.2). The two endpoints are
// the whole HTTP surface: /auth/github/start remembers a one-time state in the
// browser, and /auth/github/callback resolves the account. Every accepted
// callback makes an outbound request, so the address budget in ratelimit.go
// applies to it.
const (
	// oauthCallbackPath is the registered redirect_uri.
	oauthCallbackPath = "/auth/github/callback"
	// oauthStateCookie binds a pending flow to the browser that started it. It
	// is HttpOnly and SameSite=Lax, and the same value must come back in the
	// callback query, so a cross-site attacker can neither read it nor replay a
	// flow it did not start.
	oauthStateCookie = "boop_oauth"
	// oauthStateTTL bounds how long a sign-in may take.
	oauthStateTTL = 10 * time.Minute
	// oauthStateBytes is the state entropy; only the creating browser ever sees
	// the value.
	oauthStateBytes = 32
	// maxReturnToBytes bounds the remembered path.
	maxReturnToBytes = 300
	// maxOAuthStates bounds how many sign-ins may be pending at once. The map is
	// swept on every use, and this ceiling keeps memory bounded even while many
	// addresses start flows at the same time.
	maxOAuthStates = 1000
	// oauthStartScope and oauthCallbackScope give the two endpoints their own
	// address budget on the shared oauth limiter (see ratelimit.go).
	oauthStartScope    = "oauth-start"
	oauthCallbackScope = "oauth-callback"
)

// ErrOAuthBusy reports that too many sign-ins are pending.
var ErrOAuthBusy = errors.New("server: too many pending github sign-ins")

// oauthState is one pending GitHub flow.
type oauthState struct {
	returnTo  string
	expiresAt time.Time
}

// oauthStates holds the pending flows. Boop v0.1 runs one process, so memory is
// enough (the same ceiling the rate limits document); a restart drops pending
// sign-ins, which is safe because the user simply starts again.
type oauthStates struct {
	mu      sync.Mutex
	entries map[string]oauthState
	now     func() time.Time
}

func newOAuthStates(now func() time.Time) *oauthStates {
	return &oauthStates{entries: make(map[string]oauthState), now: now}
}

func (s *oauthStates) create(returnTo string) (string, time.Time, error) {
	raw := make([]byte, oauthStateBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("server: oauth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	expiresAt := now.Add(oauthStateTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if len(s.entries) >= maxOAuthStates {
		return "", time.Time{}, ErrOAuthBusy
	}
	s.entries[state] = oauthState{returnTo: returnTo, expiresAt: expiresAt}
	return state, expiresAt, nil
}

// consume removes a state and reports it. Single use is what makes a replay
// useless, and an expired entry is refused like an unknown one.
func (s *oauthStates) consume(state string) (oauthState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)

	entry, ok := s.entries[state]
	if !ok {
		return oauthState{}, false
	}
	delete(s.entries, state)
	if !now.Before(entry.expiresAt) {
		return oauthState{}, false
	}
	return entry, true
}

// sweepLocked drops expired entries, so abandoned flows cannot grow the map.
func (s *oauthStates) sweepLocked(now time.Time) {
	for state, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, state)
		}
	}
}

func (s *oauthStates) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// handleGitHubStart starts the flow: it stores a one-time state, binds it to
// this browser with a cookie and redirects to GitHub.
func (s *server) handleGitHubStart(w http.ResponseWriter, r *http.Request) {
	if !s.allowOAuthRequest(w, r, oauthStartScope) {
		return
	}
	clientID, _, ok := s.githubCredentials(w, r)
	if !ok {
		return
	}
	returnTo := safeReturnTo(r.URL.Query().Get("return_to"))
	state, expiresAt, err := s.oauthStates.create(returnTo)
	switch {
	case errors.Is(err, ErrOAuthBusy):
		writeFailure(w, r, http.StatusServiceUnavailable, "oauth_busy", "GitHub 登录请求过多，请稍后再试")
		return
	case err != nil:
		s.logger.LogAttrs(r.Context(), slog.LevelError, "oauth state creation failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	http.SetCookie(w, s.oauthStateCookie(state, expiresAt))
	http.Redirect(w, r, s.github.AuthorizationURL(clientID, s.cfg.BaseURL+oauthCallbackPath, state), http.StatusFound)
}

// handleGitHubCallback finishes the flow: the state must match the cookie of the
// browser that started it and is spent on use, then the code is exchanged and
// the account resolved.
func (s *server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if !s.allowOAuthRequest(w, r, oauthCallbackScope) {
		return
	}

	presented := strings.TrimSpace(r.URL.Query().Get("state"))
	cookie, cookieErr := r.Cookie(oauthStateCookie)
	cookieMatches := cookieErr == nil && cookie.Value != "" &&
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(presented)) == 1
	entry, known := s.oauthStates.consume(presented)
	// The state is spent either way: a browser never needs it twice.
	http.SetCookie(w, s.clearCookie(oauthStateCookie))
	if presented == "" || !known || !cookieMatches {
		writeFailure(w, r, http.StatusForbidden, "oauth_state_invalid", "登录请求已失效，请重新发起 GitHub 登录")
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeFailure(w, r, http.StatusBadRequest, "oauth_code_missing", "GitHub 未返回授权码")
		return
	}

	clientID, clientSecret, ok := s.githubCredentials(w, r)
	if !ok {
		return
	}
	token, err := s.github.Exchange(r.Context(), clientID, clientSecret, code)
	if err != nil {
		s.writeGitHubFailure(w, r, "github token exchange", err)
		return
	}
	identity, err := s.github.FetchIdentity(r.Context(), token)
	if err != nil {
		s.writeGitHubFailure(w, r, "github identity", err)
		return
	}
	user, err := auth.SignInWithGitHub(r.Context(), s.db, identity, time.Now())
	if err != nil {
		s.writeGitHubFailure(w, r, "github sign-in", err)
		return
	}
	if err := auth.MarkLogin(r.Context(), s.db, user.ID, time.Now()); err != nil {
		// Losing the audit timestamp must not block a valid sign-in.
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "could not record last login",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
	}

	session, err := s.newSession(r, user)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "session creation failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	http.SetCookie(w, s.sessionCookie(session.Token, session.ExpiresAt))
	http.Redirect(w, r, safeReturnTo(entry.returnTo), http.StatusSeeOther)
}

// allowOAuthRequest throttles one GitHub endpoint by address. Each endpoint has
// its own budget on the shared limiter: every accepted start stores a pending
// state and every accepted callback makes an outbound request. This is a browser
// navigation, so the refusal is the HTML status page with a Retry-After header
// rather than the JSON envelope.
func (s *server) allowOAuthRequest(w http.ResponseWriter, r *http.Request, scope string) bool {
	allowed, wait := s.limiters.oauth.allow(scope + ":" + ipKey(r.RemoteAddr))
	if allowed {
		return true
	}
	seconds := int(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeFailure(w, r, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试")
	return false
}

// githubCredentials decrypts the stored OAuth client pair for a flow that is
// about to start. Each failure is answered explicitly: the flow never starts
// half configured, and no value is ever echoed.
func (s *server) githubCredentials(w http.ResponseWriter, r *http.Request) (clientID, clientSecret string, ok bool) {
	clientID, clientSecret, err := s.githubSecrets(r.Context())
	if err == nil {
		return clientID, clientSecret, true
	}
	if errors.Is(err, settings.ErrMasterKeyRequired) {
		writeFailure(w, r, http.StatusServiceUnavailable, "github_unconfigured", "站点未配置 BOOP_MASTER_KEY，无法使用 GitHub 登录")
		return "", "", false
	}
	return "", "", s.writeGitHubSecretFailure(w, r, "github credentials", err)
}

// githubSecrets is the single place that decides whether GitHub sign-in is
// usable: it decrypts both stored values and requires both to be non-empty. A
// missing master key, a missing half and a damaged ciphertext are each reported
// as an error and never as an empty client.
func (s *server) githubSecrets(ctx context.Context) (clientID, clientSecret string, err error) {
	if s.secrets == nil {
		return "", "", settings.ErrMasterKeyRequired
	}
	clientID, err = settings.ReadSecret(ctx, s.db, s.secrets, settings.SecretKeyGitHubClientID)
	if err != nil {
		return "", "", err
	}
	clientSecret, err = settings.ReadSecret(ctx, s.db, s.secrets, settings.SecretKeyGitHubClientSecret)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return "", "", fmt.Errorf("%w: stored GitHub client pair is incomplete", settings.ErrSecretNotFound)
	}
	return clientID, clientSecret, nil
}

// githubConfigured reports whether the GitHub sign-in entry can work. Both
// stored values must actually decrypt and be non-empty, so a wrong master key or
// a damaged ciphertext hides the entry instead of offering a link that would
// fail. The check is deliberately not cached: a settings change takes effect on
// the next page. A failure degrades to "not offered" with a redacted warning,
// and the password form keeps working.
func (s *server) githubConfigured(r *http.Request) bool {
	_, _, err := s.githubSecrets(r.Context())
	if err == nil {
		return true
	}
	if !errors.Is(err, settings.ErrMasterKeyRequired) && !errors.Is(err, settings.ErrSecretNotFound) {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "github sign-in entry hidden",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
	}
	return false
}

func (s *server) writeGitHubSecretFailure(w http.ResponseWriter, r *http.Request, name string, err error) bool {
	if errors.Is(err, settings.ErrSecretNotFound) {
		writeFailure(w, r, http.StatusServiceUnavailable, "github_unconfigured", "站点尚未配置 GitHub 登录")
		return false
	}
	// The message names the stored setting and the cipher failure only; neither
	// the master key nor any plaintext appears in it.
	s.logger.LogAttrs(r.Context(), slog.LevelError, name+" unavailable",
		slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
	writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	return false
}

// writeGitHubFailure maps an OAuth failure onto the documented envelope. The
// messages never carry a code, a token or a secret.
func (s *server) writeGitHubFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, auth.ErrGitHubRejected):
		writeFailure(w, r, http.StatusBadRequest, "oauth_rejected", "GitHub 拒绝了本次授权，请重试")
	case errors.Is(err, auth.ErrGitHubUnavailable):
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, operation+" failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusBadGateway, "github_unavailable", "GitHub 暂时不可用，请稍后重试")
	case errors.Is(err, auth.ErrEmailUnverified):
		writeFailure(w, r, http.StatusForbidden, "email_unverified", "GitHub 未提供已验证的邮箱，无法登录或绑定")
	case errors.Is(err, auth.ErrRegistrationDisabled):
		writeFailure(w, r, http.StatusForbidden, "registration_disabled", "站点已关闭公开注册")
	case errors.Is(err, auth.ErrAccountDisabled):
		writeFailure(w, r, http.StatusForbidden, "account_disabled", "该账号已被停用")
	case errors.Is(err, auth.ErrGitHubLinkConflict):
		// The identity belongs to another account: refusing is the only safe
		// answer, because the alternative would sign the wrong user in.
		writeFailure(w, r, http.StatusConflict, "oauth_conflict", "该 GitHub 账号已绑定到其他账户")
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, operation+" failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	}
}

// oauthStateCookie carries the pending state to the browser that started the
// flow.
func (s *server) oauthStateCookie(state string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(expires.Sub(time.Now()).Round(time.Second).Seconds()),
	}
}

// safeReturnTo accepts only a same-origin path: an absolute URL, a
// protocol-relative URL and a backslash form (which browsers treat as "/") all
// fall back to the home page, so the callback can never redirect off-site.
func safeReturnTo(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxReturnToBytes {
		return "/"
	}
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsRune(value, '\\') {
		return "/"
	}
	if strings.ContainsAny(value, "\r\n") {
		return "/"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" {
		return "/"
	}
	return value
}
