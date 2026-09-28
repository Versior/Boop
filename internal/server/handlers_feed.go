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

// rssContentType is the documented response type of the XML feed.
const rssContentType = "application/rss+xml; charset=utf-8"

// RSS bounds. The feed is the newest 50 published posts; every text field is
// collapsed to one line and bounded before the encoder sees it.
const (
	rssItemLimit        = 50
	rssTitleRunes       = 80
	rssDescriptionRunes = 300
)

// rssFallbackTitles keep a deterministic item title for a post that has neither
// a headline nor a usable first line.
var rssFallbackTitles = map[string]string{
	content.TypeMoment:  "新动态",
	content.TypeArticle: "新文章",
	content.TypePhoto:   "新摄影",
}

// rssFeed is the RSS 2.0 document root.
type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	Language      string    `xml:"language"`
	LastBuildDate string    `xml:"lastBuildDate,omitempty"`
	Items         []rssItem `xml:"item"`
}

type rssItem struct {
	Title string  `xml:"title"`
	Link  string  `xml:"link"`
	GUID  rssGUID `xml:"guid"`
	// PubDate is omitted when the stamp cannot be parsed instead of emitting an
	// empty element, which would make the feed invalid.
	PubDate     string `xml:"pubDate,omitempty"`
	Description string `xml:"description"`
}

// rssGUID is the item permalink, declared as a permalink so readers treat the
// GUID as a URL rather than as an opaque identifier.
type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

// rssPost is the narrow projection the feed needs: the Markdown body is rendered
// to description text, so the stored body_html, the assets, the tags and the
// public counters are never read and never loaded. The feed is a machine-readable
// copy of the newest headlines, not a page of the public feed.
type rssPost struct {
	Slug         string
	Type         string
	Title        string
	BodyMarkdown string
	Excerpt      string
	PublishedAt  string
}

// rssPosts reads at most limit published, undeleted posts in the documented
// public order, with one statement over the existing feed index.
func rssPosts(ctx context.Context, db *sql.DB, limit int) ([]rssPost, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT slug, type, title, body_markdown, excerpt, COALESCE(published_at, '')
		 FROM posts
		 WHERE status = 'published' AND deleted_at IS NULL
		 ORDER BY COALESCE(published_at, '') DESC, id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("rss: query: %w", err)
	}
	defer rows.Close()

	posts := make([]rssPost, 0, limit)
	for rows.Next() {
		var post rssPost
		if err := rows.Scan(&post.Slug, &post.Type, &post.Title, &post.BodyMarkdown,
			&post.Excerpt, &post.PublishedAt); err != nil {
			return nil, fmt.Errorf("rss: scan post: %w", err)
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rss: rows: %w", err)
	}
	return posts, nil
}

// handleFeed renders the RSS 2.0 feed of the newest published posts. Every link
// is built from BOOP_BASE_URL: the request Host and forwarded headers are never
// used, so a spoofed header cannot poison a reader's cache. The items come from a
// narrow projection of the newest posts instead of the full public feed page,
// because a reader needs headlines, links and a plain-text summary, not bodies,
// assets or tags.
func (s *server) handleFeed(w http.ResponseWriter, r *http.Request) {
	values := s.displaySettings(r)
	posts, err := rssPosts(r.Context(), s.db, rssItemLimit)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "rss feed failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}

	feed := rssFeed{
		Version: "2.0",
		Channel: rssChannel{
			Title:       values.SiteName,
			Link:        s.cfg.BaseURL + "/",
			Description: values.SiteDescription,
			Language:    "zh-CN",
			Items:       make([]rssItem, 0, len(posts)),
		},
	}
	for _, post := range posts {
		description, err := rssDescription(post)
		if err != nil {
			s.logger.LogAttrs(r.Context(), slog.LevelError, "rss description failed",
				slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
			writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			return
		}
		link := s.cfg.BaseURL + postPagePrefix + post.Slug
		feed.Channel.Items = append(feed.Channel.Items, rssItem{
			Title:       rssItemTitle(post),
			Link:        link,
			GUID:        rssGUID{Value: link, IsPermaLink: true},
			PubDate:     rssPubDate(post.PublishedAt),
			Description: description,
		})
	}
	if len(posts) > 0 {
		// The first item is the newest one, so it carries the build date.
		feed.Channel.LastBuildDate = rssPubDate(posts[0].PublishedAt)
	}

	body, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelError, "rss encode failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	w.Header().Set("Content-Type", rssContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(append([]byte(xml.Header), body...)); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "rss write failed", slog.String("error", err.Error()))
	}
}

// handleFeedMethodFallback answers a non-GET request on /feed.xml with the
// documented 405 and an Allow header. The feed is XML, so no JSON envelope is
// produced here.
func (s *server) handleFeedMethodFallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
}

// rssItemTitle is deterministic: the article title when the author wrote one,
// otherwise the bounded first non-empty line of the body, otherwise a type
// fallback for a post that carries no text at all.
func rssItemTitle(post rssPost) string {
	if title := strings.TrimSpace(post.Title); title != "" {
		return clampRunes(title, rssTitleRunes)
	}
	for _, line := range strings.Split(post.BodyMarkdown, "\n") {
		if line = oneLine(line); line != "" {
			return clampRunes(line, rssTitleRunes)
		}
	}
	if fallback, ok := rssFallbackTitles[post.Type]; ok {
		return fallback
	}
	return "新内容"
}

// rssDescription is plain text. The author's excerpt is used when there is one,
// because it is authored as plain text; otherwise an article body goes through
// the shared Markdown pipeline and only its visible text is kept, so Markdown
// syntax (headings, emphasis, link targets, code fences) and raw HTML never reach
// the reader. A moment or photo caption is plain text already and is folded to
// one line as-is. XML escaping is left to the encoder.
func rssDescription(post rssPost) (string, error) {
	if excerpt := strings.TrimSpace(post.Excerpt); excerpt != "" {
		return clampRunes(oneLine(excerpt), rssDescriptionRunes), nil
	}
	source := post.BodyMarkdown
	if post.Type == content.TypeArticle {
		text, err := content.MarkdownPlainText(post.BodyMarkdown)
		if err != nil {
			return "", fmt.Errorf("rss: render description of %s: %w", post.Slug, err)
		}
		source = text
	}
	return clampRunes(oneLine(source), rssDescriptionRunes), nil
}

// oneLine collapses every whitespace run so a stored multi-line body becomes a
// single deterministic line.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// clampRunes bounds text on a rune boundary, so a multi-byte character is never
// cut in half.
func clampRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// rssPubDate renders a stored timestamp as RFC1123Z in UTC, or an empty string
// when it cannot be parsed.
func rssPubDate(stamp string) string {
	parsed, err := time.Parse(content.TimestampFormat, stamp)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC1123Z)
}
