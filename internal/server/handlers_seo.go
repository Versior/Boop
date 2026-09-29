package server

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"boop/internal/content"
)

// The two machine-readable discovery documents. Both are plain text rather than
// a rendered page, so neither goes through html/template.
const (
	robotsContentType  = "text/plain; charset=utf-8"
	sitemapContentType = "application/xml; charset=utf-8"
)

// sitemapLimit is the per-file ceiling the sitemap protocol defines. A personal
// blog does not approach it; the bound exists so one runaway table cannot make
// this handler build an unbounded document on a 500MB machine.
const sitemapLimit = 50000

// robotsDisallow are the paths that carry no value for a crawler. Rules are
// matched by longest prefix, so the single "Allow: /" written below is what
// keeps every other path crawlable while these stay out.
//
// /admin, /api and /auth are the private surface. /bookmarks is per-visitor.
// /login and /register are forms that only make sense to a human.
var robotsDisallow = []string{
	"/admin",
	"/api/",
	"/auth/",
	"/bookmarks",
	"/login",
	"/register",
}

// sitemapURLSet is the sitemap protocol document root.
type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

// sitemapPost is the narrow projection the sitemap needs: a location and a
// modification time. Bodies, assets, tags and counters are never read.
type sitemapPost struct {
	Slug      string
	UpdatedAt string
}

// sitemapQuery reads every published post in the documented public order. The
// ORDER BY is the expression-free one idx_posts_feed provides, exactly like the
// feed query, so the document is produced in index order without a temporary
// B-tree over the whole table.
const sitemapQuery = `SELECT slug, updated_at
	FROM posts
	WHERE status = 'published' AND deleted_at IS NULL
	ORDER BY published_at DESC, id DESC
	LIMIT ?`

func sitemapPosts(ctx context.Context, db *sql.DB, limit int) ([]sitemapPost, error) {
	rows, err := db.QueryContext(ctx, sitemapQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("sitemap: query: %w", err)
	}
	defer rows.Close()

	posts := make([]sitemapPost, 0, limit)
	for rows.Next() {
		var post sitemapPost
		if err := rows.Scan(&post.Slug, &post.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sitemap: scan post: %w", err)
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sitemap: rows: %w", err)
	}
	return posts, nil
}

// handleRobots answers /robots.txt. The sitemap has to be advertised with an
// absolute URL, and that URL is built from BOOP_BASE_URL: the request Host and
// the forwarded headers are never read, because a spoofed header would let a
// caller make this process advertise somebody else's origin to every crawler.
func (s *server) handleRobots(w http.ResponseWriter, r *http.Request) {
	var builder strings.Builder
	builder.WriteString("User-agent: *\n")
	builder.WriteString("Allow: /\n")
	for _, path := range robotsDisallow {
		builder.WriteString("Disallow: ")
		builder.WriteString(path)
		builder.WriteString("\n")
	}
	builder.WriteString("\nSitemap: ")
	builder.WriteString(s.cfg.BaseURL)
	builder.WriteString("/sitemap.xml\n")

	w.Header().Set("Content-Type", robotsContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(builder.String())); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "robots write failed", slog.String("error", err.Error()))
	}
}

// handleSitemap answers /sitemap.xml with every published post, not with a
// hardcoded handful of paths: a sitemap that lists the index and stops is worse
// than no sitemap, because a crawler that trusts it never learns the posts
// exist. Each entry carries the post's own lastmod so an edited post is
// re-fetched without the whole file having to look new.
//
// The home page carries no lastmod on purpose. It renders a bounded page of the
// newest posts, so no single stored timestamp describes when its content last
// changed, and inventing one would either over- or under-claim.
//
// The two filtered home views (/?type=article and /?type=photo) are left out:
// they are subsets of the index and sit one click away from it, so listing them
// would only add near-duplicate URLs to a crawler's queue.
func (s *server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	posts, err := sitemapPosts(r.Context(), s.db, sitemapLimit)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "sitemap failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}

	document := sitemapURLSet{
		XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9",
		URLs:  make([]sitemapURL, 0, len(posts)+1),
	}
	document.URLs = append(document.URLs, sitemapURL{Loc: s.cfg.BaseURL + "/"})
	for _, post := range posts {
		document.URLs = append(document.URLs, sitemapURL{
			Loc:     s.postURL(post.Slug),
			LastMod: sitemapLastMod(post.UpdatedAt),
		})
	}

	body, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "sitemap encode failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	w.Header().Set("Content-Type", sitemapContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(append([]byte(xml.Header), body...)); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "sitemap write failed", slog.String("error", err.Error()))
	}
}

// sitemapLastMod renders a stored timestamp as a W3C datetime in UTC, or an
// empty string when it cannot be parsed: the element is then omitted rather than
// emitted empty, which would make the document invalid.
func sitemapLastMod(stamp string) string {
	parsed, err := time.Parse(content.TimestampFormat, stamp)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

// handleSEOMethodFallback answers a non-GET request on the discovery documents
// with the documented 405 and an Allow header. Both are plain text, so no JSON
// envelope is produced here.
func (s *server) handleSEOMethodFallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
}
