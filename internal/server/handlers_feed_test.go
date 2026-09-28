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
