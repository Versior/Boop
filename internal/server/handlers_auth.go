package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"boop/internal/auth"
	"boop/internal/settings"
)

const (
	sessionCookieName = "boop_session"

	// maxAuthJSONBytes caps authentication request bodies on their own, far
	// below the configurable upload budget: a sign-in or registration payload is
	// a few hundred bytes, so the upload ceiling must never be spendable here.
	maxAuthJSONBytes = 64 << 10

	// navNeutralFilter matches no navigation item, so the auth pages mark no
	// section as current in the shell.
	navNeutralFilter = "auth"

	// invalidCredentialsMessage is the single external answer to every failed
	// sign-in: unknown email, wrong password and disabled account are
	// indistinguishable to the caller.
	invalidCredentialsMessage = "邮箱或密码不正确"
)

// authState is the request-scoped result of session resolution.
type authState struct {
	authenticated bool
	user          auth.User
	session       auth.Session
}

func authStateFrom(ctx context.Context) (authState, bool) {
	state, ok := ctx.Value(authContextKey).(authState)
	return state, ok
}

// authenticate resolves the session cookie once per request and stores the
// result in the context. Guests cost no database query.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := sessionTokenFrom(r)
		if token == "" || s.db == nil {
			next.ServeHTTP(w, r)
			return
		}
		session, user, err := auth.LookupSession(r.Context(), s.db, token, time.Now())
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrSessionInvalid),
				errors.Is(err, auth.ErrSessionExpired),
				errors.Is(err, auth.ErrAccountDisabled):
				// This cookie can never work again: drop it and continue as a guest.
				http.SetCookie(w, s.clearCookie(sessionCookieName))
			default:
				s.logger.LogAttrs(r.Context(), slog.LevelError, "session lookup failed",
					slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
			}
			next.ServeHTTP(w, r)
			return
		}
		state := authState{authenticated: true, user: user, session: session}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey, state)))
	})
}

// guardUnsafeMethods enforces the documented write protections: the request
// must come from this origin, and a signed-in request must present its session
// CSRF token.
func (s *server) guardUnsafeMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		state, _ := authStateFrom(r.Context())

		switch {
		case r.Header.Get("Origin") != "":
			if !s.sameOrigin(r.Header.Get("Origin")) {
				writeFailure(w, r, http.StatusForbidden, "origin_mismatch", "请求来源不被允许")
				return
			}
		case r.Header.Get("Referer") != "":
			if !s.sameOrigin(r.Header.Get("Referer")) {
				writeFailure(w, r, http.StatusForbidden, "origin_mismatch", "请求来源不被允许")
				return
			}
		case !state.authenticated:
			// No origin evidence and no session to check: refuse the write
			// rather than trusting a request the browser did not label.
			writeFailure(w, r, http.StatusForbidden, "origin_required", "写请求必须来自本站页面")
			return
		}

		if state.authenticated && !auth.VerifyCSRF(state.session, r.Header.Get("X-CSRF-Token")) {
			writeFailure(w, r, http.StatusForbidden, "csrf_invalid", "安全校验失败，请刷新页面后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// sameOrigin compares a URL (Origin header or Referer) with BOOP_BASE_URL.
func (s *server) sameOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	base, err := url.Parse(s.cfg.BaseURL)
	if err != nil || base.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Scheme, base.Scheme) && strings.EqualFold(parsed.Host, base.Host)
}

// sessionCookie builds the session cookie: HttpOnly, SameSite=Lax, Path=/ and
// Secure following BOOP_SECURE_COOKIES (docs/PRODUCT.md §5.2).
func (s *server) sessionCookie(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		// The cookie lifetime is the session TTL; Expires carries the exact instant.
		MaxAge: int(expires.Sub(time.Now()).Round(time.Second).Seconds()),
	}
}

// clearCookie expires a cookie on this browser.
func (s *server) clearCookie(name string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0).UTC(),
	}
}

func sessionTokenFrom(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// ipPrefix stores a coarse network prefix instead of the full client address.
func ipPrefix(remoteAddr string) string {
	addr, err := netip.ParseAddr(remoteHost(remoteAddr))
	if err != nil {
		return ""
	}
	bits := 64
	if addr.Unmap().Is4() {
		bits = 24
	}
	prefix, err := addr.Unmap().Prefix(bits)
	if err != nil {
		return ""
	}
	return prefix.String()
}

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type userPayload struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	AvatarURL   string `json:"avatar_url"`
}

type sessionPayload struct {
	User      userPayload `json:"user"`
	CSRFToken string      `json:"csrf_token"`
}

func userPayloadOf(user auth.User) userPayload {
	return userPayload{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        user.Role,
		AvatarURL:   user.AvatarURL,
	}
}

// decodeJSONBody reads exactly one JSON object, rejecting unknown fields and
// trailing content so a typo cannot silently become a default value.
func decodeJSONBody(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

// readJSON decodes the body and reports the failure itself, returning false
// when the caller must stop. The body is narrowed to maxBytes first, so no
// handler can be made to buffer more than its own limit; auth requests and
// content writes therefore have independent ceilings.
func (s *server) readJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	if err := decodeJSONBody(r, dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeFailure(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "请求体超过大小上限")
			return false
		}
		writeFailure(w, r, http.StatusBadRequest, "invalid_body", "请求体不是合法的 JSON 对象")
		return false
	}
	return true
}

// handleRegisterAPI creates a reader account and signs it in. Public
// registration can only ever create a reader.
func (s *server) handleRegisterAPI(w http.ResponseWriter, r *http.Request) {
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "registration settings unavailable",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	if !values.RegistrationEnabled {
		writeFailure(w, r, http.StatusForbidden, "registration_disabled", "站点已关闭公开注册")
		return
	}

	var body registerRequest
	if !s.readJSON(w, r, &body, maxAuthJSONBytes) {
		return
	}
	// Registration is limited by address and by target email, so neither one
	// address nor one mailbox can be used to create accounts in bulk.
	if !s.guardRateLimit(w, r, s.limiters.register, "register", ipKey(r.RemoteAddr), emailKey(body.Email)) {
		return
	}

	user, err := auth.CreateReader(r.Context(), s.db, body.Email, body.DisplayName, body.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidEmail):
		writeFailure(w, r, http.StatusBadRequest, "invalid_email", "邮箱格式不正确")
		return
	case errors.Is(err, auth.ErrInvalidPassword):
		writeFailure(w, r, http.StatusBadRequest, "invalid_password", "密码长度需为 10 到 72 字节")
		return
	case errors.Is(err, auth.ErrInvalidDisplayName):
		writeFailure(w, r, http.StatusBadRequest, "invalid_display_name", "昵称长度需为 1 到 60 个字符")
		return
	case errors.Is(err, auth.ErrEmailTaken):
		writeFailure(w, r, http.StatusConflict, "email_taken", "该邮箱已注册")
		return
	case err != nil:
		s.logger.LogAttrs(r.Context(), slog.LevelError, "registration failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}

	s.startSession(w, r, user, http.StatusCreated)
}

// handleLoginAPI verifies credentials and rotates the session.
func (s *server) handleLoginAPI(w http.ResponseWriter, r *http.Request) {
	var body credentialsRequest
	if !s.readJSON(w, r, &body, maxAuthJSONBytes) {
		return
	}
	if strings.TrimSpace(body.Email) == "" || body.Password == "" {
		writeFailure(w, r, http.StatusBadRequest, "invalid_body", "请填写邮箱和密码")
		return
	}
	// Guess attempts are limited per address and per account before any password
	// hashing happens, so a flood cannot burn CPU either way.
	if !s.guardRateLimit(w, r, s.limiters.login, "login", ipKey(r.RemoteAddr), emailKey(body.Email)) {
		return
	}

	user, err := auth.FindByEmail(r.Context(), s.db, body.Email)
	if err != nil {
		if !errors.Is(err, auth.ErrUserNotFound) {
			s.logger.LogAttrs(r.Context(), slog.LevelError, "login lookup failed",
				slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
			writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			return
		}
		// Burn the same bcrypt cost as a real check so response time does not
		// reveal whether the address exists.
		auth.VerifyDummyPassword(body.Password)
		writeFailure(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
		return
	}
	if !auth.VerifyPassword(user.PasswordHash, body.Password) || !user.IsActive() {
		writeFailure(w, r, http.StatusUnauthorized, "invalid_credentials", invalidCredentialsMessage)
		return
	}

	if err := auth.MarkLogin(r.Context(), s.db, user.ID, time.Now()); err != nil {
		// Losing the audit timestamp must not block a valid sign-in.
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "could not record last login",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
	}
	s.startSession(w, r, user, http.StatusOK)
}

// startSession rotates any presented session into a fresh one, sets both
// cookies and answers with the account plus its CSRF token.
func (s *server) startSession(w http.ResponseWriter, r *http.Request, user auth.User, status int) {
	session, err := auth.RotateSession(r.Context(), s.db, sessionTokenFrom(r), auth.NewSession{
		UserID:    user.ID,
		Now:       time.Now(),
		TTL:       auth.DefaultSessionTTL,
		UserAgent: r.UserAgent(),
		IPPrefix:  ipPrefix(r.RemoteAddr),
	})
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "session creation failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	http.SetCookie(w, s.sessionCookie(session.Token, session.ExpiresAt))
	writeJSON(w, status, map[string]any{
		"data": sessionPayload{User: userPayloadOf(user), CSRFToken: session.CSRFToken},
	})
}

// handleLogoutAPI deletes the current session. Signing out twice is harmless.
func (s *server) handleLogoutAPI(w http.ResponseWriter, r *http.Request) {
	if err := auth.DeleteSession(r.Context(), s.db, sessionTokenFrom(r)); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "logout failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	http.SetCookie(w, s.clearCookie(sessionCookieName))
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"logged_out": true}})
}

// handleMeAPI answers the current account and CSRF token; guests get 401.
func (s *server) handleMeAPI(w http.ResponseWriter, r *http.Request) {
	state, _ := authStateFrom(r.Context())
	if !state.authenticated {
		writeFailure(w, r, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": sessionPayload{User: userPayloadOf(state.user), CSRFToken: state.session.CSRFToken},
	})
}

// handleAuthFallback keeps every /api/v1/auth answer JSON: unknown paths are
// 404 and known paths with a wrong method are 405, never ServeMux plain text.
func (s *server) handleAuthFallback(w http.ResponseWriter, r *http.Request) {
	if allowed, known := authRouteMethods[r.URL.Path]; known {
		w.Header().Set("Allow", allowed)
		writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
		return
	}
	writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
}

var authRouteMethods = map[string]string{
	"/api/v1/auth/register": http.MethodPost,
	"/api/v1/auth/login":    http.MethodPost,
	"/api/v1/auth/logout":   http.MethodPost,
	"/api/v1/auth/me":       http.MethodGet,
}

// authView is the shell state of the sign-in pages.
type authView struct {
	pageView
	RegistrationEnabled bool
}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.redirectSignedIn(w, r) {
		return
	}
	s.render(w, r, http.StatusOK, "login", authView{pageView: s.shellView(r, navNeutralFilter)})
}

func (s *server) handleRegisterPage(w http.ResponseWriter, r *http.Request) {
	if s.redirectSignedIn(w, r) {
		return
	}
	enabled := true
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		// Fail closed: without readable settings the page must not offer a form
		// whose submission would be rejected anyway.
		s.logger.LogAttrs(r.Context(), slog.LevelError, "registration settings unavailable",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		enabled = false
	} else {
		enabled = values.RegistrationEnabled
	}
	s.render(w, r, http.StatusOK, "register", authView{pageView: s.shellView(r, navNeutralFilter), RegistrationEnabled: enabled})
}

func (s *server) redirectSignedIn(w http.ResponseWriter, r *http.Request) bool {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		return false
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return true
}
