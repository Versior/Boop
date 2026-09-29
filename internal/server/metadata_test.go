package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"boop/internal/content"
)

// structuredData extracts the JSON-LD document of a page and decodes it, so a
// test asserts on the document rather than on a substring of its encoding.
func structuredData(t *testing.T, body string) map[string]any {
	t.Helper()
	const open = `<script type="application/ld+json">`
	start := strings.Index(body, open)
	if start < 0 {
		t.Fatal("page carries no structured data")
	}
	rest := body[start+len(open):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatal("structured data block is not terminated")
	}
	block := rest[:end]
	var document map[string]any
	if err := json.Unmarshal([]byte(block), &document); err != nil {
		t.Fatalf("structured data is not valid JSON: %v\n%s", err, block)
	}
	return document
}

// head returns the <head> of a rendered page.
func head(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<head>")
	end := strings.Index(body, "</head>")
	if start < 0 || end < start {
		t.Fatalf("page has no head element:\n%s", body)
	}
	return body[start:end]
}

// setSEO pins the author's SEO override, which the create API does not accept
// yet, so the renderer is exercised the way a stored row would exercise it.
func (f *authFixture) setSEO(t *testing.T, slug, title, description string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE posts SET seo_title = ?, seo_description = ? WHERE slug = ?`,
		title, description, slug); err != nil {
		t.Fatalf("set seo of %s: %v", slug, err)
	}
}

func TestPostHeadCarriesCanonicalAndPreview(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "article", "status": "published", "title": "海边的下午",
		"body": "正文第一段", "excerpt": "作者写的摘要",
	})
	slug := created["slug"].(string)

	body := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
	page := head(t, body)
	// The slug keeps its CJK characters, so the address is the percent-encoded
	// form: a raw one would be emitted into the attribute unencoded.
	canonical := "http://localhost:8080/p/" + url.PathEscape(slug)
	if !strings.Contains(canonical, "%") {
		t.Fatal("the fixture no longer exercises a non-ASCII slug")
	}

	for _, want := range []string{
		`<link rel="canonical" href="` + canonical + `">`,
		`<meta property="og:type" content="article">`,
		`<meta property="og:site_name" content="Boop">`,
		`<meta property="og:url" content="` + canonical + `">`,
		`<meta property="og:title" content="海边的下午 · Boop">`,
		`<meta property="og:description" content="作者写的摘要">`,
		`<meta name="description" content="作者写的摘要">`,
		`<meta name="twitter:card" content="summary">`,
		`<meta property="article:published_time"`,
		`<meta property="article:modified_time"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("post head is missing %s", want)
		}
	}
	// A post with no cover claims no preview image, rather than borrowing the
	// site brand and then asserting it in structured data.
	if strings.Contains(page, "og:image") {
		t.Errorf("a post without a cover claims a preview image:\n%s", page)
	}
	// The visible heading keeps the real title.
	if !strings.Contains(body, `<h1 class="detail-title">海边的下午</h1>`) {
		t.Error("the heading did not keep the author's title")
	}

	document := structuredData(t, body)
	if document["@type"] != "BlogPosting" {
		t.Errorf("@type = %v, want BlogPosting", document["@type"])
	}
	if document["headline"] != "海边的下午" {
		t.Errorf("headline = %v", document["headline"])
	}
	if document["inLanguage"] != "zh-CN" {
		t.Errorf("inLanguage = %v", document["inLanguage"])
	}
	main, ok := document["mainEntityOfPage"].(map[string]any)
	if !ok || main["@id"] != canonical {
		t.Errorf("mainEntityOfPage = %v, want the canonical address %q", document["mainEntityOfPage"], canonical)
	}
}

func TestIndexHeadCarriesCanonicalAndSiteDocument(t *testing.T) {
	f := newAuthFixture(t)

	page := head(t, f.do(t, http.MethodGet, "/", "", nil, nil).Body.String())
	for _, want := range []string{
		`<link rel="canonical" href="http://localhost:8080/">`,
		`<meta property="og:type" content="website">`,
		`<meta property="og:url" content="http://localhost:8080/">`,
		`<meta property="og:title" content="Boop">`,
		`<meta name="description" content="遇事开心的个人博客">`,
		`<meta name="twitter:card" content="summary">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index head is missing %s", want)
		}
	}

	document := structuredData(t, f.do(t, http.MethodGet, "/", "", nil, nil).Body.String())
	if document["@type"] != "WebSite" {
		t.Errorf("@type = %v, want WebSite", document["@type"])
	}
	if document["name"] != "Boop" {
		t.Errorf("name = %v, want the configured site name", document["name"])
	}
	if document["url"] != "http://localhost:8080/" {
		t.Errorf("url = %v", document["url"])
	}
}

func TestCanonicalDropsTheQueryString(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "hello", content.TypeArticle, content.StatusPublished, "你好", "正文", "", "2026-01-01T00:00:00Z")

	// Every view of the feed renders the same document title and the same
	// cards in a different order, so they all have to name one address. The
	// cursor is the one that matters most: it is the query string that can
	// generate a URL per page forever.
	for _, target := range []string{"/", "/?type=article", "/?type=photo", "/?cursor=2026-01-01T00:00:00Z,3", "/?type=article&cursor=2026-01-01T00:00:00Z,3"} {
		t.Run(target, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, target, "", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			page := head(t, rec.Body.String())
			if !strings.Contains(page, `<link rel="canonical" href="http://localhost:8080/">`) {
				t.Errorf("canonical of %s is not the feed address:\n%s", target, page)
			}
		})
	}
}

func TestHeadMetadataUsesTheConfiguredBaseURL(t *testing.T) {
	cfg := testConfig()
	cfg.BaseURL = "https://blog.example.com"
	f := newAuthFixtureWithConfig(t, cfg)
	f.insertPost(t, "hello", content.TypeArticle, content.StatusPublished, "你好", "正文", "", "2026-01-01T00:00:00Z")

	hostile := map[string]string{
		"Host":              "evil.example.net",
		"X-Forwarded-Host":  "evil.example.net",
		"X-Forwarded-Proto": "http",
	}

	for _, target := range []string{"/", "/p/hello"} {
		t.Run(target, func(t *testing.T) {
			page := head(t, f.do(t, http.MethodGet, target, "", hostile, nil).Body.String())
			if !strings.Contains(page, "https://blog.example.com") {
				t.Errorf("%s does not use BOOP_BASE_URL:\n%s", target, page)
			}
			if strings.Contains(page, "evil.example.net") {
				t.Errorf("%s trusted a forwarded header:\n%s", target, page)
			}
		})
	}
}

func TestSEOTitleOverrideWinsInTheHeadOnly(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "article", "status": "published", "title": "真正的标题",
		"body": "正文", "excerpt": "真正的摘要",
	})
	slug := created["slug"].(string)
	c.setSEO(t, slug, "搜索用标题", "搜索用摘要")

	body := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
	page := head(t, body)

	if !strings.Contains(page, `<title>搜索用标题 · Boop</title>`) {
		t.Errorf("the document title ignored the SEO override:\n%s", page)
	}
	if !strings.Contains(page, `<meta property="og:title" content="搜索用标题 · Boop">`) {
		t.Errorf("og:title ignored the SEO override:\n%s", page)
	}
	if !strings.Contains(page, `<meta name="description" content="搜索用摘要">`) {
		t.Errorf("the description ignored the SEO override:\n%s", page)
	}
	if document := structuredData(t, body); document["headline"] != "搜索用标题" {
		t.Errorf("structured data headline = %v", document["headline"])
	}
	// A wording chosen for a search result is not what the page shows.
	if !strings.Contains(body, `<h1 class="detail-title">真正的标题</h1>`) {
		t.Error("the SEO override leaked into the visible heading")
	}
}

func TestPostSummaryIsReaderTextNotSource(t *testing.T) {
	f := newAuthFixture(t)
	// No title and no excerpt, so both the heading and the description have to
	// fall back to the body. The body opens with a tag the sanitizer removes and
	// with a heading marker the renderer consumes: neither may reappear in the
	// summary, which is the machine-readable copy of the post. The row is
	// inserted directly because the create API requires a title on an article.
	f.insertPost(t, "raw-source", content.TypeArticle, content.StatusPublished, "",
		"<script>alert('xss')</script>\n\n## 小标题\n\n[链接](https://example.com) 正文", "", "2026-01-01T00:00:00Z")

	body := f.do(t, http.MethodGet, "/p/raw-source", "", nil, nil).Body.String()
	if strings.Contains(body, "alert('xss')") {
		t.Errorf("the sanitized payload reappeared in the page:\n%s", body)
	}
	page := head(t, body)
	if !strings.Contains(page, `<title>小标题 · Boop</title>`) {
		t.Errorf("the title fallback kept Markdown source:\n%s", page)
	}
	if !strings.Contains(page, `<meta name="description" content="小标题 链接 正文">`) {
		t.Errorf("the description is not the reader's text:\n%s", page)
	}
}

func TestHeadMetadataSurvivesAHostileTitle(t *testing.T) {
	c := newContentFixture(t)
	const payload = `</script><img src=x onerror=alert(1)> & "引号"`
	created := c.createOK(t, map[string]any{
		"type": "article", "status": "published", "title": payload,
		"body": "正文", "excerpt": payload,
	})
	slug := created["slug"].(string)

	body := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()

	// The attribute values are HTML-escaped, so the payload cannot open an
	// element.
	if strings.Contains(body, "<img src=x onerror=alert(1)>") {
		t.Errorf("the payload became markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Error("the payload is missing from the escaped attribute")
	}

	// The data block is the place where escaping is subtle: it must stay valid
	// JSON, it must not contain a raw "<" that could close the script element,
	// and it has to decode back to exactly what the author wrote.
	const open = `<script type="application/ld+json">`
	start := strings.Index(body, open)
	if start < 0 {
		t.Fatal("page carries no structured data")
	}
	rest := body[start+len(open):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatal("structured data block is not terminated")
	}
	block := rest[:end]
	if strings.ContainsAny(block, "<>") {
		t.Errorf("the data block contains a raw angle bracket: %s", block)
	}
	var document struct {
		Headline    string `json:"headline"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(block), &document); err != nil {
		t.Fatalf("the data block is not valid JSON: %v\n%s", err, block)
	}
	if document.Headline != payload {
		t.Errorf("headline round-trip = %q, want %q", document.Headline, payload)
	}
	if document.Description != payload {
		t.Errorf("description round-trip = %q, want %q", document.Description, payload)
	}
}

func TestPrivatePagesAreNoIndex(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "article", "status": "published", "title": "公开内容", "body": "正文",
	})
	slug := created["slug"].(string)

	const marker = `<meta name="robots" content="noindex, nofollow">`
	for _, tt := range []struct {
		target string
		cookie bool
		want   bool
	}{
		{"/search?q=hello", false, true},
		{"/bookmarks", true, true},
		{"/login", false, true},
		{"/register", false, true},
		{"/admin/settings/site", true, true},
		{"/admin/comments", true, true},
		{"/", false, false},
		{"/p/" + slug, false, false},
	} {
		t.Run(tt.target, func(t *testing.T) {
			cookie := c.cookie
			if !tt.cookie {
				cookie = nil
			}
			rec := c.do(t, http.MethodGet, tt.target, "", nil, cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			has := strings.Contains(rec.Body.String(), marker)
			if has != tt.want {
				t.Errorf("noindex present = %v, want %v", has, tt.want)
			}
		})
	}
}

func TestNonArticlePostsAreNotArticles(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "moment", content.TypeMoment, content.StatusPublished, "", "随手一记", "", "2026-01-01T00:00:00Z")
	f.insertPost(t, "photo", content.TypePhoto, content.StatusPublished, "", "海边的下午", "", "2026-01-02T00:00:00Z")

	// Google's Article guidance is explicit that a page which is not an article
	// should not be marked up as one, and schema.org has the subtype for a post
	// on a feed.
	for _, slug := range []string{"moment", "photo"} {
		t.Run(slug, func(t *testing.T) {
			body := f.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
			document := structuredData(t, body)
			if document["@type"] != "SocialMediaPosting" {
				t.Errorf("@type = %v, want SocialMediaPosting", document["@type"])
			}
		})
	}
}
