package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"boop/internal/store"
)

// ---------- fixtures ----------

var searchNow = time.Date(2026, 9, 28, 6, 2, 3, 0, time.UTC)

type fixture struct {
	t  *testing.T
	db *sql.DB
}

// newFixture opens a migrated database, which is what the FTS triggers and the
// posts table need.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return &fixture{t: t, db: db}
}

// newUnmigratedDB is a database without any schema: it proves a query that must
// not reach SQLite, because a statement against it can only fail.
func newUnmigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// insert writes one post row directly. The search package only reads posts, and
// the migration triggers keep post_search in sync, so no request or owner
// account is needed here.
func (f *fixture) insert(slug, kind, status, title, body, excerpt, publishedAt string) int64 {
	f.t.Helper()
	var published any
	if publishedAt != "" {
		published = publishedAt
	}
	stamp := searchNow.Format(time.RFC3339)
	res, err := f.db.Exec(
		`INSERT INTO posts(slug, type, status, title, body_markdown, body_html, excerpt, published_at, created_at, updated_at)
		 VALUES(?,?,?,?,?,'',?,?,?,?)`,
		slug, kind, status, title, body, excerpt, published, stamp, stamp)
	if err != nil {
		f.t.Fatalf("insert post %s: %v", slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatalf("post id: %v", err)
	}
	return id
}

func (f *fixture) delete(slug string) {
	f.t.Helper()
	if _, err := f.db.Exec(`UPDATE posts SET deleted_at = ? WHERE slug = ?`,
		searchNow.Format(time.RFC3339), slug); err != nil {
		f.t.Fatalf("soft delete %s: %v", slug, err)
	}
}

func (f *fixture) search(opts Options) *Page {
	f.t.Helper()
	page, err := Search(context.Background(), f.db, opts)
	if err != nil {
		f.t.Fatalf("Search(%+v): %v", opts, err)
	}
	return page
}

func slugsOf(page *Page) []string {
	slugs := make([]string, 0, len(page.Results))
	for _, result := range page.Results {
		slugs = append(slugs, result.Slug)
	}
	return slugs
}

func assertValidation(t *testing.T, err error, code string) {
	t.Helper()
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v (%T), want a ValidationError with code %q", err, err, code)
	}
	if invalid.Code != code {
		t.Fatalf("code = %q, want %q", invalid.Code, code)
	}
	if invalid.Message == "" {
		t.Error("validation error has no user-facing message")
	}
}

// ---------- query normalization ----------

func TestMatchExpressionQuotesTermsAndDropsSyntax(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		want       string
		searchable bool
	}{
		{"blank", "", "", false},
		{"spaces only", "   \t ", "", false},
		{"punctuation only", "!!!??? ---", "", false},
		{"operators only", `"*()^:`, "", false},
		{"single word", "Go", `"Go"`, true},
		{"two words", "hello world", `"hello" AND "world"`, true},
		{"quote injection", `foo" OR "bar`, `"foo" AND "OR" AND "bar"`, true},
		{"unterminated quote", `"unterminated`, `"unterminated"`, true},
		{"wildcard and grouping", "alpha* (beta) -gamma:delta", `"alpha" AND "beta" AND "gamma" AND "delta"`, true},
		{"operator keywords", "NEAR(alpha beta)", `"NEAR" AND "alpha" AND "beta"`, true},
		{"duplicates folded", "Go go GO", `"Go"`, true},
		{"cjk terms", "写作 计划", `"写作" AND "计划"`, true},
		{"mixed script", "Go 语言 1.27", `"Go" AND "语言" AND "1" AND "27"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expression, searchable, err := matchExpression(tc.query)
			if err != nil {
				t.Fatalf("matchExpression(%q): %v", tc.query, err)
			}
			if searchable != tc.searchable {
				t.Fatalf("searchable = %v, want %v", searchable, tc.searchable)
			}
			if expression != tc.want {
				t.Errorf("expression = %q, want %q", expression, tc.want)
			}
		})
	}
}

func TestMatchExpressionRejectsOverlongQueries(t *testing.T) {
	atLimit := strings.Repeat("字", MaxQueryRunes)
	if _, searchable, err := matchExpression(atLimit); err != nil || !searchable {
		t.Fatalf("query at the limit: searchable = %v, err = %v", searchable, err)
	}

	_, _, err := matchExpression(strings.Repeat("字", MaxQueryRunes+1))
	assertValidation(t, err, "invalid_query")
}

// ---------- hostile input never becomes MATCH syntax ----------

func TestSearchTreatsOperatorsAsData(t *testing.T) {
	f := newFixture(t)
	f.insert("alpha", "moment", "published", "", "alpha gamma", "", "2026-01-02T00:00:00Z")
	f.insert("beta", "moment", "published", "", "beta delta", "", "2026-01-01T00:00:00Z")

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"plain term matches", "alpha", 1},
		{"quoted injection is not an OR", `alpha" OR "beta`, 0},
		{"operator keywords are terms", "NEAR(alpha beta)", 0},
		{"negation is data", "-alpha", 1},
		{"caret is data", "^alpha", 1},
		{"wildcard is data", "alph*", 0},
		{"both terms required", "alpha gamma", 1},
		{"second term missing", "alpha missing", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := f.search(Options{Query: tc.query})
			if len(page.Results) != tc.want {
				t.Fatalf("results = %v, want %d", slugsOf(page), tc.want)
			}
		})
	}
}

func TestBlankAndPunctuationOnlyQueriesNeverReachSQLite(t *testing.T) {
	db := newUnmigratedDB(t)
	ctx := context.Background()

	for _, query := range []string{"", "   ", "!!!???", "*()", "-"} {
		page, err := Search(ctx, db, Options{Query: query})
		if err != nil {
			t.Fatalf("Search(%q) on an empty database: %v", query, err)
		}
		if len(page.Results) != 0 || page.NextCursor != "" {
			t.Errorf("Search(%q) = %+v, want an empty page", query, page)
		}
	}

	// The same database proves a real query does execute SQL, so the check above
	// is about skipping MATCH, not about an inert database.
	if _, err := Search(ctx, db, Options{Query: "alpha"}); err == nil {
		t.Fatal("a searchable query on an unschematized database did not fail")
	}
}

// ---------- matching ----------

func TestSearchMatchesUnicodeAndCJK(t *testing.T) {
	f := newFixture(t)
	f.insert("go", "article", "published", "Go 并发实践", "关于 写作 与 计划 的内容", "", "2026-01-03T00:00:00Z")
	f.insert("diary", "moment", "published", "", "今天 写作 很顺利", "", "2026-01-02T00:00:00Z")
	f.insert("other", "moment", "published", "", "与搜索无关 的内容", "", "2026-01-01T00:00:00Z")

	cases := []struct {
		query string
		want  []string
	}{
		{"写作", []string{"go", "diary"}},
		{"写作 计划", []string{"go"}},
		{"并发实践", []string{"go"}},
		{"与搜索无关", []string{"other"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			page := f.search(Options{Query: tc.query})
			got := slugsOf(page)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("results = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSearchExcludesDraftArchivedAndDeletedPosts(t *testing.T) {
	f := newFixture(t)
	f.insert("published", "moment", "published", "", "关键词 内容", "", "2026-01-04T00:00:00Z")
	f.insert("draft", "moment", "draft", "", "关键词 内容", "", "")
	f.insert("archived", "moment", "archived", "", "关键词 内容", "", "2026-01-03T00:00:00Z")
	f.insert("removed", "moment", "published", "", "关键词 内容", "", "2026-01-02T00:00:00Z")
	f.delete("removed")

	page := f.search(Options{Query: "关键词"})
	if got := slugsOf(page); fmt.Sprint(got) != fmt.Sprint([]string{"published"}) {
		t.Errorf("results = %v, want only the published post", got)
	}
}

// ---------- snippets ----------

func TestSnippetIsPlainMarkedText(t *testing.T) {
	f := newFixture(t)
	f.insert("html", "article", "published", "标题",
		`<script>alert(1)</script> 关键词 与 其它内容`, "", "2026-01-01T00:00:00Z")

	page := f.search(Options{Query: "关键词"})
	if len(page.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(page.Results))
	}
	snippet := page.Results[0].Snippet
	if !strings.Contains(snippet, SnippetOpen+"关键词"+SnippetClose) {
		t.Errorf("snippet %q does not mark the match", snippet)
	}
	// The fragment stays the stored plain text: escaping is the view's job, so a
	// source tag is still a literal tag here.
	if !strings.Contains(snippet, `<script>`) {
		t.Errorf("snippet %q pre-escaped or dropped the stored text", snippet)
	}

	body := "关键词 " + strings.Repeat("很长的一段中文内容", 200)
	f.insert("long", "moment", "published", "", body, "", "2026-01-02T00:00:00Z")
	var long string
	for _, result := range f.search(Options{Query: "关键词"}).Results {
		if result.Slug == "long" {
			long = result.Snippet
		}
	}
	if long == "" {
		t.Fatal("the long post is missing from the results")
	}
	if runes := utf8.RuneCountInString(long); runes > MaxSnippetRunes+1 {
		t.Errorf("snippet has %d runes, want at most %d", runes, MaxSnippetRunes+1)
	}
	if open, closed := strings.Count(long, SnippetOpen), strings.Count(long, SnippetClose); open != closed {
		t.Errorf("snippet %q has %d open and %d close markers", long, open, closed)
	}
}

// A match that only lives in the title or the excerpt must still produce a
// fragment that shows it: the snippet is taken from whichever indexed column
// matches best, not from a fixed body column that would render an unhighlighted
// body prefix and hide the reason the row was returned.
func TestSnippetUsesTheBestMatchingColumn(t *testing.T) {
	f := newFixture(t)
	f.insert("title-hit", "article", "published", "独特标记词 标题", "正文里没有这个词。", "", "2026-01-03T00:00:00Z")
	f.insert("excerpt-hit", "article", "published", "普通标题", "正文里没有这个词。", "摘要里提到 独特标记词", "2026-01-02T00:00:00Z")
	f.insert("body-hit", "article", "published", "普通标题", "正文里有 独特标记词。", "摘要里没有。", "2026-01-01T00:00:00Z")

	page := f.search(Options{Query: "独特标记词"})
	if len(page.Results) != 3 {
		t.Fatalf("results = %v, want all three columns to match", slugsOf(page))
	}
	for _, result := range page.Results {
		if !strings.Contains(result.Snippet, SnippetOpen+"独特标记词"+SnippetClose) {
			t.Errorf("snippet of %s = %q, want the matched term from its own column", result.Slug, result.Snippet)
		}
	}
}

// ---------- pagination ----------

func TestSearchPaginatesByPublishedAtAndID(t *testing.T) {
	f := newFixture(t)
	// Five posts sharing one timestamp: the id is the only tie-breaker, so the
	// cursor has to carry both values.
	for i := 1; i <= 5; i++ {
		f.insert(fmt.Sprintf("post-%d", i), "moment", "published", "", "分页 内容", "", "2026-02-01T00:00:00Z")
	}

	var (
		seen    []string
		cursor  string
		pages   int
		visited = map[string]bool{}
	)
	for {
		page := f.search(Options{Query: "分页", Cursor: cursor, Limit: 2})
		for _, slug := range slugsOf(page) {
			if visited[slug] {
				t.Fatalf("slug %q appeared twice across pages", slug)
			}
			visited[slug] = true
			seen = append(seen, slug)
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
	}

	want := []string{"post-5", "post-4", "post-3", "post-2", "post-1"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("pagination order = %v, want %v", seen, want)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
}

func TestSearchPageCursorMatchesItsLastResult(t *testing.T) {
	f := newFixture(t)
	f.insert("newer", "moment", "published", "", "游标 内容", "", "2026-03-02T00:00:00Z")
	f.insert("older", "moment", "published", "", "游标 内容", "", "2026-03-01T00:00:00Z")

	first := f.search(Options{Query: "游标", Limit: 1})
	if len(first.Results) != 1 || first.Results[0].Slug != "newer" {
		t.Fatalf("first page = %v", slugsOf(first))
	}
	at, id, err := parseCursor(first.NextCursor)
	if err != nil {
		t.Fatalf("parseCursor(%q): %v", first.NextCursor, err)
	}
	if at != first.Results[0].PublishedAt || id != first.Results[0].ID {
		t.Errorf("cursor = %q, want the last result's <published_at,id>", first.NextCursor)
	}

	second := f.search(Options{Query: "游标", Cursor: first.NextCursor, Limit: 1})
	if got := slugsOf(second); fmt.Sprint(got) != fmt.Sprint([]string{"older"}) {
		t.Errorf("second page = %v, want [older]", got)
	}
	if second.NextCursor != "" {
		t.Errorf("next cursor of the last page = %q, want empty", second.NextCursor)
	}
}

// ---------- validation ----------

func TestSearchRejectsInvalidOptions(t *testing.T) {
	f := newFixture(t)
	f.insert("alpha", "moment", "published", "", "alpha", "", "2026-01-01T00:00:00Z")

	cases := []struct {
		name string
		opts Options
		code string
	}{
		{"limit above the maximum", Options{Query: "alpha", Limit: 51}, "invalid_limit"},
		{"negative limit", Options{Query: "alpha", Limit: -1}, "invalid_limit"},
		{"cursor without an id", Options{Query: "alpha", Cursor: "nope"}, "invalid_cursor"},
		{"cursor with a zero id", Options{Query: "alpha", Cursor: "2026-01-01T00:00:00Z,0"}, "invalid_cursor"},
		{"cursor with a broken stamp", Options{Query: "alpha", Cursor: "yesterday,1"}, "invalid_cursor"},
		{"overlong query", Options{Query: strings.Repeat("词", MaxQueryRunes+1)}, "invalid_query"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Search(context.Background(), f.db, tc.opts)
			if err == nil {
				t.Fatalf("Search(%+v) succeeded, want %s", tc.opts, tc.code)
			}
			assertValidation(t, err, tc.code)
		})
	}

	if _, err := Search(context.Background(), f.db, Options{Query: "alpha", Limit: 50}); err != nil {
		t.Errorf("limit at the maximum was rejected: %v", err)
	}
}

func TestSearchRequiresADatabase(t *testing.T) {
	if _, err := Search(context.Background(), nil, Options{Query: "alpha"}); err == nil {
		t.Fatal("Search with a nil database succeeded")
	}
}
