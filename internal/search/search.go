// Package search owns Boop's keyword retrieval: query normalization, safe
// MATCH construction, deterministic snippets and stable cursor pagination over
// the public feed order.
//
// A query is answered through one of two paths, chosen by its script, because
// FTS5 has no CJK tokenizer. Everything after the query is shared: the same
// validation, the same `<published_at,id>` cursor, the same ordering, the same
// page size and the same snippet markers. See the comment on Search for why
// the split exists and what it costs.
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
	// substringLead and substringTail are the snippet context of the substring
	// path, in runes. That path cannot count tokens, because the whole reason it
	// exists is that CJK has no token boundaries, so its window is measured the
	// way its matching is: in characters. The two together stay far below
	// MaxSnippetRunes, which still clamps whatever comes out of either path.
	substringLead = 24
	substringTail = 56
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
	// Rank is FTS5's bm25 score of this row, and zero on the substring path,
	// which has no index to score with. It is informational only: pages are
	// ordered by the <published_at,id> cursor, so ranking can never reorder a
	// page boundary or duplicate a row, and the API does not expose it.
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
//
// The query then takes one of two paths, chosen by its script, because FTS5
// ships no CJK tokenizer. `unicode61` is the tokenizer Boop's index uses, and it
// treats a whole run of CJK - Han, kana, Hangul - as ONE token: the query 评论
// can never match the run 一段待评论的动态, while a Latin query matches
// normally. Such text is written without spaces, so no query rewriting can fix
// it: the indexed token boundaries are simply not there. A query carrying one of
// those characters is answered by substring matching over the three indexed
// columns instead of by the index.
//
// What that second path costs, stated plainly: it reads published rows rather
// than seeking an index, it has no bm25 score, and the Latin terms of a mixed
// query are matched as substrings too. What it buys is that Chinese search
// returns the right rows at all, with the same cursor, the same ordering and the
// same highlight markers as the indexed path. docs/DATABASE.md records the
// measured cost and the bigram index that would replace the scan if the corpus
// ever grows past what a scan can serve.
func Search(ctx context.Context, db *sql.DB, opts Options) (*Page, error) {
	if db == nil {
		return nil, errors.New("search: nil database")
	}
	limit, err := pageLimit(opts.Limit)
	if err != nil {
		return nil, err
	}
	terms, searchable, err := queryTerms(opts.Query)
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
	if hasCJK(terms) {
		return substringPage(ctx, db, terms, cursorTime, cursorID, limit)
	}
	return ftsPage(ctx, db, matchExpression(terms), cursorTime, cursorID, limit)
}

// ftsPage answers a query whose terms the index can match.
func ftsPage(ctx context.Context, db *sql.DB, expression, cursorTime string, cursorID int64, limit int) (*Page, error) {
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
		query += ` AND (p.published_at < ? OR (p.published_at = ? AND p.id < ?))`
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	// Ordering and the cursor read the bare column, exactly like content.Feed and
	// the RSS query: docs/DATABASE.md records the time invariant that a
	// status='published' row always carries a non-empty published_at, so no
	// COALESCE guard is needed. The select list keeps its COALESCE because it
	// pins the JSON shape for draft rows, and feed.go does the same.
	query += ` ORDER BY p.published_at DESC, p.id DESC LIMIT ?`
	// One extra row tells us whether another page exists without a count query.
	args = append(args, limit+1)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: query: %w", err)
	}
	return collect(rows, limit, scanFTS)
}

// scanFTS reads one indexed row, FTS5's own snippet and bm25 score included.
func scanFTS(rows *sql.Rows) (Result, error) {
	var result Result
	var snippet string
	if err := rows.Scan(&result.ID, &result.Slug, &result.Type, &result.Title, &result.Excerpt,
		&snippet, &result.PublishedAt, &result.UpdatedAt, &result.Rank); err != nil {
		return Result{}, fmt.Errorf("search: scan result: %w", err)
	}
	result.Snippet = snippet
	return result, nil
}

// substringPage answers a query the index cannot match by reading the three
// indexed columns directly. Every term must appear as a substring, exactly as
// the AND of quoted terms does on the FTS path, and the row filter, the cursor
// and the ordering are the ones the feed, the bookmarks list and RSS use.
//
// `LIKE` folds ASCII case by default, which is the same folding the FTS path
// applies to the Latin terms that can reach this branch. A term can never carry
// a LIKE wildcard, because queryTerms only yields letter and digit runs, so
// escapeLike is a guard rather than the active defence - and a test fails the
// moment that stops being true.
func substringPage(ctx context.Context, db *sql.DB, terms []string, cursorTime string, cursorID int64, limit int) (*Page, error) {
	query, args := substringQuery(terms, cursorTime, cursorID, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: query: %w", err)
	}
	return collect(rows, limit, scanSubstring(terms))
}

// substringQuery builds the statement the substring path runs. It is separate
// from the call that runs it so a test can EXPLAIN the real text: this query
// cannot seek, and the one thing a scan must still do is read through
// idx_posts_feed in cursor order instead of sorting the whole table.
func substringQuery(terms []string, cursorTime string, cursorID int64, limit int) (string, []any) {
	query := `SELECT p.id, p.slug, p.type, p.title, p.excerpt, p.body_markdown,
			COALESCE(p.published_at, ''), p.updated_at
		FROM posts p
		WHERE p.status = 'published' AND p.deleted_at IS NULL`
	args := make([]any, 0, 3*len(terms)+4)
	for _, term := range terms {
		pattern := "%" + escapeLike(term) + "%"
		query += ` AND (p.title LIKE ? ESCAPE '\' OR p.excerpt LIKE ? ESCAPE '\' OR p.body_markdown LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern, pattern)
	}
	if cursorID > 0 {
		// The same feed cursor as every other page query.
		query += ` AND (p.published_at < ? OR (p.published_at = ? AND p.id < ?))`
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query += ` ORDER BY p.published_at DESC, p.id DESC LIMIT ?`
	args = append(args, limit+1)
	return query, args
}

// scanSubstring reads one row of the substring path. It carries the Markdown
// body, which the FTS path never loads, because the snippet has to be built
// from the text the query matched against.
func scanSubstring(terms []string) func(*sql.Rows) (Result, error) {
	return func(rows *sql.Rows) (Result, error) {
		var result Result
		var body string
		if err := rows.Scan(&result.ID, &result.Slug, &result.Type, &result.Title, &result.Excerpt,
			&body, &result.PublishedAt, &result.UpdatedAt); err != nil {
			return Result{}, fmt.Errorf("search: scan result: %w", err)
		}
		result.Snippet = substringSnippet(terms, result.Title, result.Excerpt, body)
		return result, nil
	}
}

// collect drains a result set into one page plus the cursor that continues it.
// One extra row tells us whether another page exists without a count query, and
// the cursor is the last row this page returns - the same rule every other
// list in Boop follows.
func collect(rows *sql.Rows, limit int, scan func(*sql.Rows) (Result, error)) (*Page, error) {
	defer rows.Close()

	results := make([]Result, 0, limit)
	for rows.Next() {
		result, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result.Snippet = clampSnippet(result.Snippet)
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

// queryTerms validates a public query and splits it into letter/number terms in
// input order. Only Unicode letter/number runs become terms, and the script of
// those terms is what picks the retrieval path.
func queryTerms(raw string) ([]string, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if utf8.RuneCountInString(trimmed) > MaxQueryRunes {
		return nil, false, invalid("invalid_query", fmt.Sprintf("搜索关键词最多 %d 个字符", MaxQueryRunes))
	}
	terms := tokenize(trimmed)
	if len(terms) == 0 {
		return nil, false, nil
	}
	return terms, true, nil
}

// matchExpression quotes every term and joins them with AND. Quoting makes an
// FTS5 operator, a quote, a wildcard or a parenthesis in the input data instead
// of syntax, and bounding the terms is what keeps the expression bounded.
func matchExpression(terms []string) string {
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+term+`"`)
	}
	return strings.Join(quoted, " AND ")
}

// hasCJK reports whether any term contains a character from a script that
// unicode61 cannot split. Kana and Hangul are in the same position as Han
// ideographs: they are written without spaces, so one run is one token.
func hasCJK(terms []string) bool {
	for _, term := range terms {
		for _, r := range term {
			if unmergeable(r) {
				return true
			}
		}
	}
	return false
}

// unmergeable reports whether r belongs to a script whose words are not
// separated by spaces, which is exactly the set unicode61 keeps in one token.
func unmergeable(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x2EFF, // CJK radicals supplement
		r >= 0x3040 && r <= 0x30FF,   // Hiragana and Katakana
		r >= 0x3400 && r <= 0x4DBF,   // Han, extension A
		r >= 0x4E00 && r <= 0x9FFF,   // Han
		r >= 0xAC00 && r <= 0xD7AF,   // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF,   // Han compatibility ideographs
		r >= 0x20000 && r <= 0x2FA1F: // Han, extensions B onward
		return true
	}
	return false
}

// escapeLike makes a term literal inside a LIKE pattern. The backslash is
// escaped first, so the escapes added for % and _ are not themselves escaped,
// and both statements that build patterns outside this file pair it with
// ESCAPE '\'.
func escapeLike(term string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term)
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

// substringSnippet marks the first occurrence of whichever term a column
// carries, taking the columns in the order the FTS path would consider them:
// title, then excerpt, then the Markdown body. A result that matched only in
// its title has to show that hit rather than an unhighlighted body prefix.
func substringSnippet(terms []string, title, excerpt, body string) string {
	for _, column := range []string{title, excerpt, body} {
		for _, term := range terms {
			if marked, ok := markFirst(column, term); ok {
				return marked
			}
		}
	}
	return ""
}

// markFirst returns the column text around its first occurrence of term, with
// the hit between the two snippet markers and an ellipsis on whichever side was
// cut. It reuses the FTS path's markers, so the view needs no second code path
// and no second escaping rule.
func markFirst(text, term string) (string, bool) {
	index := foldIndex(text, term)
	if index < 0 {
		return "", false
	}
	runes := []rune(text)
	start := utf8.RuneCountInString(text[:index])
	length := utf8.RuneCountInString(term)

	from := start - substringLead
	headCut := from > 0
	if from < 0 {
		from = 0
	}
	to := start + length + substringTail
	tailCut := to < len(runes)
	if to > len(runes) {
		to = len(runes)
	}

	var b strings.Builder
	if headCut {
		b.WriteString("…")
	}
	b.WriteString(string(runes[from:start]))
	b.WriteString(SnippetOpen)
	b.WriteString(string(runes[start : start+length]))
	b.WriteString(SnippetClose)
	b.WriteString(string(runes[start+length : to]))
	if tailCut {
		b.WriteString("…")
	}
	return b.String(), true
}

// foldIndex is the first case-insensitive occurrence of term in text as a byte
// offset, or -1. Folding only happens when it preserves byte length: ToLower can
// change a string's length for a few code points, and a shifted offset would
// slice the snippet mid-rune, so those rare inputs keep the exact match.
func foldIndex(text, term string) int {
	if term == "" {
		return -1
	}
	if index := strings.Index(text, term); index >= 0 {
		return index
	}
	folded, foldedTerm := strings.ToLower(text), strings.ToLower(term)
	if len(folded) != len(text) || len(foldedTerm) != len(term) {
		return -1
	}
	return strings.Index(folded, foldedTerm)
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
