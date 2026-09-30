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

// markupBlock returns the markup between two markers. The shell is small and
// hand-written, so slicing between a start marker and the end of the region is
// enough to ask what one part of it contains - and unlike a whole-document
// search it cannot be satisfied by an entry that lives somewhere else.
func markupBlock(t *testing.T, body, from, to string) string {
	t.Helper()
	start := strings.Index(body, from)
	if start < 0 {
		t.Fatalf("the document has no %q", from)
	}
	rest := body[start:]
	end := strings.Index(rest, to)
	if end < 0 {
		t.Fatalf("%q is never closed by %q", from, to)
	}
	return rest[:end]
}

// TestBothBarsMarkTheCurrentDestination pins that each of the four front-end
// destinations is flagged as current in *both* navigations. The desktop rail and
// the mobile bar are the same four destinations in two shapes, so a page that
// highlights itself in one and not the other leaves half the visitors with no
// idea where they are. The count is asserted as well as the presence: exactly one
// marked entry per bar, so a stray second cannot pass unnoticed. The count is
// taken inside each navigation rather than over the whole document, because the
// feed's content-type row marks itself current too and that row is neither bar.
func TestBothBarsMarkTheCurrentDestination(t *testing.T) {
	c := newContentFixture(t)

	for _, page := range []struct {
		name   string
		target string
	}{
		{"feed", "/"},
		{"articles", "/?type=article"},
		{"photos", "/?type=photo"},
		{"bookmarks", "/bookmarks"},
	} {
		t.Run(page.name, func(t *testing.T) {
			body := c.do(t, http.MethodGet, page.target, "", nil, c.cookie).Body.String()
			if got := strings.Count(navBlock(t, body, "nav"), `aria-current="page"`); got != 1 {
				t.Errorf("GET %s marks %d rail items current, want 1", page.target, got)
			}
			if got := strings.Count(navBlock(t, body, "bottom-nav"), `aria-current="page"`); got != 1 {
				t.Errorf("GET %s marks %d bottom-bar items current, want 1", page.target, got)
			}
		})
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
		{"bookmarks", "/bookmarks", c.cookie},
		{"login", "/login", nil},
		{"register", "/register", nil},
		{"admin comments", "/admin/comments", c.cookie},
		{"admin settings", "/admin/settings/site", c.cookie},
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

// The front rail is a compact icon-only strip: one brand mark, four content
// destinations, then theme and account controls. Labels stay available to
// assistive technology and mouse users through hidden text and title attributes.
func TestTheFrontRailUsesOnlyIconsAndKeepsFourDestinations(t *testing.T) {
	f := newContentFixture(t)

	page := func(cookie *http.Cookie) string {
		t.Helper()
		return f.do(t, http.MethodGet, "/", "", nil, cookie).Body.String()
	}
	rail := func(cookie *http.Cookie) string {
		t.Helper()
		return markupBlock(t, page(cookie), `data-rail`, "</aside>")
	}

	guest := rail(nil)
	for _, want := range []string{
		`class="brandmark"`,
		`data-theme-toggle`,
	} {
		if !strings.Contains(guest, want) {
			t.Errorf("the rail is missing %s:\n%s", want, guest)
		}
	}
	for _, unwanted := range []string{`class="wordmark"`, `class="brand-name"`} {
		if strings.Contains(guest, unwanted) {
			t.Errorf("the icon-only rail still renders brand text %s:\n%s", unwanted, guest)
		}
	}
	nav := navBlock(t, guest, "nav")
	if got := strings.Count(nav, `class="nav-item`); got != 4 {
		t.Errorf("the rail navigation has %d entries, want 4:\n%s", got, nav)
	}
	// The switch and the account entries are controls, not destinations, so none
	// of them belongs inside the navigation element.
	for _, unwanted := range []string{"外观", "我的", "登录", "退出登录"} {
		if strings.Contains(nav, unwanted) {
			t.Errorf("the rail navigation offers %s, which is not one of the four", unwanted)
		}
	}
	if strings.Contains(nav, "data-theme-toggle") {
		t.Error("the theme switch is one of the navigation entries")
	}
	// A guest gets the way in; a session gets the way out. Never both.
	foot := markupBlock(t, guest, `class="rail-foot"`, "</div>")
	if !strings.Contains(foot, `href="/login"`) {
		t.Errorf("the rail offers a guest no way to sign in:\n%s", foot)
	}

	owner := rail(f.cookie)
	foot = markupBlock(t, owner, `class="rail-foot"`, "</div>")
	if !strings.Contains(foot, `href="/admin"`) || !strings.Contains(foot, "data-logout") {
		t.Errorf("the rail account area is missing an entry for the owner:\n%s", foot)
	}
	if strings.Contains(foot, `href="/login"`) {
		t.Error("a signed-in session is still offered the sign-in page")
	}

	// Navigation labels remain in the accessibility tree but are visually hidden.
	css := f.do(t, http.MethodGet, "/static/app.css", "", nil, nil).Body.String()
	start := strings.Index(css, ".sr-only")
	if start < 0 {
		t.Fatal("app.css has no .sr-only rule")
	}
	group := css[start:]
	if end := strings.Index(group, "{"); end >= 0 {
		group = group[:end]
	}
	if !strings.Contains(group, ".rail-left .nav-item .label") {
		t.Errorf("the rail's navigation labels are visible again, and the owner asked for icons only:\n%s", group)
	}
}

// TestSignedInPagesOfferSignOut pins the way out of a session. The endpoint
// (POST /api/v1/auth/logout) and the session store were always complete: the
// shell simply rendered no control on any page that called them, so a visitor
// who signed in had no way to sign out from the interface.
//
// Three identities are asserted because the two entries in the left rail's
// footer do not share a condition: Owner adds the moderation link, SignedIn
// adds the sign-out control, and a guest gets neither. Two entries are the
// target, not one: the left rail is display:none below 899px and the top bar is
// display:none above it, so a control that exists in only one of them is
// unreachable at some width.
func TestSignedInPagesOfferSignOut(t *testing.T) {
	c := newContentFixture(t)
	reader, _ := c.readerFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "moment", "status": "published", "body": "退出登录回归用例",
	})
	slug := created["slug"].(string)

	cases := []struct {
		name     string
		target   string
		cookie   *http.Cookie
		signedIn bool
		owner    bool
	}{
		{"guest on the feed", "/", nil, false, false},
		{"guest on a detail page", "/p/" + slug, nil, false, false},
		{"reader on the feed", "/", reader, true, false},
		{"reader on bookmarks", "/bookmarks", reader, true, false},
		{"owner on the feed", "/", c.cookie, true, true},
		{"owner on bookmarks", "/bookmarks", c.cookie, true, true},
		{"owner on admin comments", "/admin/comments", c.cookie, true, true},
		{"owner on admin settings", "/admin/settings/ai", c.cookie, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := c.do(t, http.MethodGet, tc.target, "", nil, tc.cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200", tc.target, rec.Code)
			}
			body := rec.Body.String()
			// The front end renders .rail-left and the back end .rail-admin; the two
			// share the data-rail hook so this test reads whichever one the page has.
			rail := markupBlock(t, body, `data-rail`, "</aside>")
			bar := markupBlock(t, body, `class="tb-actions"`, "</header>")

			wantEach := 0
			if tc.signedIn {
				wantEach = 1
			}
			if got := strings.Count(body, "data-logout"); got != 2*wantEach {
				t.Errorf("GET %s renders %d sign-out controls, want %d", tc.target, got, 2*wantEach)
			}
			if got := strings.Count(rail, "data-logout"); got != wantEach {
				t.Errorf("GET %s renders %d sign-out controls in the rail, want %d:\n%s",
					tc.target, got, wantEach, rail)
			}
			if got := strings.Count(bar, "data-logout"); got != wantEach {
				t.Errorf("GET %s renders %d sign-out controls in the mobile top bar, want %d:\n%s",
					tc.target, got, wantEach, bar)
			}
			if got := strings.Contains(rail, `href="/admin"`); got != tc.owner {
				t.Errorf("GET %s offers the admin entry = %v, want %v", tc.target, got, tc.owner)
			}
			// Signing out is a write, so the control must be a button that the
			// script turns into a POST. A link would end the session on a GET,
			// which any third-party page can trigger with an image tag.
			if strings.Contains(body, `href="/api/v1/auth/logout"`) {
				t.Error("the sign-out control is a plain link, so a GET would end the session")
			}
		})
	}
}

// TestTheTwoSurfacesKeepTheirOwnNavigation pins the boundary between the front
// end and the back end. The back end used to render the front end's shell: the
// same icon rail (首页/文章/摄影/收藏) and the same mobile bar, which made a
// management page look like the feed and offered entries that mean nothing
// there. The shell swaps one navigation for the other, and this is what keeps
// the two from trading parts again.
func TestTheTwoSurfacesKeepTheirOwnNavigation(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "moment", "status": "published", "body": "两套界面回归用例",
	})
	slug := created["slug"].(string)

	// Entries that only mean something on the front end. The bottom bar's label
	// is how the two mobile bars are told apart; the rail is identified by its
	// class because both rails are asides carrying the same data-rail hook.
	frontOnly := []string{
		`class="rail-left"`,
		`class="nav-item`,
		`class="bottom-nav" aria-label="移动端导航"`,
		`href="/bookmarks"`,
		`href="/?type=article"`,
		`href="/?type=photo"`,
	}
	// Entries that only mean something in the back end.
	backOnly := []string{
		`class="rail-admin"`,
		`class="app is-admin"`,
		`class="bottom-nav" aria-label="后台导航"`,
		`class="admin-nav-item`,
	}

	back := []string{"/admin/comments", "/admin/settings/site", "/admin/settings/ai"}
	for _, target := range back {
		t.Run("back end"+target, func(t *testing.T) {
			body := c.do(t, http.MethodGet, target, "", nil, c.cookie).Body.String()
			for _, marker := range backOnly {
				if !strings.Contains(body, marker) {
					t.Errorf("GET %s is missing %s", target, marker)
				}
			}
			for _, marker := range frontOnly {
				if strings.Contains(body, marker) {
					t.Errorf("GET %s renders %s, which belongs to the front end", target, marker)
				}
			}
			// The right rail is the author status card's column, and that card
			// exists on the feed only.
			if strings.Contains(body, `class="rail-right"`) {
				t.Errorf("GET %s renders the feed's right rail", target)
			}
		})
	}

	front := []string{"/", "/p/" + slug}
	for _, target := range front {
		t.Run("front end"+target, func(t *testing.T) {
			body := c.do(t, http.MethodGet, target, "", nil, nil).Body.String()
			for _, marker := range []string{
				`class="rail-left"`,
				`class="bottom-nav" aria-label="移动端导航"`,
			} {
				if !strings.Contains(body, marker) {
					t.Errorf("GET %s is missing %s", target, marker)
				}
			}
			for _, marker := range backOnly {
				if strings.Contains(body, marker) {
					t.Errorf("GET %s renders %s, which belongs to the back end", target, marker)
				}
			}
		})
	}
}
