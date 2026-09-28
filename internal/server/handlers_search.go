package server

import (
	"errors"
	stdhtml "html"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"boop/internal/search"
)

// navSearchFilter marks the 搜索 section as current in the shell.
const navSearchFilter = "search"

// searchResultPayload is the JSON shape of one result: the same minimum
// retrieval fields the SSR stream renders, without the internal bm25 rank.
type searchResultPayload struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Excerpt     string `json:"excerpt"`
	Snippet     string `json:"snippet"`
	URL         string `json:"url"`
	PublishedAt string `json:"published_at"`
	UpdatedAt   string `json:"updated_at"`
}

// searchCard is one compact result row of the SSR stream. Snippet is the only
// HTML the page trusts, and only because highlightSnippet escaped the stored
// text before turning the search package's markers into <mark> tags.
type searchCard struct {
	Type      string
	TypeLabel string
	Title     string
	Excerpt   string
	Snippet   template.HTML
	URL       string
	Datetime  string
	TimeLabel string
}

// searchView drives the search page: the query, the result count context, the
// rows and the same-origin link that continues them.
type searchView struct {
	pageView
	Query       string
	ResultCount int
	Results     []searchCard
	LoadMoreURL string
	// Prompted is true for a blank query: the page invites a search instead of
	// reporting a zero-result state, so an empty search box is never an error.
	Prompted bool
	// Empty is true for a real query without matches, including a
	// punctuation-only one, which is reported exactly like "no results".
	Empty bool
}

// handleSearchAPI serves keyword retrieval as JSON (docs/API.md).
func (s *server) handleSearchAPI(w http.ResponseWriter, r *http.Request) {
	limit, ok := s.pageLimitParam(w, r)
	if !ok {
		return
	}
	page, err := search.Search(r.Context(), s.db, search.Options{
		Query:  r.URL.Query().Get("q"),
		Cursor: r.URL.Query().Get("cursor"),
		Limit:  limit,
	})
	if err != nil {
		s.writeSearchFailure(w, r, "search api", err)
		return
	}
	payload := make([]searchResultPayload, 0, len(page.Results))
	for _, result := range page.Results {
		payload = append(payload, searchResultPayload{
			ID:          result.ID,
			Slug:        result.Slug,
			Type:        result.Type,
			Title:       result.Title,
			Excerpt:     result.Excerpt,
			Snippet:     result.Snippet,
			URL:         postPagePrefix + result.Slug,
			PublishedAt: result.PublishedAt,
			UpdatedAt:   result.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload, "next_cursor": page.NextCursor})
}

// handleSearchPage renders the search stream inside the center column.
func (s *server) handleSearchPage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	values := s.displaySettings(r)
	page, err := search.Search(r.Context(), s.db, search.Options{
		Query:  query,
		Cursor: r.URL.Query().Get("cursor"),
		Limit:  values.PageSize,
	})
	if err != nil {
		s.writeSearchFailure(w, r, "search page", err)
		return
	}
	location := loadLocation(values.SiteTimezone)

	view := searchView{
		pageView: s.shellViewWithSettings(r, navSearchFilter, values),
		Query:    query,
		Prompted: strings.TrimSpace(query) == "",
		Results:  make([]searchCard, 0, len(page.Results)),
	}
	for _, result := range page.Results {
		datetime, label := displayTime(result.PublishedAt, location)
		view.Results = append(view.Results, searchCard{
			Type:      result.Type,
			TypeLabel: typeLabels[result.Type],
			Title:     result.Title,
			Excerpt:   result.Excerpt,
			Snippet:   highlightSnippet(result.Snippet),
			URL:       postPagePrefix + result.Slug,
			Datetime:  datetime,
			TimeLabel: label,
		})
	}
	view.ResultCount = len(view.Results)
	view.Empty = !view.Prompted && view.ResultCount == 0
	// The right rail repeats the current query; the center form stays the one
	// that submits it.
	view.SearchQuery = query
	if page.NextCursor != "" {
		view.LoadMoreURL = searchURL(query, page.NextCursor)
	}
	s.render(w, r, http.StatusOK, "search", view)
}

// highlightSnippet produces the only HTML the search stream trusts: the stored
// fragment is escaped first, and only then are the search package's two internal
// markers replaced. FTS output and post text are therefore never trusted HTML.
func highlightSnippet(snippet string) template.HTML {
	escaped := stdhtml.EscapeString(snippet)
	escaped = strings.ReplaceAll(escaped, search.SnippetOpen, "<mark>")
	escaped = strings.ReplaceAll(escaped, search.SnippetClose, "</mark>")
	return template.HTML(escaped) //nolint:gosec // escaped above; only the trusted markers become tags
}

// searchURL builds a same-origin link that preserves the query, so "加载更多"
// never string-concatenates a value into a URL.
func searchURL(query, cursor string) string {
	params := url.Values{}
	if query != "" {
		params.Set("q", query)
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	if len(params) == 0 {
		return "/search"
	}
	return "/search?" + params.Encode()
}

// handleSearchFallback keeps every /api/v1/search answer JSON: a wrong method on
// the collection is a 405 with Allow, an unknown sub-path is a 404.
func (s *server) handleSearchFallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/search" {
		w.Header().Set("Allow", http.MethodGet)
		writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
		return
	}
	writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
}

// writeSearchFailure maps a search error onto the documented envelope.
func (s *server) writeSearchFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var invalid *search.ValidationError
	switch {
	case errors.As(err, &invalid):
		writeFailure(w, r, http.StatusBadRequest, invalid.Code, invalid.Message)
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, operation+" failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	}
}
