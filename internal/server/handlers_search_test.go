package server

import (
	"context"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"boop/internal/content"
	"boop/internal/settings"
)

// ---------- shared post fixtures ----------

// insertPost writes one post row directly. The search page, the search API and
// the RSS feed only read published rows, and the migration triggers keep the FTS
// index in sync, so no owner session or HTTP create is involved and a test can
// control status and published_at exactly.
func (f *authFixture) insertPost(t *testing.T, slug, kind, status, title, body, excerpt, publishedAt string) int64 {
	t.Helper()
	var published any
	if publishedAt != "" {
		published = publishedAt
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := f.db.Exec(
		`INSERT INTO posts(slug, type, status, title, body_markdown, body_html, excerpt, published_at, created_at, updated_at)
		 VALUES(?,?,?,?,?,'',?,?,?,?)`,
		slug, kind, status, title, body, excerpt, published, stamp, stamp)
	if err != nil {
		t.Fatalf("insert post %s: %v", slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("post id: %v", err)
	}
	return id
}

func (f *authFixture) softDeletePost(t *testing.T, slug string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE posts SET deleted_at = ? WHERE slug = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), slug); err != nil {
		t.Fatalf("soft delete %s: %v", slug, err)
	}
}

// loadMoreLink returns the href of the "加载更多" link, or "" when the page has
// none. The attribute is HTML-unescaped, so the result is the real URL.
func loadMoreLink(t *testing.T, body string) string {
	t.Helper()
	const marker = `class="load-more" href="`
	start := strings.Index(body, marker)
	if start < 0 {
		return ""
	}
	rest := body[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("load-more link is not terminated: %q", rest)
	}
	return html.UnescapeString(rest[:end])
}

// ---------- SSR page ----------

func TestSearchPageStatesAndActiveNav(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "写作", content.TypeArticle, content.StatusPublished, "关于 写作", "正文内容", "", "2026-01-02T00:00:00Z")

	t.Run("blank query invites a search", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/search", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "输入关键词") {
			t.Error("the blank search page does not invite a query")
		}
		if strings.Contains(body, "没有找到") || strings.Contains(body, "本页") {
			t.Error("the blank search page rendered a result state")
		}
	})

	t.Run("search nav item is current", func(t *testing.T) {
		body := f.do(t, http.MethodGet, "/search?q=写作", "", nil, nil).Body.String()
		if !strings.Contains(body, `class="nav-item on" href="/search" aria-current="page"`) {
			t.Error("the 搜索 nav item is not marked current on /search")
		}
		// Both navigation bars mark the current section, exactly as they do on
		// the feed: the desktop rail and the mobile bottom bar. While the bottom
		// bar had no search entry this counted one, which is what left a phone
		// with nothing highlighted on this page.
		if got := strings.Count(body, `aria-current="page"`); got != 2 {
			t.Errorf("active navigation items = %d, want 2", got)
		}
	})

	t.Run("rss discovery link is in the head", func(t *testing.T) {
		body := f.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
		if !strings.Contains(body, `<link rel="alternate" type="application/rss+xml"`) ||
			!strings.Contains(body, `href="/feed.xml"`) {
			t.Error("pages do not advertise the RSS feed")
		}
	})

	t.Run("results render the highlighted snippet", func(t *testing.T) {
		f.insertPost(t, "escaped", content.TypeArticle, content.StatusPublished,
			"标题", `<script>alert(1)</script> 关键词 与正文`, "", "2026-01-03T00:00:00Z")

		rec := f.do(t, http.MethodGet, "/search?q=关键词", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<mark>关键词</mark>") {
			t.Error("the highlighted match is missing from the result row")
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Error("the stored text was not escaped in the snippet")
		}
		if strings.Contains(body, "<script>") {
			t.Error("the rendered page contains an unescaped tag from stored content")
		}
		if !strings.Contains(body, `href="/p/escaped"`) {
			t.Error("the result row does not link to the post")
		}
		if !strings.Contains(body, `本页 1 条`) {
			t.Error("the page does not report how many results this page shows")
		}
	})

	t.Run("a title or excerpt hit is visible in the fragment", func(t *testing.T) {
		// The snippet must come from the column that matched: a title-only hit
		// otherwise renders a body prefix with nothing highlighted.
		f.insertPost(t, "title-hit", content.TypeArticle, content.StatusPublished,
			"独特标题词 命中", "正文里没有这个词。", "", "2026-01-04T00:00:00Z")

		rec := f.do(t, http.MethodGet, "/search?q=独特标题词", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `<mark>独特标题词</mark>`) {
			t.Errorf("the title hit is not highlighted in the result row: %s", body)
		}
	})

	t.Run("raw highlight markers cannot inject or unbalance markup", func(t *testing.T) {
		// A stored body may contain the two control characters the search package
		// uses as markers. They may only open or close a highlight: no fragment is
		// ever trusted as HTML, so the worst case is a spurious <mark>, never
		// injected markup or a broken tag structure.
		f.insertPost(t, "markers", content.TypeArticle, content.StatusPublished,
			"原生控制符",
			"前段 \x02<em>注入</em>\x03 关键词 \x02尾部", "", "2026-01-05T00:00:00Z")

		rec := f.do(t, http.MethodGet, "/search?q=关键词", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "<em>") || strings.Contains(body, "<script>") {
			t.Error("stored markup was rendered as markup")
		}
		if !strings.Contains(body, "&lt;em&gt;") {
			t.Error("the stored text of the fragment is not escaped")
		}
		if open, closed := strings.Count(body, "<mark>"), strings.Count(body, "</mark>"); open != closed || open == 0 {
			t.Errorf("highlights are unbalanced or missing: %d <mark> vs %d </mark>", open, closed)
		}
	})

	t.Run("no matches is a quiet empty state", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/search?q=不存在的内容", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "没有找到“不存在的内容”") {
			t.Error("the empty state does not repeat the escaped query")
		}
		if !strings.Contains(body, "本页 0 条") {
			t.Error("the empty state does not report zero results on this page")
		}
	})

	t.Run("punctuation only never leaks a SQLite error", func(t *testing.T) {
		for _, query := range []string{`"`, "!!!???", "*()", "-"} {
			rec := f.do(t, http.MethodGet, "/search?q="+urlQueryEscape(query), "", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("query %q status = %d, want 200", query, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "本页 0 条") {
				t.Errorf("query %q did not render the zero-result state", query)
			}
			for _, leak := range []string{"SQL logic error", "syntax error", "sqlite"} {
				if strings.Contains(body, leak) {
					t.Errorf("query %q leaked %q", query, leak)
				}
			}
		}
	})

	t.Run("escaped query is echoed, never injected", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/search?q="+urlQueryEscape(`<script>alert(1)</script>`), "", nil, nil)
		body := rec.Body.String()
		if strings.Contains(body, "<script>") {
			t.Fatal("the query was echoed unescaped")
		}
		if !strings.Contains(body, "&lt;script&gt;") {
			t.Error("the escaped query is missing from the page")
		}
	})

	t.Run("right rail repeats the query", func(t *testing.T) {
		body := f.do(t, http.MethodGet, "/search?q=写作", "", nil, nil).Body.String()
		if !strings.Contains(body, `id="rail-search-input"`) || !strings.Contains(body, `value="写作"`) {
			t.Error("the right-rail search form does not reflect the current query")
		}
		// Every other page keeps an empty rail input.
		home := f.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
		if !strings.Contains(home, `id="rail-search-input" name="q" type="search" value=""`) {
			t.Error("the rail search form of the home page is not empty")
		}
	})
}

func TestSearchPagePaginatesWithTheQueryPreserved(t *testing.T) {
	f := newAuthFixture(t)
	if err := settings.Set(context.Background(), f.db, settings.KeyContentPageSize, 2); err != nil {
		t.Fatalf("settings.Set: %v", err)
	}
	f.insertPost(t, "pager-3", content.TypeArticle, content.StatusPublished, "分页 三", "分页 内容", "", "2026-01-03T00:00:00Z")
	f.insertPost(t, "pager-2", content.TypeArticle, content.StatusPublished, "分页 二", "分页 内容", "", "2026-01-02T00:00:00Z")
	f.insertPost(t, "pager-1", content.TypeArticle, content.StatusPublished, "分页 一", "分页 内容", "", "2026-01-01T00:00:00Z")

	first := f.do(t, http.MethodGet, "/search?q=分页", "", nil, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}
	body := first.Body.String()
	if !strings.Contains(body, "pager-3") || !strings.Contains(body, "pager-2") || strings.Contains(body, "pager-1") {
		t.Fatalf("first page rendered the wrong rows: %s", body)
	}

	link := loadMoreLink(t, body)
	if link == "" {
		t.Fatal("the first page has no 加载更多 link")
	}
	if !strings.HasPrefix(link, "/search?") || !strings.Contains(link, "q=%E5%88%86%E9%A1%B5") || !strings.Contains(link, "cursor=") {
		t.Fatalf("load-more link %q does not preserve the query and cursor", link)
	}

	second := f.do(t, http.MethodGet, link, "", nil, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second page status = %d, want 200", second.Code)
	}
	next := second.Body.String()
	if !strings.Contains(next, "pager-1") {
		t.Error("the second page does not contain the remaining result")
	}
	for _, slug := range []string{"pager-2", "pager-3"} {
		if strings.Contains(next, slug) {
			t.Errorf("the second page repeats %s", slug)
		}
	}
	if strings.Contains(next, "load-more") {
		t.Error("the last page still offers more results")
	}
}

// ---------- JSON API ----------

func TestSearchAPIEnvelopeAndFallbacks(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "api-one", content.TypeArticle, content.StatusPublished, "API 设计", "关于 api 的正文", "", "2026-01-03T00:00:00Z")
	f.insertPost(t, "api-two", content.TypeMoment, content.StatusPublished, "", "api 的一句话", "", "2026-01-02T00:00:00Z")
	f.insertPost(t, "api-draft", content.TypeMoment, content.StatusDraft, "", "api 草稿", "", "")
	f.insertPost(t, "api-gone", content.TypeMoment, content.StatusPublished, "", "api 已删除", "", "2026-01-04T00:00:00Z")
	f.softDeletePost(t, "api-gone")

	t.Run("results carry the minimum retrieval fields", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/search?q=api", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q, want JSON", got)
		}
		payload := decodeEnvelope(t, rec)
		items, ok := payload["data"].([]any)
		if !ok {
			t.Fatalf("data is %T, want an array: %s", payload["data"], rec.Body.String())
		}
		if len(items) != 2 {
			t.Fatalf("results = %d, want the two published posts: %s", len(items), rec.Body.String())
		}
		if cursor, ok := payload["next_cursor"].(string); !ok || cursor != "" {
			t.Errorf("next_cursor = %v, want an empty string", payload["next_cursor"])
		}

		first, ok := items[0].(map[string]any)
		if !ok {
			t.Fatalf("result is %T, want an object", items[0])
		}
		for _, key := range []string{"id", "slug", "type", "title", "excerpt", "snippet", "url", "published_at", "updated_at"} {
			if _, present := first[key]; !present {
				t.Errorf("result is missing %q: %v", key, first)
			}
		}
		if _, present := first["rank"]; present {
			t.Error("the internal bm25 rank leaked into the API")
		}
		if first["slug"] != "api-one" || first["url"] != "/p/api-one" {
			t.Errorf("first result = %v, want the newest published post", first)
		}
		snippet, _ := first["snippet"].(string)
		// The fragment comes from whichever column matched best, so the marked term
		// is compared case-insensitively and the markers themselves must survive.
		if !strings.Contains(strings.ToLower(snippet), "\x02api\x03") {
			t.Errorf("snippet %q does not carry the plain-text highlight markers", snippet)
		}
	})

	t.Run("blank query is an empty page, not an error", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/search", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"data":[]`) {
			t.Errorf("body = %s, want an empty data array", rec.Body.String())
		}
	})

	t.Run("validation errors are stable codes", func(t *testing.T) {
		cases := []struct {
			name   string
			target string
			code   string
		}{
			{"limit above the maximum", "/api/v1/search?q=api&limit=51", "invalid_limit"},
			{"limit is not a number", "/api/v1/search?q=api&limit=many", "invalid_limit"},
			{"broken cursor", "/api/v1/search?q=api&cursor=nope", "invalid_cursor"},
			{"overlong query", "/api/v1/search?q=" + urlQueryEscape(strings.Repeat("词", 101)), "invalid_query"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rec := f.do(t, http.MethodGet, tc.target, "", nil, nil)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
				}
				if code, _, _ := decodeAPIError(t, rec); code != tc.code {
					t.Errorf("code = %q, want %q", code, tc.code)
				}
			})
		}
	})

	t.Run("wrong method stays JSON with Allow", func(t *testing.T) {
		rec := f.do(t, http.MethodPost, "/api/v1/search?q=api", "", map[string]string{"Origin": testOrigin}, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("Allow = %q, want GET", allow)
		}
		if code, _, _ := decodeAPIError(t, rec); code != "method_not_allowed" {
			t.Errorf("code = %q, want method_not_allowed", code)
		}
	})

	t.Run("unknown sub-path stays JSON", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/search/nope", "", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if code, _, _ := decodeAPIError(t, rec); code != "not_found" {
			t.Errorf("code = %q, want not_found", code)
		}
	})
}

// The API reads the same cursor as the SSR page, so a client can keep paging.
func TestSearchAPIPaginatesWithoutDuplicates(t *testing.T) {
	f := newAuthFixture(t)
	for i := 1; i <= 3; i++ {
		f.insertPost(t, "page-"+string(rune('0'+i)), content.TypeMoment, content.StatusPublished,
			"", "翻页 内容", "", "2026-01-0"+string(rune('0'+i))+"T00:00:00Z")
	}

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 4; page++ {
		target := "/api/v1/search?q=翻页&limit=2"
		if cursor != "" {
			target += "&cursor=" + urlQueryEscape(cursor)
		}
		rec := f.do(t, http.MethodGet, target, "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		payload := decodeEnvelope(t, rec)
		items, _ := payload["data"].([]any)
		for _, item := range items {
			slug, _ := item.(map[string]any)["slug"].(string)
			if seen[slug] {
				t.Fatalf("slug %q was returned twice", slug)
			}
			seen[slug] = true
		}
		cursor, _ = payload["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Errorf("visited %d results, want 3", len(seen))
	}
}
