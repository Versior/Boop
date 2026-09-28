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
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"

	"boop/internal/config"
	"boop/internal/store"
	"boop/web"
)

const (
	htmlContentType = "text/html; charset=utf-8"
	jsonContentType = "application/json; charset=utf-8"

	apiPrefix = "/api/"
)

type server struct {
	cfg    config.Config
	db     *sql.DB
	logger *slog.Logger
	pages  map[string]*template.Template
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
	return &server{cfg: cfg, db: db, logger: logger, pages: pages}, nil
}

// parsePages builds one isolated template set per page so pages cannot leak
// definitions into each other.
func parsePages() (map[string]*template.Template, error) {
	names := []string{"home"}
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
	mux.HandleFunc("GET /static/", s.handleStatic)
	mux.HandleFunc("/", s.handleNotFound)

	var handler http.Handler = mux
	handler = limitBody(handler, s.cfg.MaxUploadBytes())
	handler = recoverPanic(handler, s.logger)
	handler = accessLog(handler, s.logger)
	handler = securityHeaders(handler)
	return withRequestID(handler)
}

// homeView is the server-rendered state of the shell. Feed content arrives with
// the publishing module; the filter already drives navigation state.
type homeView struct {
	Filter string
}

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("type")
	switch filter {
	case "", "article", "photo":
	default:
		writeFailure(w, r, http.StatusBadRequest, "invalid_type", "该内容筛选类型不存在")
		return
	}
	s.render(w, r, http.StatusOK, "home", homeView{Filter: filter})
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

// handleStatic serves the embedded assets with explicit content types so the
// result does not depend on the host MIME database.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == r.URL.Path || !fs.ValidPath(name) {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}
	content, err := fs.ReadFile(web.FS, "static/"+name)
	if err != nil {
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
		return
	}
	w.Header().Set("Content-Type", staticContentType(name))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(content); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "static write failed",
			slog.String("asset", name), slog.String("error", err.Error()))
	}
}

func staticContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
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
		"<title>" + escape(http.StatusText(status)) + " · Boop</title>" +
		"<link rel=\"stylesheet\" href=\"/static/app.css\"></head><body>" +
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
