package server

import (
	"errors"
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

// snippetPart is one run of a search snippet. Text is always plain text, and Mark
// only tells the template whether this run is a match: the markup is static in
// the template and every part is escaped by html/template, so neither FTS output
// nor stored post text is ever trusted as HTML.
type snippetPart struct {
	Text string
	Mark bool
}

// searchCard is one compact result row of the SSR stream. Snippet carries the
// fragment as escaped-by-the-template plain text, never as HTML.
type searchCard struct {
	Type      string
	TypeLabel string
	Title     string
	Excerpt   string
	Snippet   []snippetPart
	URL       string
	Datetime  string
	TimeLabel string
}

// searchView drives the search page: the query, the result count context, the
// rows and the same-origin link that continues them.
type searchView struct {
	pageView
	Query string
	// ResultCount is how many rows this page carries, never how many a site-wide
	// query would match: the page has no COUNT statement behind it, so it must not
	// present its own length as a total.
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
			URL:         postPath(result.Slug),
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
			Snippet:   snippetParts(result.Snippet),
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

// snippetParts splits a stored snippet on the two search markers and returns
// plain-text runs, so the template decides the markup. A marker that survives in
// the stored text (a body containing U+0002 or U+0003) can therefore only open or
// close a highlight; it can neither inject HTML nor leave an unbalanced tag,
// because every run is escaped and every <mark> is emitted by the template.
func snippetParts(snippet string) []snippetPart {
	if snippet == "" {
		return nil
	}
	parts := make([]snippetPart, 0, 3)
	marked := false
	for len(snippet) > 0 {
		// The markers are single-byte control characters, so the byte index of the
		// earliest one is also its rune boundary.
		index := strings.IndexAny(snippet, search.SnippetOpen+search.SnippetClose)
		if index < 0 {
			break
		}
		if text := snippet[:index]; text != "" {
			parts = append(parts, snippetPart{Text: text, Mark: marked})
		}
		marked = snippet[index] == search.SnippetOpen[0]
		snippet = snippet[index+1:]
	}
	if snippet != "" {
		parts = append(parts, snippetPart{Text: snippet, Mark: marked})
	}
	return parts
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
