// Package server wires the HTTP surface of Boop: embedded UI shell, static
// assets and the JSON API infrastructure shared by every module.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	stdhtml "html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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
	// storage is where uploads are kept right now. It is read from the settings
	// table at construction and replaced after every settings save, which is
	// what lets the storage category take effect without a restart; it stays
	// nil until a read succeeds, and mediaOpts then keeps to the environment.
	storage atomic.Pointer[media.Options]
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
	box, err := cfg.SecretBox()
	if err != nil {
		return nil, err
	}
	srv := &server{
		cfg:          cfg,
		db:           db,
		logger:       logger,
		pages:        pages,
		limiters:     newLimiters(time.Now),
		secrets:      box,
		github:       auth.NewGitHubAPI(nil),
		oauthStates:  newOAuthStates(time.Now),
		sessionSweep: newSessionSweep(time.Now),
	}
	// Where uploads are kept decides the address of every published image, so
	// it is read here rather than on the first request. A failed read is not
	// fatal: the process keeps to the environment until a save replaces it,
	// which is exactly what it did before the storage category existed.
	if err := srv.refreshStorage(context.Background()); err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn, "storage settings unavailable, staying on the environment",
			slog.String("error", err.Error()))
	}
	return srv, nil
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
	mux.HandleFunc("GET /admin/settings", s.handleAdminSettingsIndex)
	mux.HandleFunc("GET /admin/settings/{section}", s.handleAdminSettingsPage)
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
	// Owner is true for the signed-in owner, so the shell can offer the
	// moderation entry without every page computing it.
	Owner bool
	// Admin names the back-end category this page belongs to and stays empty on
	// every front-end page. The shell renders the back-end navigation instead of
	// the front-end one when it is set, so the two surfaces never mix.
	Admin string
	// SignedIn is true for any signed-in account, owner or reader. The shell
	// needs it for the entries every session gets - signing out - while Owner
	// stays the narrower flag for the owner-only ones. Deriving it from
	// CSRFToken would have worked by accident: the token is only issued to a
	// session, but it is a credential, not a statement about who is asking.
	SignedIn bool
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
		view.SignedIn = true
		view.Owner = state.user.IsOwner()
	}
	return view
}

// adminShellView is the shell of an owner-only page. The back end deliberately
// does not reuse the front-end navigation: 首页/搜索/文章/摄影 mean nothing in a
// management page, and showing them would make the two surfaces look identical.
// The shell swaps one navigation for the other instead of rendering both.
func (s *server) adminShellView(r *http.Request, section string) pageView {
	view := s.shellView(r, navNeutralFilter)
	view.Admin = section
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
//
// It reads the snapshot refreshed by refreshStorage rather than the process
// configuration, so saving the storage category changes where uploads go
// without a restart. A nil snapshot means the settings table has not been read
// yet or could not be, and the environment is what the process falls back to.
func (s *server) mediaOpts() media.Options {
	if opts := s.storage.Load(); opts != nil {
		return *opts
	}
	return s.environmentMediaOpts()
}

// environmentMediaOpts is the configuration the process had before the storage
// category existed: BOOP_DATA_DIR for the local directory, BOOP_R2_* for a
// bucket. It is the value a construction-time read failure falls back to.
func (s *server) environmentMediaOpts() media.Options {
	return media.Options{
		DataDir:  s.cfg.DataDir,
		MaxBytes: s.cfg.MaxUploadBytes(),
		Object:   s.cfg.ObjectStorage(),
	}
}

// refreshStorage re-reads where uploads are kept and makes it the configuration
// every store, serve and link path uses. It is called at construction and after
// every settings save; that second call is what makes the storage category
// effective without a restart.
func (s *server) refreshStorage(ctx context.Context) error {
	storage, err := settings.ReadStorage(ctx, s.db, s.secrets, s.cfg.StorageSelection())
	if err != nil {
		return err
	}
	if storage.Object() && (storage.AccessKeyID == "" || storage.SecretAccessKey == "") {
		// Reading a bucket needs the public address, but writing to one needs
		// the signing key: a site in this state still serves its images and
		// then fails every upload, so it is said out loud rather than left to
		// the first upload of the day.
		s.logger.LogAttrs(ctx, slog.LevelWarn, "object storage is configured without usable credentials",
			slog.String("bucket", storage.Bucket))
	}
	opts := media.Options{
		DataDir:  s.cfg.DataDir,
		MaxBytes: s.cfg.MaxUploadBytes(),
		Object:   storage.ObjectOptions(),
	}
	s.storage.Store(&opts)
	return nil
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
