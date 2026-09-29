package server

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"

	"boop/internal/content"
)

// parsedFeed mirrors the RSS 2.0 elements the handler must produce.
type parsedFeed struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel struct {
		Title         string `xml:"title"`
		Link          string `xml:"link"`
		Description   string `xml:"description"`
		Language      string `xml:"language"`
		LastBuildDate string `xml:"lastBuildDate"`
		Items         []struct {
			Title string `xml:"title"`
			Link  string `xml:"link"`
			GUID  struct {
				Value       string `xml:",chardata"`
				IsPermaLink string `xml:"isPermaLink,attr"`
			} `xml:"guid"`
			PubDate     string `xml:"pubDate"`
			Description string `xml:"description"`
		} `xml:"item"`
	} `xml:"channel"`
}

func parseFeed(t *testing.T, body string) parsedFeed {
	t.Helper()
	var feed parsedFeed
	if err := xml.Unmarshal([]byte(body), &feed); err != nil {
		t.Fatalf("feed is not valid XML: %v\n%s", err, body)
	}
	return feed
}

func TestFeedServesValidRSS(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "newest", content.TypeArticle, content.StatusPublished,
		"标题 & <script>", "正文 `<html>` 内容 <b>加粗</b>", "摘要 & 说明", "2026-01-03T09:30:00Z")
	f.insertPost(t, "moment", content.TypeMoment, content.StatusPublished,
		"", "第一行 动态\n第二行 不应该出现", "", "2026-01-02T00:00:00Z")
	f.insertPost(t, "draft", content.TypeArticle, content.StatusDraft, "草稿", "草稿正文", "", "")
	f.insertPost(t, "archived", content.TypeArticle, content.StatusArchived, "归档", "归档正文", "", "2026-01-01T00:00:00Z")
	f.insertPost(t, "removed", content.TypeArticle, content.StatusPublished, "删除", "删除正文", "", "2025-12-31T00:00:00Z")
	f.softDeletePost(t, "removed")

	rec := f.do(t, http.MethodGet, "/feed.xml", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/rss+xml; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Error("feed does not start with the XML declaration")
	}
	// XML escaping must be automatic: no stored markup survives as markup.
	if strings.Contains(body, "<script>") || strings.Contains(body, "<b>") {
		t.Errorf("feed contains unescaped stored markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("feed does not escape the stored title")
	}

	feed := parseFeed(t, body)
	if feed.Version != "2.0" {
		t.Errorf("version = %q, want 2.0", feed.Version)
	}
	channel := feed.Channel
	if channel.Title != "Boop" {
		t.Errorf("channel title = %q, want the configured site name", channel.Title)
	}
	if channel.Description == "" {
		t.Error("channel description is empty")
	}
	if channel.Language != "zh-CN" {
		t.Errorf("language = %q, want zh-CN", channel.Language)
	}
	// BOOP_BASE_URL, never the request Host (which httptest sets to example.com).
	if channel.Link != "http://localhost:8080/" {
		t.Errorf("channel link = %q, want the configured base URL", channel.Link)
	}
	if _, err := time.Parse(time.RFC1123Z, channel.LastBuildDate); err != nil {
		t.Errorf("lastBuildDate = %q is not RFC1123Z: %v", channel.LastBuildDate, err)
	}

	if len(channel.Items) != 2 {
		t.Fatalf("items = %d, want the two published posts", len(channel.Items))
	}
	newest := channel.Items[0]
	if newest.Title != "标题 & <script>" {
		t.Errorf("item title = %q, want the stored article title", newest.Title)
	}
	if newest.Link != "http://localhost:8080/p/newest" {
		t.Errorf("item link = %q, want an absolute BOOP_BASE_URL permalink", newest.Link)
	}
	if newest.GUID.Value != newest.Link || newest.GUID.IsPermaLink != "true" {
		t.Errorf("guid = %+v, want the same permalink marked isPermaLink", newest.GUID)
	}
	if _, err := time.Parse(time.RFC1123Z, newest.PubDate); err != nil {
		t.Errorf("pubDate = %q is not RFC1123Z: %v", newest.PubDate, err)
	}
	if newest.Description != "摘要 & 说明" {
		t.Errorf("description = %q, want the stored excerpt", newest.Description)
	}

	// The moment has no headline, so its first non-empty line becomes the title
	// and the rest of the body stays out of the single-line description.
	moment := channel.Items[1]
	if moment.Title != "第一行 动态" {
		t.Errorf("moment title = %q, want its first line", moment.Title)
	}
	if strings.Contains(moment.Description, "\n") || moment.Description != "第一行 动态 第二行 不应该出现" {
		t.Errorf("moment description = %q, want one collapsed line", moment.Description)
	}

	for _, slug := range []string{"draft", "archived", "removed"} {
		if strings.Contains(body, slug) {
			t.Errorf("feed leaked the %s post", slug)
		}
	}
}

// Markdown must reach a reader as text: the description carries the visible
// words only, and the XML encoder escapes whatever is left. Excerpts are authored
// as plain text and keep their own path.
func TestFeedDescriptionRendersMarkdownAsText(t *testing.T) {
	f := newAuthFixture(t)
	body := "# 文章小标题\n\n一段 **粗体** 文字与 [站内链接](/p/other)，还有 `代码片段`。\n\n" +
		"<script>alert(1)</script>\n\n<img src=x onerror=alert(2)>\n\n引用 &amp; 实体 < 符号"
	id := f.insertPost(t, "markdown", content.TypeArticle, content.StatusPublished,
		"Markdown 描述", body, "", "2026-01-03T00:00:00Z")
	// The stored body_html, the assets and the tags are not part of the feed's
	// projection, so markers in them must never appear in the document.
	if _, err := f.db.Exec(`UPDATE posts SET body_html = '<p>BODY_HTML_ONLY_MARKER</p>' WHERE id = ?`, id); err != nil {
		t.Fatalf("stamp body_html: %v", err)
	}
	if _, err := f.db.Exec(`INSERT INTO tags(name, slug) VALUES('TAG_ONLY_MARKER', 'tag-only-marker')`); err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	if _, err := f.db.Exec(`INSERT INTO post_tags(post_id, tag_id)
		SELECT ?, id FROM tags WHERE name = 'TAG_ONLY_MARKER'`, id); err != nil {
		t.Fatalf("link tag: %v", err)
	}

	raw := f.do(t, http.MethodGet, "/feed.xml", "", nil, nil).Body.String()
	feed := parseFeed(t, raw)
	if len(feed.Channel.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(feed.Channel.Items))
	}
	description := feed.Channel.Items[0].Description
	for _, want := range []string{"文章小标题", "粗体", "站内链接", "代码片段", "引用 & 实体 < 符号"} {
		if !strings.Contains(description, want) {
			t.Errorf("description %q is missing %q", description, want)
		}
	}
	for _, unwanted := range []string{
		"#", "**", "/p/other", "`", "<h1", "<strong", "<a ", "<code", "alert(", "onerror",
		"BODY_HTML_ONLY_MARKER", "TAG_ONLY_MARKER",
	} {
		if strings.Contains(description, unwanted) {
			t.Errorf("description %q still carries %q", description, unwanted)
		}
	}
	for _, unwanted := range []string{"BODY_HTML_ONLY_MARKER", "TAG_ONLY_MARKER"} {
		if strings.Contains(raw, unwanted) {
			t.Errorf("the feed read a column it must not project: %q", unwanted)
		}
	}
	// The markup itself never survives: only the text of the entity does.
	if !strings.Contains(raw, "引用 &amp; 实体 &lt; 符号") {
		t.Errorf("the description is not XML-escaped text: %s", raw)
	}
}

// A moment body is authored as plain text, so its description is folded to one
// line without going through the Markdown renderer.
func TestFeedDescriptionKeepsPlainTextBodiesIntact(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "moment-asterisk", content.TypeMoment, content.StatusPublished, "",
		"1*2*3 与 # 井号开头的行", "", "2026-01-02T00:00:00Z")

	feed := parseFeed(t, f.do(t, http.MethodGet, "/feed.xml", "", nil, nil).Body.String())
	if len(feed.Channel.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(feed.Channel.Items))
	}
	if got := feed.Channel.Items[0].Description; got != "1*2*3 与 # 井号开头的行" {
		t.Errorf("description = %q, want the plain text body unchanged", got)
	}
}

func TestFeedUsesTheConfiguredBaseURL(t *testing.T) {
	cfg := testConfig()
	cfg.BaseURL = "https://blog.example.com"
	f := newAuthFixtureWithConfig(t, cfg)
	f.insertPost(t, "hello", content.TypeArticle, content.StatusPublished, "你好", "正文", "", "2026-01-01T00:00:00Z")

	feed := parseFeed(t, f.do(t, http.MethodGet, "/feed.xml", "", nil, nil).Body.String())
	if feed.Channel.Link != "https://blog.example.com/" {
		t.Errorf("channel link = %q", feed.Channel.Link)
	}
	if len(feed.Channel.Items) != 1 || feed.Channel.Items[0].Link != "https://blog.example.com/p/hello" {
		t.Errorf("items = %+v", feed.Channel.Items)
	}
}

func TestFeedIsBoundedToTheNewestFiftyPosts(t *testing.T) {
	f := newAuthFixture(t)
	oldest := ""
	for i := 0; i < 51; i++ {
		slug := "post-" + twoDigits(i)
		if i == 0 {
			oldest = slug
		}
		f.insertPost(t, slug, content.TypeMoment, content.StatusPublished, "",
			"第 "+twoDigits(i)+" 条", "", time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339))
	}

	body := f.do(t, http.MethodGet, "/feed.xml", "", nil, nil).Body.String()
	feed := parseFeed(t, body)
	if len(feed.Channel.Items) != 50 {
		t.Fatalf("items = %d, want 50", len(feed.Channel.Items))
	}
	if len(feed.Channel.Items) > 0 && feed.Channel.Items[0].Link != "http://localhost:8080/p/post-50" {
		t.Errorf("first item = %q, want the newest post", feed.Channel.Items[0].Link)
	}
	if strings.Contains(body, "post-00") {
		t.Error("the feed is not bounded to the newest 50 posts")
	}
	if strings.Contains(body, oldest) {
		t.Errorf("the oldest post %q leaked into the feed", oldest)
	}
}

func TestFeedEmptySiteIsStillValid(t *testing.T) {
	f := newAuthFixture(t)

	rec := f.do(t, http.MethodGet, "/feed.xml", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	feed := parseFeed(t, rec.Body.String())
	if len(feed.Channel.Items) != 0 {
		t.Errorf("items = %d, want none", len(feed.Channel.Items))
	}
	if feed.Channel.LastBuildDate != "" {
		t.Errorf("lastBuildDate = %q, want it omitted without items", feed.Channel.LastBuildDate)
	}
}

// TestFeedQueryUsesTheFeedIndex pins the plan of the feed read: the statement must
// be served by idx_posts_feed in index order. A COALESCE over published_at in the
// projection or in the ORDER BY hides the indexed column and makes SQLite sort
// the published rows into a temporary B-tree, so this test fails on exactly that
// regression.
func TestFeedQueryUsesTheFeedIndex(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "plan-newer", content.TypeMoment, content.StatusPublished, "",
		"计划", "", "2026-01-03T00:00:00Z")
	f.insertPost(t, "plan-older", content.TypeMoment, content.StatusPublished, "",
		"计划", "", "2026-01-02T00:00:00Z")

	rows, err := f.db.Query(`EXPLAIN QUERY PLAN `+rssQuery, rssItemLimit)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()

	var steps []string
	for rows.Next() {
		// Since SQLite 3.24 the plan rows are (id, parent, notused, detail).
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("plan row: %v", err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("the query plan is empty")
	}
	plan := strings.Join(steps, " | ")
	if !strings.Contains(plan, "idx_posts_feed") {
		t.Errorf("plan does not read the feed index: %s", plan)
	}
	// The plan text is version dependent, and older SQLite versions word the scan
	// differently, so only the stable half is asserted here: an ORDER BY satisfied
	// by the index needs no temporary sort at all.
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Errorf("plan sorts into a temporary B-tree: %s", plan)
	}
}

func TestFeedRejectsOtherMethods(t *testing.T) {
	f := newAuthFixture(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := f.do(t, method, "/feed.xml", "", map[string]string{"Origin": testOrigin}, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
			}
			if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
				t.Errorf("Allow = %q, want GET", allow)
			}
		})
	}
	if rec := f.do(t, http.MethodGet, "/feed.xml", "", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("GET status = %d, want 200", rec.Code)
	}
}

// twoDigits pads a small fixture index, keeping slugs and timestamps sortable.
func twoDigits(value int) string {
	digits := []byte{byte('0' + value/10%10), byte('0' + value%10)}
	return string(digits)
}

// TestFeedEncodesAPostSlugAsOnePathSegment pins the same rule as the sitemap
// test, on the element that makes it matter most: guid isPermaLink="true"
// asserts the value is a URL a reader can open, so a raw Chinese string there
// is a claim that is simply false.
//
// The item link and the GUID are asserted together because they are the same
// address by construction; a feed whose permalink and GUID disagree would make
// a reader treat one post as two.
func TestFeedEncodesAPostSlugAsOnePathSegment(t *testing.T) {
	f := newAuthFixture(t)
	f.insertPost(t, "中文标题", content.TypeArticle, content.StatusPublished, "中文标题", "正文", "", "2026-01-01T00:00:00Z")
	f.insertPost(t, "plain-slug", content.TypeArticle, content.StatusPublished, "Plain", "正文", "", "2026-01-02T00:00:00Z")

	body := f.do(t, http.MethodGet, "/feed.xml", "", nil, nil).Body.String()
	feed := parseFeed(t, body)
	if len(feed.Channel.Items) != 2 {
		t.Fatalf("feed has %d items, want 2:\n%s", len(feed.Channel.Items), body)
	}

	links := make(map[string]bool, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		links[item.Link] = true
		if item.GUID.Value != item.Link {
			t.Errorf("item %q: guid = %q but link = %q", item.Title, item.GUID.Value, item.Link)
		}
		if item.GUID.IsPermaLink != "true" {
			t.Errorf("item %q: isPermaLink = %q", item.Title, item.GUID.IsPermaLink)
		}
	}
	for _, want := range []string{
		"http://localhost:8080/p/%E4%B8%AD%E6%96%87%E6%A0%87%E9%A2%98",
		"http://localhost:8080/p/plain-slug",
	} {
		if !links[want] {
			t.Errorf("feed has no item linking to %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/p/中文标题") {
		t.Errorf("feed still carries a raw slug in an address:\n%s", body)
	}
}
