// Package server wires the HTTP surface of Boop: embedded UI shell, static
// assets and the JSON API infrastructure shared by every module.
package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	stdhtml "html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"boop/internal/ai"
	"boop/internal/auth"
	"boop/internal/config"
	"boop/internal/media"
	"boop/internal/secretbox"
	"boop/internal/settings"
	"boop/internal/store"
	"boop/web"
)

const (
	htmlContentType = "text/html; charset=utf-8"
	jsonContentType = "application/json; charset=utf-8"

	apiPrefix = "/api/"

	// immutableCacheControl is the lifetime of a resource whose address never
	// means anything else: a versioned static asset, and a stored upload whose
	// name is random and never rewritten.
	immutableCacheControl = "public, max-age=31536000, immutable"
)

type server struct {
	cfg      config.Config
	db       *sql.DB
	logger   *slog.Logger
	pages    map[string]*template.Template
	limiters *limiters
	// secrets is nil when BOOP_MASTER_KEY is not configured; every secret feature
	// then fails closed instead of writing plaintext.
	secrets *secretbox.Box
	// github is the GitHub client. Its base URLs are fields so handler tests can
	// point the flow at a local server.
	github      auth.GitHubAPI
	oauthStates *oauthStates
	// sessionSweep amortises the expired-session cleanup over sign-ins. It is a
	// struct rather than a timer so the process keeps exactly one concurrency
	// model: no goroutine writes to the database behind a request's back.
	sessionSweep *sessionSweep
	// aiRefresh is the process-local single-flight guard of the author status
	// refresh, so concurrent home visits never duplicate generation.
	aiRefresh ai.Guard
}

// New builds the Boop HTTP handler. It panics only when the embedded templates
// fail to parse, which is a build-time invariant rather than a runtime state.
func New(cfg config.Config, db *sql.DB) http.Handler {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	srv, err := newServer(cfg, db, logger)
	if err != nil {
		panic(err)
	}
	return srv.handler()
}

func newServer(cfg config.Config, db *sql.DB, logger *slog.Logger) (*server, error) {
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	// The master key is optional: without it every secret feature reports an
	// unconfigured site instead of storing plaintext, so the box stays nil.
	var box *secretbox.Box
	if cfg.MasterKey != "" {
		if box, err = secretbox.NewFromBase64(cfg.MasterKey); err != nil {
			return nil, err
		}
	}
	return &server{
		cfg:          cfg,
		db:           db,
		logger:       logger,
		pages:        pages,
		limiters:     newLimiters(time.Now),
		secrets:      box,
		github:       auth.NewGitHubAPI(nil),
		oauthStates:  newOAuthStates(time.Now),
		sessionSweep: newSessionSweep(time.Now),
	}, nil
}

// parsePages builds one isolated template set per page so pages cannot leak
// definitions into each other.
func parsePages() (map[string]*template.Template, error) {
	names := []string{"home", "post", "search", "login", "register", "bookmarks", "admin_comments", "admin_settings"}
	pages := make(map[string]*template.Template, len(names))
	for _, name := range names {
		tmpl, err := template.New(name).ParseFS(web.FS, "templates/base.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		pages[name] = tmpl
	}
	return pages, nil
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /register", s.handleRegisterPage)
	mux.HandleFunc("GET /p/{slug}", s.handlePostPage)
	mux.HandleFunc("GET /search", s.handleSearchPage)
	mux.HandleFunc("GET /feed.xml", s.handleFeed)
	// A non-GET /feed.xml is answered by this process rather than by ServeMux
	// plain text, so the 405 always carries an Allow header.
	mux.HandleFunc("/feed.xml", s.handleFeedMethodFallback)
	// The discovery documents a crawler looks for at the root. Their absolute
	// links come from BOOP_BASE_URL and never from the request.
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	mux.HandleFunc("/robots.txt", s.handleSEOMethodFallback)
	mux.HandleFunc("GET /sitemap.xml", s.handleSitemap)
	mux.HandleFunc("/sitemap.xml", s.handleSEOMethodFallback)
	mux.HandleFunc("GET /bookmarks", s.handleBookmarksPage)
	mux.HandleFunc("GET /admin", s.handleAdminIndex)
	mux.HandleFunc("GET /admin/settings", s.handleAdminSettingsPage)
	mux.HandleFunc("GET /admin/comments", s.handleAdminCommentsPage)
	mux.HandleFunc("GET /auth/github/start", s.handleGitHubStart)
	mux.HandleFunc("GET /auth/github/callback", s.handleGitHubCallback)
	mux.HandleFunc("GET /api/v1/ai/author-status", s.handleAuthorStatusAPI)
	mux.HandleFunc("/api/v1/ai/", s.handleAIFallback)
	mux.HandleFunc("POST /api/v1/admin/ai/test", s.handleAITestAPI)
	mux.HandleFunc("POST /api/v1/admin/ai/author-status/regenerate", s.handleAIRegenerateAPI)
	mux.HandleFunc("POST /api/v1/admin/ai/assist", s.handleAIAssistAPI)
	mux.HandleFunc("GET /api/v1/admin/settings", s.handleAdminSettingsAPI)
	mux.HandleFunc("PATCH /api/v1/admin/settings", s.handlePatchSettingsAPI)
	mux.HandleFunc("GET /api/v1/posts", s.handlePostsAPI)
	mux.HandleFunc("GET /api/v1/search", s.handleSearchAPI)
	mux.HandleFunc("/api/v1/search", s.handleSearchFallback)
	mux.HandleFunc("/api/v1/search/", s.handleSearchFallback)
	mux.HandleFunc("GET /api/v1/posts/{slug}", s.handlePostAPI)
	mux.HandleFunc("GET /api/v1/posts/{slug}/comments", s.handleCommentsAPI)
	mux.HandleFunc("POST /api/v1/posts/{id}/comments", s.handleCreateCommentAPI)
	mux.HandleFunc("PUT /api/v1/posts/{id}/like", s.handleLikeAPI(true))
	mux.HandleFunc("DELETE /api/v1/posts/{id}/like", s.handleLikeAPI(false))
	mux.HandleFunc("PUT /api/v1/posts/{id}/bookmark", s.handleBookmarkAPI(true))
	mux.HandleFunc("DELETE /api/v1/posts/{id}/bookmark", s.handleBookmarkAPI(false))
	mux.HandleFunc("DELETE /api/v1/comments/{id}", s.handleDeleteCommentAPI)
	mux.HandleFunc("/api/v1/comments/", s.handleCommentsFallback)
	mux.HandleFunc("GET /api/v1/me/bookmarks", s.handleBookmarksAPI)
	mux.HandleFunc("/api/v1/me/", s.handleMeFallback)
	mux.HandleFunc("/api/v1/posts/", s.handlePostsFallback)
	mux.HandleFunc("POST /api/v1/admin/posts", s.handleCreatePostAPI)
	mux.HandleFunc("PATCH /api/v1/admin/posts/{id}", s.handlePatchPostAPI)
	mux.HandleFunc("DELETE /api/v1/admin/posts/{id}", s.handleDeletePostAPI)
	mux.HandleFunc("GET /api/v1/admin/comments", s.handleAdminCommentsAPI)
	mux.HandleFunc("POST /api/v1/admin/comments/{id}/approve", s.handleApproveCommentAPI)
	mux.HandleFunc("POST /api/v1/admin/comments/{id}/reject", s.handleRejectCommentAPI)
	mux.HandleFunc("DELETE /api/v1/admin/comments/{id}", s.handleAdminDeleteCommentAPI)
	mux.HandleFunc("/api/v1/admin/", s.handleAdminFallback)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegisterAPI)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLoginAPI)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogoutAPI)
	mux.HandleFunc("GET /api/v1/auth/me", s.handleMeAPI)
	mux.HandleFunc("/api/v1/auth/", s.handleAuthFallback)
	mux.HandleFunc("POST /api/v1/admin/uploads", s.handleUploadAPI)
	mux.HandleFunc("GET "+media.UploadsPath+"{key...}", s.handleUploads)
	mux.HandleFunc("GET /static/", s.handleStatic)
	mux.HandleFunc("/", s.handleNotFound)

	var handler http.Handler = mux
	handler = s.guardUnsafeMethods(handler)
	handler = s.authenticate(handler)
	// The body ceiling is the single-file cap plus the multipart framing budget;
	// each handler still narrows its own body (readJSON) or the file itself
	// (readUpload, media.Store).
	handler = limitBody(handler, s.cfg.MaxUploadBytes()+multipartBodyOverhead)
	handler = recoverPanic(handler, s.logger)
	handler = accessLog(handler, s.logger)
	handler = securityHeaders(handler)
	return withRequestID(handler)
}

// homeView was replaced by feedView once the feed existed; pageView remains the
// shared shell state.
type pageView struct {
	Filter    string
	CSRFToken string
	// SearchQuery is the query the search page is showing. It stays empty
	// everywhere else, so the right rail only reflects a query on /search.
	SearchQuery string
	// Owner is true for the signed-in owner, so the shell can offer the
	// moderation entry without every page computing it.
	Owner bool
	// SiteName, SiteDescription, SiteAvatarURL and SiteIconURL are the configured
	// brand of the site. Every page renders them (title suffix, brand, aria
	// labels, search placeholder, meta description, favicon), so no user-visible
	// surface hardcodes a name.
	SiteName        string
	SiteDescription string
	SiteAvatarURL   string
	// SiteIconURL is the browser tab icon on its own. It is separate from the
	// avatar so a site can brand the tab without changing the face next to every
	// post.
	SiteIconURL string
	// StaticVersion is appended to every /static URL. The assets are served
	// immutable for a year, so the version is what a deploy changes to move
	// returning visitors off the previous bytes.
	StaticVersion string
	// Meta is the document-level metadata: the canonical address, the link
	// preview and, on the pages that carry one, the structured data.
	Meta pageMeta
	// AIStatus is the author status card of the home right rail. It stays zero on
	// every other page, so the shared shell renders no card outside the home page
	// and no other page ever reads the AI cache.
	AIStatus authorStatusPayload
}

// shellView builds the shell state of a page for the current request: the
// navigation filter plus the session CSRF token when the visitor is signed in.
// It reads the settings itself; a page that already loaded them avoids the
// second read with shellViewWithSettings.
func (s *server) shellView(r *http.Request, filter string) pageView {
	return s.shellViewWithSettings(r, filter, s.displaySettings(r))
}

// shellViewWithSettings is the minimal shell state for a caller that has already
// read the settings, so a render path never queries them twice.
func (s *server) shellViewWithSettings(r *http.Request, filter string, values settings.Values) pageView {
	view := pageView{
		Filter:          filter,
		SiteName:        values.SiteName,
		SiteDescription: values.SiteDescription,
		SiteAvatarURL:   values.SiteAvatarURL,
		SiteIconURL:     values.SiteIconURL,
		StaticVersion:   staticAssets().version,
		Meta:            s.pageMetaOf(r),
	}
	if state, ok := authStateFrom(r.Context()); ok && state.authenticated {
		view.CSRFToken = state.session.CSRFToken
		view.Owner = state.user.IsOwner()
	}
	return view
}

// displaySettings reads the settings for a page that must still render: a failed
// read degrades to the documented defaults with a warning instead of hiding the
// page behind a 500.
func (s *server) displaySettings(r *http.Request) settings.Values {
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "settings unavailable, using defaults",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		return settings.Defaults()
	}
	return values
}

// mediaOpts is the storage configuration shared by every path that stores,
// serves or links an upload. It exists so the upload handler, the asset route
// and the JSON and HTML payloads all read one configuration rather than each
// assembling their own.
func (s *server) mediaOpts() media.Options {
	return media.Options{
		DataDir:  s.cfg.DataDir,
		MaxBytes: s.cfg.MaxUploadBytes(),
		Object:   s.cfg.ObjectStorage(),
	}
}

// handleHealthz reports process liveness and never touches external systems.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "ok\n"); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "healthz write failed", slog.String("error", err.Error()))
	}
}

// handleReadyz reports whether SQLite answers and the schema is up to date.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := store.Ready(r.Context(), s.db); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "readiness check failed", slog.String("error", err.Error()))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "unavailable\n")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ready\n")
}

// handleNotFound answers unknown paths: JSON for the API, HTML for pages.
func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
}

func (s *server) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	tmpl, ok := s.pages[page]
	if !ok {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "unknown page",
			slog.String("page", page), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", data); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "render failed",
			slog.String("page", page), slog.String("error", err.Error()),
			slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	w.Header().Set("Content-Type", htmlContentType)
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "render write failed", slog.String("error", err.Error()))
	}
}

// writeFailure renders the shared failure shape: JSON under /api, HTML pages
// elsewhere. Both carry the request id.
func writeFailure(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	id := requestIDFrom(r.Context())
	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		writeJSON(w, status, map[string]any{
			"error":      map[string]string{"code": code, "message": message},
			"request_id": id,
		})
		return
	}
	writeStatusPage(w, status, message, id)
}

func writeStatusPage(w http.ResponseWriter, status int, message, requestID string) {
	w.Header().Set("Content-Type", htmlContentType)
	w.WriteHeader(status)
	escape := stdhtml.EscapeString
	body := "<!doctype html>\n<html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<meta name=\"color-scheme\" content=\"light dark\">" +
		"<title>" + escape(http.StatusText(status)) + "</title>" +
		// The version query is what keeps a one-year immutable cache honest, so
		// this page needs it too: an unversioned /static/app.css would be pinned
		// in a browser that only ever saw it here.
		"<link rel=\"stylesheet\" href=\"/static/app.css?v=" + staticAssets().version + "\"></head><body>" +
		"<main class=\"status-page\"><h1>" + escape(http.StatusText(status)) + "</h1>" +
		"<p>" + escape(message) + "</p>" +
		"<p class=\"status-id\">request_id " + escape(requestID) + "</p>" +
		"<p><a href=\"/\">返回首页</a></p></main></body></html>\n"
	if _, err := io.WriteString(w, body); err != nil {
		return
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		return
	}
}
