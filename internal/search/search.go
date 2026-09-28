// Package search owns Boop's keyword retrieval over the existing FTS5 index:
// query normalization, safe MATCH construction, deterministic snippets and
// stable cursor pagination over the public feed order.
//
// The concrete Result/Page/Options values are the v0.2 retrieval seam: a later
// semantic or AI answer path can consume the same shape, so no interface and no
// provider abstraction is needed for the single implementation that exists.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"boop/internal/content"
)

// Query and snippet bounds. docs/API.md documents the public ones.
const (
	// MaxQueryRunes caps the accepted public query so the generated MATCH
	// expression stays bounded (at most 50 quoted terms) and an absurd input is
	// an explicit invalid_query instead of wasted work.
	MaxQueryRunes = 100
	// MaxSnippetRunes bounds the fragment handed to the API and the SSR view.
	// FTS5 counts tokens, not characters, so one CJK run can be a long token.
	MaxSnippetRunes = 240
	// SnippetOpen and SnippetClose are the FTS5 highlight markers. They are
	// control characters, so stored post text only produces them by accident,
	// and they never become markup: the SSR view splits the snippet on these
	// markers into plain-text parts, escapes every part and lets the template
	// emit its own <mark> tags.
	SnippetOpen  = "\x02"
	SnippetClose = "\x03"
	// snippetWindow is the FTS5 snippet width in tokens.
	snippetWindow = 16
)

// Result is the minimum reusable retrieval shape of one matching post: enough
// for the search stream and for a later AI answer, and deliberately free of
// bodies, assets, tags, reactions and comments.
type Result struct {
	ID          int64
	Slug        string
	Type        string
	Title       string
	Excerpt     string
	Snippet     string
	PublishedAt string
	UpdatedAt   string
	// Rank is FTS5's bm25 score of this row. It is informational only: pages are
	// ordered by the <published_at,id> cursor, so ranking can never reorder a
	// page boundary or duplicate a row.
	Rank float64
}

// Options is one public search request. Query is the raw user input; Cursor is
// the <published_at,id> cursor of the previous page.
type Options struct {
	Query  string
	Cursor string
	Limit  int
}

// Page is one page of results plus the cursor that continues it.
type Page struct {
	Results    []Result
	NextCursor string
}

// ValidationError carries the machine code and the user-facing message of a
// rejected request so the HTTP layer stays a thin translation.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("search: %s: %s", e.Code, e.Message)
}

func invalid(code, message string) *ValidationError {
	return &ValidationError{Code: code, Message: message}
}

// Search runs one page of keyword retrieval. A blank or punctuation-only query
// is not an error: it returns an empty page without executing any MATCH
// expression, because `MATCH ”` and `MATCH '!!!'` are SQL syntax errors rather
// than "no results".
func Search(ctx context.Context, db *sql.DB, opts Options) (*Page, error) {
	if db == nil {
		return nil, errors.New("search: nil database")
	}
	limit, err := pageLimit(opts.Limit)
	if err != nil {
		return nil, err
	}
	expression, searchable, err := matchExpression(opts.Query)
	if err != nil {
		return nil, err
	}
	if !searchable {
		return &Page{}, nil
	}
	cursorTime, cursorID, err := parseCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}

	// The snippet column is -1, so FTS5 fragments the indexed column with the best
	// match (title, body or excerpt) instead of a fixed one: pinning the Markdown
	// body would return an unhighlighted body prefix for a post that only matched
	// in its title or excerpt, and the stream would show no evidence of the hit.
	query := `SELECT p.id, p.slug, p.type, p.title, p.excerpt,
			snippet(post_search, -1, ?, ?, '…', ?),
			COALESCE(p.published_at, ''), p.updated_at, bm25(post_search)
		FROM post_search JOIN posts p ON p.id = post_search.rowid
		WHERE post_search MATCH ?
			AND p.status = 'published' AND p.deleted_at IS NULL`
	args := []any{SnippetOpen, SnippetClose, snippetWindow, expression}
	if cursorID > 0 {
		// The documented feed cursor: matching results are ordered by the same
		// columns as the public feed, so pagination cannot skip or repeat a row.
		query += ` AND (COALESCE(p.published_at, '') < ? OR (COALESCE(p.published_at, '') = ? AND p.id < ?))`
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query += ` ORDER BY COALESCE(p.published_at, '') DESC, p.id DESC LIMIT ?`
	// One extra row tells us whether another page exists without a count query.
	args = append(args, limit+1)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: query: %w", err)
	}
	defer rows.Close()

	results := make([]Result, 0, limit)
	for rows.Next() {
		var result Result
		var snippet string
		if err := rows.Scan(&result.ID, &result.Slug, &result.Type, &result.Title, &result.Excerpt,
			&snippet, &result.PublishedAt, &result.UpdatedAt, &result.Rank); err != nil {
			return nil, fmt.Errorf("search: scan result: %w", err)
		}
		result.Snippet = clampSnippet(snippet)
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: rows: %w", err)
	}

	page := &Page{}
	if len(results) > limit {
		results = results[:limit]
		last := results[len(results)-1]
		page.NextCursor = content.EncodeCursor(last.PublishedAt, last.ID)
	}
	page.Results = results
	return page, nil
}

// matchExpression turns a public query into a bounded FTS5 MATCH expression and
// reports whether anything searchable survived. Only Unicode letter/number runs
// become terms and every term is quoted, so an FTS5 operator, a quote, a
// wildcard or a parenthesis in the input is data instead of syntax.
func matchExpression(raw string) (string, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if utf8.RuneCountInString(trimmed) > MaxQueryRunes {
		return "", false, invalid("invalid_query", fmt.Sprintf("搜索关键词最多 %d 个字符", MaxQueryRunes))
	}
	terms := tokenize(trimmed)
	if len(terms) == 0 {
		return "", false, nil
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+term+`"`)
	}
	return strings.Join(quoted, " AND "), true, nil
}

// tokenize splits the query into letter/number terms in input order, dropping
// duplicates case-insensitively so "Go go" is one term.
func tokenize(query string) []string {
	var (
		terms   []string
		seen    = make(map[string]bool)
		current []rune
	)
	flush := func() {
		if len(current) == 0 {
			return
		}
		term := string(current)
		current = current[:0]
		key := strings.ToLower(term)
		if seen[key] {
			return
		}
		seen[key] = true
		terms = append(terms, term)
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	return terms
}

// clampSnippet bounds a snippet by runes and keeps its highlight markers
// balanced, so a truncated fragment never opens a highlight it does not close.
func clampSnippet(raw string) string {
	trimmed := strings.TrimSpace(raw)
	runes := []rune(trimmed)
	if len(runes) <= MaxSnippetRunes {
		return trimmed
	}
	clamped := string(runes[:MaxSnippetRunes])
	if strings.Count(clamped, SnippetOpen) > strings.Count(clamped, SnippetClose) {
		clamped += SnippetClose
	}
	return clamped
}

func pageLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return content.DefaultPageSize, nil
	case limit < 0 || limit > content.MaxPageSize:
		return 0, invalid("invalid_limit", fmt.Sprintf("每页数量需在 1 到 %d 之间", content.MaxPageSize))
	default:
		return limit, nil
	}
}

// parseCursor reuses the public feed cursor so search pages read the same
// <published_at,id> format as every other list, and translates its error into
// this package's one error type.
func parseCursor(cursor string) (string, int64, error) {
	if cursor == "" {
		return "", 0, nil
	}
	at, id, err := content.DecodeCursor(cursor)
	if err == nil {
		return at, id, nil
	}
	var invalidCursor *content.ValidationError
	if errors.As(err, &invalidCursor) {
		return "", 0, invalid(invalidCursor.Code, invalidCursor.Message)
	}
	return "", 0, err
}
