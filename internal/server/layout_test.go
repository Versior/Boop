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

// TestEveryPageCarriesOneToastSlot pins the shared toast container. app.js
// drops a message on the floor when #toast is missing - showToast() returns
// early - so a page that calls it without rendering the slot loses its only
// feedback. The detail page did exactly that for comment submission and
// deletion, which is how this test came to exist.
//
// The count matters as much as the presence: the slot used to be declared by
// each page separately, so the fix moved it into the shell and every template
// that still declares its own copy would produce a second element with the
// same id.
func TestEveryPageCarriesOneToastSlot(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "moment", "status": "published", "body": "提示条回归用例",
	})
	slug := created["slug"].(string)

	pages := []struct {
		name   string
		target string
		cookie *http.Cookie
	}{
		{"feed", "/", nil},
		{"feed, signed in", "/", c.cookie},
		{"detail", "/p/" + slug, nil},
		{"search", "/search?q=提示", nil},
		{"bookmarks", "/bookmarks", c.cookie},
		{"login", "/login", nil},
		{"register", "/register", nil},
		{"admin comments", "/admin/comments", c.cookie},
		{"admin settings", "/admin/settings", c.cookie},
	}
	for _, page := range pages {
		t.Run(page.name, func(t *testing.T) {
			rec := c.do(t, http.MethodGet, page.target, "", nil, page.cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200", page.target, rec.Code)
			}
			body := rec.Body.String()
			if got := strings.Count(body, `id="toast"`); got != 1 {
				t.Errorf("GET %s renders %d toast containers, want 1", page.target, got)
			}
			if got := strings.Count(body, `data-toast-message`); got != 1 {
				t.Errorf("GET %s renders %d toast message slots, want 1", page.target, got)
			}
		})
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
