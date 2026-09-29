package server

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"boop/internal/content"
)

// parsedSitemap mirrors the sitemap protocol elements the handler must produce.
type parsedSitemap struct {
	XMLName xml.Name `xml:"urlset"`
	XMLNS   string   `xml:"xmlns,attr"`
	URLs    []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
}

func parseSitemap(t *testing.T, body string) parsedSitemap {
	t.Helper()
	var document parsedSitemap
	if err := xml.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("sitemap is not valid XML: %v\n%s", err, body)
	}
	return document
}

// setUpdatedAt pins the lastmod a post will report, because insertPost stamps
// updated_at with the wall clock and the assertion needs a known value.
func (f *authFixture) setUpdatedAt(t *testing.T, slug, stamp string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE posts SET updated_at = ? WHERE slug = ?`, stamp, slug); err != nil {
		t.Fatalf("set updated_at of %s: %v", slug, err)
	}
}

func TestRobotsAllowsCrawlingAndBlocksThePrivateSurface(t *testing.T) {
	f := newAuthFixture(t)

	rec := f.do(t, http.MethodGet, "/robots.txt", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	body := rec.Body.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if lines[0] != "User-agent: *" {
		t.Errorf("first line = %q, want the wildcard group", lines[0])
	}
	if !strings.Contains(body, "\nAllow: /\n") {
		t.Errorf("robots.txt does not allow the rest of the site:\n%s", body)
	}
	for _, path := range robotsDisallow {
		if !strings.Contains(body, "\nDisallow: "+path+"\n") {
			t.Errorf("robots.txt does not disallow %q:\n%s", path, body)
		}
	}
	// The sitemap line is what makes the posts discoverable, and it has to be an
	// absolute URL on the configured origin: httptest sets Host to example.com,
	// so a handler that read the request would show up here.
	if !strings.Contains(body, "\nSitemap: http://localhost:8080/sitemap.xml\n") {
		t.Errorf("robots.txt does not advertise the configured sitemap:\n%s", body)
	}
	if strings.Contains(body, "example.com") {
		t.Errorf("robots.txt leaked the request Host:\n%s", body)
	}
}

func TestRobotsAndSitemapUseTheConfiguredBaseURL(t *testing.T) {
	cfg := testConfig()
	cfg.BaseURL = "https://blog.example.com"
	f := newAuthFixtureWithConfig(t, cfg)
	f.insertPost(t, "hello", content.TypeArticle, content.StatusPublished, "你好", "正文", "", "2026-01-01T00:00:00Z")

	// Forwarded headers are a spoofable input, so neither document may read
	// them: a crawler asked to index somebody else's origin would be a
	// self-inflicted poisoning attack.
	hostile := map[string]string{
		"X-Forwarded-Host":  "evil.example.net",
		"X-Forwarded-Proto": "http",
		"Host":              "evil.example.net",
	}

	robots := f.do(t, http.MethodGet, "/robots.txt", "", hostile, nil).Body.String()
	if !strings.Contains(robots, "Sitemap: https://blog.example.com/sitemap.xml") {
		t.Errorf("robots.txt does not use BOOP_BASE_URL:\n%s", robots)
	}
	if strings.Contains(robots, "evil.example.net") {
		t.Errorf("robots.txt trusted a forwarded header:\n%s", robots)
	}

	sitemap := f.do(t, http.MethodGet, "/sitemap.xml", "", hostile, nil).Body.String()
	if !strings.Contains(sitemap, "<loc>https://blog.example.com/p/hello</loc>") {
		t.Errorf("sitemap does not use BOOP_BASE_URL:\n%s", sitemap)
	}
	if strings.Contains(sitemap, "evil.example.net") {
		t.Errorf("sitemap trusted a forwarded header:\n%s", sitemap)
	}
}

func TestSitemapListsEveryPublishedPost(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "newest", content.TypeArticle, content.StatusPublished, "最新", "正文", "", "2026-01-03T09:30:00Z")
	f.insertPost(t, "moment", content.TypeMoment, content.StatusPublished, "", "动态正文", "", "2026-01-02T00:00:00Z")
	f.insertPost(t, "oldest", content.TypePhoto, content.StatusPublished, "", "摄影说明", "", "2026-01-01T00:00:00Z")
	f.insertPost(t, "draft", content.TypeArticle, content.StatusDraft, "草稿", "草稿正文", "", "")
	f.insertPost(t, "archived", content.TypeArticle, content.StatusArchived, "归档", "归档正文", "", "2025-12-01T00:00:00Z")
	f.insertPost(t, "removed", content.TypeArticle, content.StatusPublished, "删除", "删除正文", "", "2025-11-01T00:00:00Z")
	f.softDeletePost(t, "removed")
	f.setUpdatedAt(t, "newest", "2026-02-04T05:06:07Z")
	f.setUpdatedAt(t, "moment", "2026-01-02T00:00:00Z")
	f.setUpdatedAt(t, "oldest", "2026-01-01T00:00:00Z")

	rec := f.do(t, http.MethodGet, "/sitemap.xml", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Error("sitemap does not start with the XML declaration")
	}

	document := parseSitemap(t, body)
	if document.XMLNS != "http://www.sitemaps.org/schemas/sitemap/0.9" {
		t.Errorf("xmlns = %q", document.XMLNS)
	}

	// A sitemap that lists the index and stops never tells a crawler the posts
	// exist, so every published post has to be here - and nothing else.
	locs := make([]string, 0, len(document.URLs))
	for _, entry := range document.URLs {
		locs = append(locs, entry.Loc)
	}
	want := []string{
		"http://localhost:8080/",
		"http://localhost:8080/p/newest",
		"http://localhost:8080/p/moment",
		"http://localhost:8080/p/oldest",
	}
	if len(locs) != len(want) {
		t.Fatalf("sitemap lists %d URLs (%v), want %d", len(locs), locs, len(want))
	}
	for i, loc := range want {
		if locs[i] != loc {
			t.Errorf("URL %d = %q, want %q", i, locs[i], loc)
		}
	}
	for _, hidden := range []string{"draft", "archived", "removed"} {
		if strings.Contains(body, hidden) {
			t.Errorf("sitemap exposes the %s post:\n%s", hidden, body)
		}
	}
}

func TestSitemapCarriesPerPostLastMod(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "edited", content.TypeArticle, content.StatusPublished, "编辑过", "正文", "", "2026-01-01T00:00:00Z")
	f.setUpdatedAt(t, "edited", "2026-02-04T05:06:07Z")

	document := parseSitemap(t, f.do(t, http.MethodGet, "/sitemap.xml", "", nil, nil).Body.String())
	if len(document.URLs) != 2 {
		t.Fatalf("sitemap lists %d URLs, want the home page and the post", len(document.URLs))
	}

	// The home page renders a bounded page of the newest posts, so no single
	// stored stamp describes when it changed; it claims nothing.
	if got := document.URLs[0].LastMod; got != "" {
		t.Errorf("home lastmod = %q, want it omitted", got)
	}
	if got := document.URLs[1].LastMod; got != "2026-02-04T05:06:07Z" {
		t.Errorf("post lastmod = %q, want the stored updated_at in UTC", got)
	}
}

func TestSitemapOmitsAnUnparsableLastMod(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "odd", content.TypeArticle, content.StatusPublished, "时间异常", "正文", "", "2026-01-01T00:00:00Z")
	f.setUpdatedAt(t, "odd", "not-a-timestamp")

	body := f.do(t, http.MethodGet, "/sitemap.xml", "", nil, nil).Body.String()
	if strings.Contains(body, "<lastmod>") {
		t.Errorf("sitemap emitted the stamp of a row it could not parse:\n%s", body)
	}
	// The document still has to be valid XML after the element is dropped.
	document := parseSitemap(t, body)
	if len(document.URLs) != 2 {
		t.Errorf("sitemap lists %d URLs, want the home page and the post", len(document.URLs))
	}
	if document.URLs[1].Loc != "http://localhost:8080/p/odd" {
		t.Errorf("post loc = %q", document.URLs[1].Loc)
	}
}

func TestSitemapOfAnEmptyBlogIsStillValid(t *testing.T) {
	f := newAuthFixture(t)

	body := f.do(t, http.MethodGet, "/sitemap.xml", "", nil, nil).Body.String()
	document := parseSitemap(t, body)
	if len(document.URLs) != 1 || document.URLs[0].Loc != "http://localhost:8080/" {
		t.Errorf("empty sitemap = %+v, want just the home page", document.URLs)
	}
}

func TestDiscoveryDocumentsRejectNonGet(t *testing.T) {
	f := newAuthFixture(t)

	for _, target := range []string{"/robots.txt", "/sitemap.xml"} {
		t.Run(target, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, target, "", map[string]string{"Origin": testOrigin}, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want GET", got)
			}
		})
	}
}

// TestDiscoveryDocumentsAnswerHead pins the Go 1.22 ServeMux behaviour the route
// table relies on: a "GET /x" pattern also matches HEAD, so a crawler that
// probes with HEAD gets the headers instead of the 405 fallback.
func TestDiscoveryDocumentsAnswerHead(t *testing.T) {
	f := newAuthFixture(t)

	for _, target := range []string{"/robots.txt", "/sitemap.xml"} {
		t.Run(target, func(t *testing.T) {
			rec := f.do(t, http.MethodHead, target, "", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("HEAD %s status = %d, want 200", target, rec.Code)
			}
		})
	}
}

func TestSEOMethodFallbackIsNotReachableWithoutOrigin(t *testing.T) {
	f := newAuthFixture(t)

	// GuardUnsafeMethods runs ahead of the route table, so an unlabelled write
	// is refused before the 405 is even considered. This pins that ordering.
	rec := f.do(t, http.MethodPost, "/robots.txt", "", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 from the origin guard", rec.Code)
	}
}

// TestSitemapEncodesAPostSlugAsOnePathSegment pins the addresses a crawler is
// handed. Slugs are stored as raw text and Slugify keeps CJK, so a Chinese
// headline becomes a Chinese path segment. encoding/xml escapes the five XML
// entities and nothing else, so the sitemap used to publish /p/中文标题
// verbatim: well-formed XML carrying an address the sitemap protocol does not
// permit, since <loc> has to be an escaped URI.
//
// The encoded form is asserted to equal the detail page's own canonical. A
// crawler that follows the loc then lands on a page declaring exactly that
// address, instead of two spellings of the same page.
//
// A plain ASCII slug is asserted to come through untouched. Encoding a slug
// that needs no encoding still yields a working document, but it would change
// every address a subscriber has already cached.
func TestSitemapEncodesAPostSlugAsOnePathSegment(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "中文标题", content.TypeArticle, content.StatusPublished, "中文标题", "正文", "", "2026-01-01T00:00:00Z")
	f.insertPost(t, "plain-slug", content.TypeArticle, content.StatusPublished, "Plain", "正文", "", "2026-01-02T00:00:00Z")

	body := f.do(t, http.MethodGet, "/sitemap.xml", "", nil, nil).Body.String()

	const encoded = "http://localhost:8080/p/%E4%B8%AD%E6%96%87%E6%A0%87%E9%A2%98"
	if !strings.Contains(body, "<loc>"+encoded+"</loc>") {
		t.Errorf("sitemap does not carry the encoded address %q:\n%s", encoded, body)
	}
	if strings.Contains(body, "中文标题") {
		t.Errorf("sitemap still carries a raw slug:\n%s", body)
	}
	if !strings.Contains(body, "<loc>http://localhost:8080/p/plain-slug</loc>") {
		t.Errorf("sitemap rewrote a slug that needed no encoding:\n%s", body)
	}

	// The document is still valid XML after the change: the ampersands of the
	// percent escapes are not XML entities.
	parseSitemap(t, body)

	detail := f.do(t, http.MethodGet, "/p/"+url.PathEscape("中文标题"), "", nil, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail of the encoded slug: status = %d", detail.Code)
	}
	if !strings.Contains(detail.Body.String(), `<link rel="canonical" href="`+encoded+`">`) {
		t.Errorf("the detail page's canonical is not the address the sitemap lists:\n%s", detail.Body.String())
	}
}
