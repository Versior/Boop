package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type contextKey int

const (
	requestIDContextKey contextKey = iota
	authContextKey
)

// contentSecurityPolicy allows only same-origin scripts, styles and connections:
// templates and scripts must never need inline code, so no nonce or
// unsafe-inline escape hatch exists. Images are the one exception: the site
// avatar (site.avatar_url) and the owner account avatar are absolute http(s)
// URLs the operator configures, so a page that renders them needs those origins
// to be loadable.
const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; object-src 'none'; " +
	"frame-ancestors 'none'; form-action 'self'; img-src 'self' data: http: https:; " +
	"style-src 'self'; script-src 'self'; connect-src 'self'"

// withRequestID assigns a correlation id to every request and echoes it.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey, id)))
	})
}

func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on supported platforms; degrade to a time
		// derived id rather than dropping correlation entirely.
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey).(string)
	return id
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// accessLog writes one structured JSON line per request. Health probes are
// excluded so orchestrators cannot flood the log.
func accessLog(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isHealthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Int("bytes", recorder.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("remote_addr", remoteHost(r.RemoteAddr)),
		)
	})
}

// recoverPanic converts a handler panic into a logged 500 response.
func recoverPanic(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
				slog.Any("error", fmt.Errorf("%v", recovered)),
				slog.String("path", r.URL.Path),
				slog.String("request_id", requestIDFrom(r.Context())),
				slog.String("stack", string(debug.Stack())),
			)
			if recorder, ok := w.(*statusRecorder); ok && recorder.wroteHeader {
				// The response is already partially sent; do not append to it.
				return
			}
			writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		}()
		next.ServeHTTP(w, r)
	})
}

// multipartBodyOverhead is the fixed, bounded budget added on top of the
// single-file ceiling to cover multipart framing: boundaries, part headers and
// small non-file fields. Without it a file of exactly BOOP_MAX_UPLOAD_MB could
// never be uploaded, because its framing pushes the request body past the cap.
// The file itself is still limited to cfg.MaxUploadBytes() by readUpload and
// media.Store, and JSON handlers keep their own 64KiB / 512KiB ceilings.
const multipartBodyOverhead = 64 << 10

// limitBody caps every request body at the configured upload ceiling plus the
// multipart framing budget. Handlers that read a body surface the
// *http.MaxBytesError as a 413 response, and narrower per-handler limits (auth
// and content JSON) still apply inside it.
func limitBody(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func isHealthPath(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

// statusRecorder remembers the status code and byte count for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// Unwrap exposes the wrapped writer to http.ResponseController, so Flush,
// Hijack and deadline methods still reach the real connection through the
// middleware. Streaming responses (SSE) depend on it.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func remoteHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.TrimSpace(remoteAddr)
}
