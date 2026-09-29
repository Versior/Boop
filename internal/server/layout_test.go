package server

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"boop/web"
)

// navBlock returns the markup of the first navigation element carrying class.
// The shell is small and hand-written, so a substring slice is enough to ask
// what a navigation bar actually contains.
func navBlock(t *testing.T, body, class string) string {
	t.Helper()
	marker := `class="` + class + `"`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("no element carries class %q", class)
	}
	rest := body[start:]
	end := strings.Index(rest, "</nav>")
	if end < 0 {
		t.Fatalf("the element with class %q is not a closed nav element", class)
	}
	return rest[:end]
}

func TestBottomNavOffersSearch(t *testing.T) {
	f := newAuthFixture(t)

	// The search field lives in the right rail, which is hidden below 1240px, so
	// below that breakpoint the bottom bar is the only way to reach /search.
	block := navBlock(t, f.do(t, http.MethodGet, "/", "", nil, nil).Body.String(), "bottom-nav")
	if !strings.Contains(block, `href="/search"`) {
		t.Errorf("the bottom navigation has no search entry:\n%s", block)
	}

	// Both bars mark the current section, so /search must not leave the mobile
	// bar with nothing highlighted - which is what happened while the entry was
	// missing from this bar only.
	body := f.do(t, http.MethodGet, "/search?q=hello", "", nil, nil).Body.String()
	if got := strings.Count(body, `aria-current="page"`); got != 2 {
		t.Errorf("/search marks %d active navigation items, want 2", got)
	}
	if !strings.Contains(navBlock(t, body, "bottom-nav"), `aria-current="page"`) {
		t.Error("the bottom navigation does not mark /search as current")
	}
}

func TestDetailPhotoIsNotCropped(t *testing.T) {
	// The card is one row of a list of thumbnails and keeps one uniform ratio;
	// the detail page is where a photograph is the content, so the cropped part
	// is exactly what the reader clicked through to see. The rule is scoped to
	// the detail markup rather than to the element, because the element is
	// shared by both.
	css, err := fs.ReadFile(web.FS, "static/app.css")
	if err != nil {
		t.Fatalf("read the stylesheet: %v", err)
	}
	if !strings.Contains(string(css), ".post-detail .photo-art{aspect-ratio:auto") {
		t.Error("the stylesheet has no detail-page override that releases the crop")
	}

	c := newContentFixture(t)
	asset := c.uploadPNG(t, "雾海.png", pngFixture(t, 8, 5, 31))
	created := c.createOK(t, map[string]any{
		"type": "photo", "status": "published", "body": "雾里的海岸线",
		"asset_ids": []int64{int64(asset["id"].(float64))},
	})
	slug := created["slug"].(string)

	detail := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
	// The override only reaches the image if the detail markup carries the
	// ancestor it is scoped to, and the image renders inside it.
	if !strings.Contains(detail, `class="post post-detail"`) {
		t.Error("the detail page does not carry the post-detail scope")
	}
	if !strings.Contains(detail, `class="photo-art"`) {
		t.Error("the detail page does not render the cover as a photograph")
	}

	// And the feed must not carry that scope, or every card would lose the
	// uniform thumbnail height the list depends on.
	feed := c.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
	if !strings.Contains(feed, `class="photo-art"`) {
		t.Error("the feed card does not render the photograph")
	}
	if strings.Contains(feed, "post-detail") {
		t.Error("the feed carries the detail scope and would lose its thumbnail ratio")
	}
}
