package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"boop/internal/auth"
	"boop/internal/config"
	"boop/internal/content"
	"boop/internal/settings"
)

// contentFixture is the owner's browser: a signed-in owner session plus the
// CSRF token every unsafe request must carry.
type contentFixture struct {
	*authFixture
	owner  auth.User
	cookie *http.Cookie
	csrf   string
}

func newContentFixture(t *testing.T) *contentFixture {
	t.Helper()
	return newContentFixtureWithConfig(t, testConfig())
}

// newContentFixtureWithConfig signs in the owner on a server built from a
// specific configuration, which the storage tests need: whether uploads land on
// disk or in a bucket is decided by the configuration and nothing else.
func newContentFixtureWithConfig(t *testing.T, cfg config.Config) *contentFixture {
	t.Helper()
	f := newAuthFixtureWithConfig(t, cfg)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	rec := f.login(t, "owner@example.com", authPassword, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner login: status = %d, body %s", rec.Code, rec.Body.String())
	}
	return &contentFixture{
		authFixture: f,
		owner:       owner,
		cookie:      sessionCookie(t, rec),
		csrf:        decodeData(t, rec)["csrf_token"].(string),
	}
}

// readerFixture signs in a reader account, which must never publish.
func (f *authFixture) readerFixture(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	rec := f.register(t, "reader@example.com", "读者甲")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register reader: status = %d, body %s", rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec), decodeData(t, rec)["csrf_token"].(string)
}

// insertPost writes one post row directly, so a test can control status and
// published_at exactly instead of going through an owner create request. Every
// reading surface filters on `status = 'published' AND deleted_at IS NULL`, so
// the draft, archived and soft-deleted rows this can write are exactly what
// proves that filter works.
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

// softDeletePost flips deleted_at and nothing else. A soft delete has to hide
// the row from every reading surface while leaving the row itself recoverable,
// so tests need both states of one row, not two rows.
func (f *authFixture) softDeletePost(t *testing.T, slug string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE posts SET deleted_at = ? WHERE slug = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), slug); err != nil {
		t.Fatalf("soft delete %s: %v", slug, err)
	}
}

func (c *contentFixture) post(t *testing.T, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return c.postSameOrigin(t, target, body, c.cookie, map[string]string{"X-CSRF-Token": c.csrf})
}

// createPost sends an owner create request built from a Go map so test payloads
// use real UTF-8 instead of escaped literals.
func (c *contentFixture) createPost(t *testing.T, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return c.post(t, "/api/v1/admin/posts", marshalJSON(t, payload))
}

func (c *contentFixture) createOK(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	rec := c.createPost(t, payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body %s", rec.Code, rec.Body.String())
	}
	return decodeData(t, rec)
}

func (c *contentFixture) patchPost(t *testing.T, id int64, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	merged := map[string]any{}
	for key, value := range payload {
		merged[key] = value
	}
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/admin/posts/%d", id), strings.NewReader(marshalJSON(t, merged)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.AddCookie(c.cookie)
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

func (c *contentFixture) deletePost(t *testing.T, id int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/admin/posts/%d", id), nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.AddCookie(c.cookie)
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

func marshalJSON(t *testing.T, payload any) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(body)
}

// insertAssetFixture stands in for Task 5 uploads: photo publishing needs an
// existing asset row.
func (c *contentFixture) insertAssetFixture(t *testing.T, key string) int64 {
	t.Helper()
	res, err := c.db.Exec(
		`INSERT INTO assets(owner_user_id, storage_key, original_name, mime_type, size_bytes, sha256, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		c.owner.ID, key, "海滩.jpg", "image/jpeg", 2048, []byte("sha"),
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("asset id: %v", err)
	}
	return id
}

// ---------- authorization ----------

func TestAdminPostsRequireOwner(t *testing.T) {
	f := newAuthFixture(t)
	readerCookie, readerCSRF := f.readerFixture(t)

	payload := marshalJSON(t, map[string]any{"type": "moment", "status": "published", "body": "读者不该发布"})

	t.Run("guest", func(t *testing.T) {
		rec := f.postSameOrigin(t, "/api/v1/admin/posts", payload, nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "unauthorized" {
			t.Errorf("code = %q, want unauthorized", code)
		}
	})

	t.Run("reader", func(t *testing.T) {
		rec := f.postSameOrigin(t, "/api/v1/admin/posts", payload, readerCookie, map[string]string{"X-CSRF-Token": readerCSRF})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "forbidden" {
			t.Errorf("code = %q, want forbidden", code)
		}
		if posts := f.countRows(t, "posts"); posts != 0 {
			t.Errorf("posts = %d, want the reader write rejected", posts)
		}
	})

	t.Run("reader patch and delete", func(t *testing.T) {
		owner := newContentFixture(t)
		created := owner.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "站长动态"})
		id := int64(created["id"].(float64))
		// The reader lives in the same database as the post it tries to change.
		readerCookie, readerCSRF := owner.readerFixture(t)

		patch := func(method string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, fmt.Sprintf("/api/v1/admin/posts/%d", id), strings.NewReader(`{"body":"读者改写"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", testOrigin)
			req.Header.Set("X-CSRF-Token", readerCSRF)
			req.AddCookie(readerCookie)
			rec := httptest.NewRecorder()
			owner.handler.ServeHTTP(rec, req)
			return rec
		}
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			rec := patch(method)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s status = %d, want 403: %s", method, rec.Code, rec.Body.String())
			}
		}
	})
}

func TestAdminPostsRequireCSRF(t *testing.T) {
	c := newContentFixture(t)
	body := marshalJSON(t, map[string]any{"type": "moment", "status": "published", "body": "缺少令牌"})

	rec := c.postSameOrigin(t, "/api/v1/admin/posts", body, c.cookie, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if code, _, _ := decodeAPIError(t, rec); code != "csrf_invalid" {
		t.Errorf("code = %q, want csrf_invalid", code)
	}

	foreign := c.postSameOrigin(t, "/api/v1/admin/posts", body, c.cookie,
		map[string]string{"X-CSRF-Token": c.csrf, "Origin": "https://evil.example"})
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", foreign.Code)
	}
	if code, _, _ := decodeAPIError(t, foreign); code != "origin_mismatch" {
		t.Errorf("code = %q, want origin_mismatch", code)
	}
}

// ---------- create ----------

func TestCreatePostAPI(t *testing.T) {
	c := newContentFixture(t)

	created := c.createOK(t, map[string]any{
		"type":    "article",
		"status":  "published",
		"title":   "第一篇文章",
		"body":    "# 标题\n\n正文 **加粗**",
		"excerpt": "摘要",
		"tags":    []string{"写作", "Go"},
	})

	if created["slug"] != "第一篇文章" {
		t.Errorf("slug = %v, want the title based slug", created["slug"])
	}
	if created["status"] != "published" {
		t.Errorf("status = %v", created["status"])
	}
	if created["published_at"] == "" || created["published_at"] == nil {
		t.Errorf("published_at = %v, want a timestamp", created["published_at"])
	}
	html, _ := created["body_html"].(string)
	if !strings.Contains(html, "<strong>加粗</strong>") {
		t.Errorf("body_html = %q, want rendered markdown", html)
	}
	if tags, ok := created["tags"].([]any); !ok || len(tags) != 2 {
		t.Errorf("tags = %v", created["tags"])
	}
	if url, _ := created["url"].(string); url != "/p/第一篇文章" {
		t.Errorf("url = %q, want the detail path", url)
	}
}

func TestCreatePostValidationErrorsAreDeterministicJSON(t *testing.T) {
	c := newContentFixture(t)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"unknown field", `{"type":"moment","status":"draft","body":"x","extra":1}`, http.StatusBadRequest, "invalid_body"},
		{"unknown type", `{"type":"video","status":"draft","body":"x"}`, http.StatusBadRequest, "invalid_type"},
		{"unknown status", `{"type":"moment","status":"scheduled","body":"x"}`, http.StatusBadRequest, "invalid_status"},
		{"moment title", `{"type":"moment","status":"draft","title":"标题","body":"x"}`, http.StatusBadRequest, "invalid_title"},
		{"article without title", `{"type":"article","status":"published","body":"正文"}`, http.StatusBadRequest, "invalid_title"},
		{"photo without assets", `{"type":"photo","status":"published"}`, http.StatusBadRequest, "invalid_assets"},
		{"unknown asset", `{"type":"photo","status":"published","asset_ids":[4242]}`, http.StatusBadRequest, "invalid_asset"},
		{"trailing content", `{"type":"moment","status":"draft","body":"x"} {}`, http.StatusBadRequest, "invalid_body"},
		{"not an object", `[]`, http.StatusBadRequest, "invalid_body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := c.post(t, "/api/v1/admin/posts", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			code, message, _ := decodeAPIError(t, rec)
			if code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
			if !strings.HasPrefix(message, "") || message == "" {
				t.Errorf("message is empty")
			}
			if posts := c.countRows(t, "posts"); posts != 0 {
				t.Errorf("posts = %d, want every rejected create to store nothing", posts)
			}
		})
	}
}

func TestCreatePostRejectsOversizedContentBody(t *testing.T) {
	cfg := testConfig()
	cfg.MaxUploadMB = 100
	c := &contentFixture{authFixture: newAuthFixtureWithConfig(t, cfg)}
	owner := c.bootstrapOwner(t, "owner@example.com", "遇事开心")
	c.owner = owner
	rec := c.login(t, "owner@example.com", authPassword, nil)
	c.cookie = sessionCookie(t, rec)
	c.csrf = decodeData(t, rec)["csrf_token"].(string)

	body := marshalJSON(t, map[string]any{
		"type": "article", "status": "draft", "title": "很大",
		"body": strings.Repeat("a", 600<<10),
	})
	response := c.post(t, "/api/v1/admin/posts", body)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", response.Code, response.Body.String())
	}
	if code, _, _ := decodeAPIError(t, response); code != "payload_too_large" {
		t.Errorf("code = %q, want payload_too_large", code)
	}
}

func TestCreatePhotoPostWithAsset(t *testing.T) {
	c := newContentFixture(t)
	assetID := c.insertAssetFixture(t, "2026/09/sea.jpg")

	created := c.createOK(t, map[string]any{
		"type": "photo", "status": "published", "body": "雾里的海岸线",
		"asset_ids": []int64{assetID}, "location": "青岛 · 石老人海边",
		"captured_at": "2026-09-26T08:04:00Z",
	})

	assets, ok := created["assets"].([]any)
	if !ok || len(assets) != 1 {
		t.Fatalf("assets = %v, want the linked image", created["assets"])
	}
	asset := assets[0].(map[string]any)
	if asset["url"] != "/uploads/2026/09/sea.jpg" {
		t.Errorf("asset url = %v", asset["url"])
	}
	if created["location"] != "青岛 · 石老人海边" {
		t.Errorf("location = %v", created["location"])
	}
	if created["captured_at"] != "2026-09-26T08:04:00Z" {
		t.Errorf("captured_at = %v", created["captured_at"])
	}
}

// ---------- update and delete ----------

func TestPatchPostOptimisticLock(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "第一版"})
	id := int64(created["id"].(float64))
	updatedAt := created["updated_at"].(string)

	stale := c.patchPost(t, id, map[string]any{"updated_at": "2020-01-01T00:00:00Z", "body": "过期写入"})
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale patch status = %d, want 409: %s", stale.Code, stale.Body.String())
	}
	if code, _, _ := decodeAPIError(t, stale); code != "conflict" {
		t.Errorf("code = %q, want conflict", code)
	}

	ok := c.patchPost(t, id, map[string]any{"updated_at": updatedAt, "body": "第二版"})
	if ok.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body %s", ok.Code, ok.Body.String())
	}
	data := decodeData(t, ok)
	if data["body"] != "第二版" {
		t.Errorf("body = %v", data["body"])
	}
	if data["updated_at"] == updatedAt {
		t.Errorf("updated_at did not advance")
	}
	if data["slug"] != created["slug"] {
		t.Errorf("slug changed to %v", data["slug"])
	}
}

func TestPatchPostRejectsUnknownFieldsAndType(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "正文"})
	id := int64(created["id"].(float64))

	rec := c.patchPost(t, id, map[string]any{"updated_at": created["updated_at"], "type": "article"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if code, _, _ := decodeAPIError(t, rec); code != "invalid_body" {
		t.Errorf("code = %q, want invalid_body (a post never changes type)", code)
	}
	if rec := c.patchPost(t, id, map[string]any{"updated_at": created["updated_at"], "expected_updated_at": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field status = %d, want 400", rec.Code)
	}
}

func TestPatchPostPublishesADraft(t *testing.T) {
	c := newContentFixture(t)
	draft := c.createOK(t, map[string]any{"type": "article", "status": "draft", "title": "草稿标题", "body": "草稿正文"})
	id := int64(draft["id"].(float64))

	rec := c.patchPost(t, id, map[string]any{"updated_at": draft["updated_at"], "status": "published"})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body %s", rec.Code, rec.Body.String())
	}
	if data := decodeData(t, rec); data["published_at"] == nil || data["published_at"] == "" {
		t.Errorf("published_at = %v, want the publish moment", data["published_at"])
	}
}

func TestPatchPostUnknownIdAndBadId(t *testing.T) {
	c := newContentFixture(t)

	rec := c.patchPost(t, 4242, map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339), "body": "正文"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/posts/abc", strings.NewReader(`{"updated_at":"2026-09-28T06:02:03Z"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.AddCookie(c.cookie)
	bad := httptest.NewRecorder()
	c.handler.ServeHTTP(bad, req)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric id status = %d, want 400: %s", bad.Code, bad.Body.String())
	}
	if code, _, _ := decodeAPIError(t, bad); code != "invalid_id" {
		t.Errorf("code = %q, want invalid_id", code)
	}
}

func TestDeletePostAPIIsSoftAndIdempotent(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "将被删除"})
	id := int64(created["id"].(float64))
	slug := created["slug"].(string)

	rec := c.deletePost(t, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}

	detail := c.do(t, http.MethodGet, "/api/v1/posts/"+slug, "", nil, nil)
	if detail.Code != http.StatusNotFound {
		t.Errorf("deleted detail status = %d, want 404", detail.Code)
	}
	page := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil)
	if page.Code != http.StatusNotFound {
		t.Errorf("deleted page status = %d, want 404", page.Code)
	}

	var deletedAt *string
	if err := c.db.QueryRow(`SELECT deleted_at FROM posts WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("row disappeared: %v", err)
	}
	if deletedAt == nil || *deletedAt == "" {
		t.Errorf("deleted_at = %v, want a soft delete timestamp", deletedAt)
	}

	if again := c.deletePost(t, id); again.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", again.Code)
	}
}

// ---------- public read API ----------

func TestFeedAPIReturnsPublishedOnly(t *testing.T) {
	c := newContentFixture(t)
	assetID := c.insertAssetFixture(t, "2026/09/feed.jpg")

	c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "公开动态"})
	c.createOK(t, map[string]any{"type": "article", "status": "published", "title": "公开文章", "body": "正文"})
	c.createOK(t, map[string]any{"type": "photo", "status": "published", "asset_ids": []int64{assetID}})
	c.createOK(t, map[string]any{"type": "moment", "status": "draft", "body": "草稿动态"})

	rec := c.do(t, http.MethodGet, "/api/v1/posts", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	payload := decodeEnvelope(t, rec)
	list, ok := payload["data"].([]any)
	if !ok {
		t.Fatalf("data is not a list: %s", rec.Body.String())
	}
	if len(list) != 3 {
		t.Fatalf("feed returned %d posts, want the 3 published ones", len(list))
	}

	first := list[0].(map[string]any)
	if first["type"] != "photo" {
		t.Errorf("newest type = %v, want photo", first["type"])
	}
	if _, ok := first["like_count"]; !ok {
		t.Errorf("payload has no like_count: %v", first)
	}
	if _, ok := first["comment_count"]; !ok {
		t.Errorf("payload has no comment_count: %v", first)
	}
	if _, ok := first["body_html"]; !ok {
		t.Errorf("payload has no body_html: %v", first)
	}
	if next, ok := payload["next_cursor"].(string); !ok || next != "" {
		t.Errorf("next_cursor = %v, want an empty string on a short page", payload["next_cursor"])
	}

	articles := decodeEnvelope(t, c.do(t, http.MethodGet, "/api/v1/posts?type=article", "", nil, nil))["data"].([]any)
	if len(articles) != 1 || articles[0].(map[string]any)["title"] != "公开文章" {
		t.Errorf("article filter returned %v", articles)
	}
	photos := decodeEnvelope(t, c.do(t, http.MethodGet, "/api/v1/posts?type=photo", "", nil, nil))["data"].([]any)
	if len(photos) != 1 {
		t.Errorf("photo filter returned %v", photos)
	}
}

func TestFeedAPIRejectsBadFiltersAndCursor(t *testing.T) {
	c := newContentFixture(t)

	cases := []struct {
		target string
		code   string
	}{
		{"/api/v1/posts?type=video", "invalid_type"},
		{"/api/v1/posts?type=moment", "invalid_type"},
		{"/api/v1/posts?limit=999", "invalid_limit"},
		{"/api/v1/posts?limit=0", ""},
		{"/api/v1/posts?cursor=nonsense", "invalid_cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			rec := c.do(t, http.MethodGet, tc.target, "", nil, nil)
			if tc.code == "" {
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want 200 (limit=0 means default): %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if code, _, _ := decodeAPIError(t, rec); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

func TestFeedAPIPaginatesWithCursor(t *testing.T) {
	c := newContentFixture(t)
	for i := 0; i < 5; i++ {
		c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": fmt.Sprintf("第 %d 条", i)})
	}

	seen := map[float64]bool{}
	cursor := ""
	for page := 0; page < 5; page++ {
		target := "/api/v1/posts?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		payload := decodeEnvelope(t, c.do(t, http.MethodGet, target, "", nil, nil))
		list := payload["data"].([]any)
		for _, item := range list {
			id := item.(map[string]any)["id"].(float64)
			if seen[id] {
				t.Fatalf("post %v returned twice", id)
			}
			seen[id] = true
		}
		next, _ := payload["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d posts, want 5", len(seen))
	}
}

func TestPostDetailAPI(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "article", "status": "published", "title": "详情", "body": "**正文**",
	})
	slug := created["slug"].(string)

	rec := c.do(t, http.MethodGet, "/api/v1/posts/"+slug, "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	data := decodeData(t, rec)
	if data["title"] != "详情" {
		t.Errorf("title = %v", data["title"])
	}
	if html, _ := data["body_html"].(string); !strings.Contains(html, "<strong>正文</strong>") {
		t.Errorf("body_html = %v", data["body_html"])
	}

	draft := c.createOK(t, map[string]any{"type": "article", "status": "draft", "title": "草稿", "body": "正文"})
	if rec := c.do(t, http.MethodGet, "/api/v1/posts/"+draft["slug"].(string), "", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("draft detail status = %d, want 404", rec.Code)
	}
	if rec := c.do(t, http.MethodGet, "/api/v1/posts/nope", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown slug status = %d, want 404", rec.Code)
	}
}

func TestContentAPINeverReturnsHTML(t *testing.T) {
	c := newContentFixture(t)

	cases := []struct {
		name   string
		method string
		target string
		body   string
		origin bool
		status int
		code   string
	}{
		{"wrong method on the collection", http.MethodGet, "/api/v1/admin/posts", "", true, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"wrong method on an item", http.MethodPost, "/api/v1/admin/posts/1", `{}`, true, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"wrong method on the feed item", http.MethodPost, "/api/v1/posts/anything", `{}`, true, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"unknown admin path", http.MethodGet, "/api/v1/admin/other", "", true, http.StatusNotFound, "not_found"},
		{"unknown feed path", http.MethodGet, "/api/v1/nothing", "", false, http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.origin {
				headers["Origin"] = testOrigin
			}
			var cookie *http.Cookie
			if tc.body != "" {
				headers["X-CSRF-Token"] = c.csrf
				headers["Content-Type"] = "application/json"
				cookie = c.cookie
			}
			rec := c.do(t, tc.method, tc.target, tc.body, headers, cookie)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want JSON", ct)
			}
			if code, _, _ := decodeAPIError(t, rec); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

// ---------- server-rendered pages ----------

func TestHomePageRendersFeed(t *testing.T) {
	c := newContentFixture(t)
	assetID := c.insertAssetFixture(t, "2026/09/page.jpg")

	c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "动态正文甲"})
	c.createOK(t, map[string]any{"type": "article", "status": "published", "title": "文章标题乙", "body": "正文", "excerpt": "文章摘要乙"})
	c.createOK(t, map[string]any{"type": "photo", "status": "published", "asset_ids": []int64{assetID}, "body": "摄影说明丙"})

	rec := c.do(t, http.MethodGet, "/", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"动态正文甲", "文章标题乙", "摄影说明丙", "/p/", "/uploads/2026/09/page.jpg", "遇事开心"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page is missing %q", want)
		}
	}
	if strings.Contains(body, "这里还没有公开内容。") {
		t.Errorf("empty state shown while posts exist")
	}
}

func TestHomePageEmptyStateAndFilters(t *testing.T) {
	c := newContentFixture(t)

	body := c.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
	if !strings.Contains(body, "这里还没有公开内容。") {
		t.Errorf("missing empty state")
	}

	c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "只有动态"})
	articles := c.do(t, http.MethodGet, "/?type=article", "", nil, nil)
	if articles.Code != http.StatusOK {
		t.Fatalf("article filter status = %d", articles.Code)
	}
	if strings.Contains(articles.Body.String(), "只有动态") {
		t.Errorf("the article filter leaked a moment")
	}
	if rec := c.do(t, http.MethodGet, "/?type=video", "", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown type status = %d, want 400", rec.Code)
	}
	if rec := c.do(t, http.MethodGet, "/?type=moment", "", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("moment filter status = %d, want 400", rec.Code)
	}
	if rec := c.do(t, http.MethodGet, "/?cursor=broken", "", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("broken cursor status = %d, want 400", rec.Code)
	}
}

// The home page opens with the author's block and the content-type row. The
// block is the site's front door - cover image, big avatar, the bio - and the
// row carries a count per view, because a row of links that only repeated the
// navigation would be saying nothing the sidebar does not.
func TestHomePageAuthorHeaderAndTypeRow(t *testing.T) {
	c := newContentFixture(t)
	assetID := c.insertAssetFixture(t, "2026/09/header.jpg")
	// A draft first: it is the oldest row in the table, so a span query that
	// forgot the visibility rule would date the site from it (a draft has no
	// published_at, and the line would disappear) instead of from the feed.
	c.createOK(t, map[string]any{"type": "moment", "status": "draft", "body": "草稿不算"})
	oldest := c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "动态正文甲"})
	c.createOK(t, map[string]any{"type": "article", "status": "published", "title": "文章标题乙", "body": "正文"})
	c.createOK(t, map[string]any{"type": "article", "status": "published", "title": "文章标题丙", "body": "正文"})
	newest := c.createOK(t, map[string]any{"type": "photo", "status": "published", "asset_ids": []int64{assetID}})

	const (
		cover  = "https://cdn.example.com/cover.jpg"
		avatar = "https://cdn.example.com/site-avatar.png"
	)
	rec := c.patchSettings(t,
		`{"site_description":"海边的个人博客","site_avatar_url":"`+avatar+`","site_cover_url":"`+cover+`"}`,
		c.cookie, c.csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch settings: status = %d: %s", rec.Code, rec.Body.String())
	}

	home := c.do(t, http.MethodGet, "/", "", nil, nil)
	if home.Code != http.StatusOK {
		t.Fatalf("status = %d", home.Code)
	}
	body := home.Body.String()
	for _, want := range []string{
		`class="author-head has-cover"`,
		`class="author-cover"`,
		`src="` + cover + `"`,
		`<h2 class="author-name">遇事开心</h2>`,
		`<p class="author-bio">海边的个人博客</p>`,
		`src="` + avatar + `"`,
		// The row offers exactly the three filters the feed accepts, each with
		// its count, and marks the unfiltered view.
		`class="type-tab on" href="/" aria-current="page"`,
		`class="type-tab" href="/?type=article"`,
		`class="type-tab" href="/?type=photo"`,
		`<span class="type-tab-count">4</span>`,
		`<span class="type-tab-count">2</span>`,
		`<span class="type-tab-count">1</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the home page is missing %s", want)
		}
	}
	// docs/PRODUCT.md §5.1: moments only ever appear in the unfiltered feed, so
	// the row must not offer a filter the server answers with a 400.
	if strings.Contains(body, "type=moment") {
		t.Error("the type row offers a moment filter")
	}
	// The three facts each carry an icon. That is the shape the front door was
	// laid out to: a plain run of text reads as one sentence chopped up by
	// spaces, and the row is three parallel entries. The sprite symbols are
	// asserted alongside them because a <use> to a symbol whose viewBox has a
	// non-zero origin draws nothing at all while every markup assertion stays
	// green - the failure mode that already cost one afternoon here.
	location := loadLocation(settings.Defaults().SiteTimezone)
	sinceISO := oldest["published_at"].(string)
	updatedISO := newest["published_at"].(string)
	parsedSince, err := time.Parse(content.TimestampFormat, sinceISO)
	if err != nil {
		t.Fatalf("parse the oldest published_at %q: %v", sinceISO, err)
	}
	wantSince := parsedSince.In(location).Format("2006年1月2日")
	for _, want := range []string{
		`<span class="author-fact"><svg class="ic ic-fact" viewBox="0 0 24 24" aria-hidden="true"><use href="#i-calendar"/></svg>始于 <time datetime="` + sinceISO + `">` + wantSince + `</time></span>`,
		`<span class="author-fact"><svg class="ic ic-fact" viewBox="0 0 24 24" aria-hidden="true"><use href="#i-calendar"/></svg>最近更新于 <time datetime="` + updatedISO + `">`,
		`<a class="author-fact author-rss" href="/feed.xml"><svg class="ic ic-fact" viewBox="0 0 24 24" aria-hidden="true"><use href="#i-rss"/></svg>RSS 订阅</a>`,
		`<symbol id="i-rss" viewBox="0 0 24 24">`,
		`<symbol id="i-calendar" viewBox="0 0 24 24">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the author block is missing %s", want)
		}
	}
	if strings.Contains(body, `最近更新于 <time datetime="`+updatedISO+`"></time>`) {
		t.Error("the author block dates the site with an empty string")
	}
	// The avatar is a block of its own above the name, not a column beside it,
	// and it is bigger than a feed avatar. Both live in the stylesheet, so the
	// stylesheet is read: the markup alone cannot tell the two layouts apart.
	css := c.do(t, http.MethodGet, "/static/app.css", "", nil, nil).Body.String()
	for _, want := range []string{
		`.author-id{display:flex;flex-direction:column`,
		`.author-id .avatar{width:104px;height:104px;`,
		`.author-head.has-cover .author-id .avatar{margin-top:-52px}`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css no longer lays the author block out that way: missing %s", want)
		}
	}
	// Boop has one author and no follow graph (docs/PRODUCT.md §7), so the block
	// must not grow the empty follow/fan counters a user profile would carry.
	for _, unwanted := range []string{"关注", "粉丝", "合集"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the author block invented a %s counter", unwanted)
		}
	}

	// A filtered view marks its own entry and leaves "全部" unmarked.
	article := c.do(t, http.MethodGet, "/?type=article", "", nil, nil)
	if article.Code != http.StatusOK {
		t.Fatalf("article view status = %d", article.Code)
	}
	articleBody := article.Body.String()
	if !strings.Contains(articleBody, `class="type-tab on" href="/?type=article" aria-current="page"`) {
		t.Error("the article view does not mark its own entry")
	}
	if strings.Contains(articleBody, `class="type-tab on" href="/"`) {
		t.Error("the article view still marks 全部")
	}
	// The header is the site's identity, not a function of the filter, so it
	// stays on every view.
	if !strings.Contains(articleBody, `class="author-head has-cover"`) {
		t.Error("the filtered view dropped the author header")
	}

	// A site with no configured cover falls back to the built-in Boop cover.
	if rec := c.patchSettings(t, `{"site_cover_url":""}`, c.cookie, c.csrf); rec.Code != http.StatusOK {
		t.Fatalf("clear the cover: status = %d: %s", rec.Code, rec.Body.String())
	}
	plain := c.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
	if !strings.Contains(plain, `class="author-head has-cover"`) {
		t.Error("the header loses its cover shape when no custom image is configured")
	}
	if !strings.Contains(plain, `src="/static/brand/boop-cover.svg"`) {
		t.Error("a cleared cover image does not fall back to the built-in Boop cover")
	}
	if strings.Contains(plain, cover) {
		t.Error("the cleared custom cover image is still rendered")
	}
	if !strings.Contains(plain, `class="type-tabs"`) {
		// A guard, not a requirement of the feature: the two blocks are
		// independent, so losing the row here would mean the shared edit broke
		// both.
		t.Error("clearing the cover dropped the type row")
	}
}

func TestHomePageCursorLink(t *testing.T) {
	c := newContentFixture(t)
	for i := 0; i < content.DefaultPageSize+2; i++ {
		c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": fmt.Sprintf("条目 %02d", i)})
	}

	body := c.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
	if !strings.Contains(body, "cursor=") {
		t.Fatalf("no cursor link on a full page")
	}
	const marker = `/??`
	if strings.Contains(body, marker) {
		t.Errorf("cursor link has a malformed query: %s", body)
	}

	// The link must lead to the following page without repeating posts. The feed
	// is newest first, so page two holds the oldest entries.
	cursor := extractCursor(t, body)
	next := c.do(t, http.MethodGet, "/?cursor="+cursor, "", nil, nil)
	if next.Code != http.StatusOK {
		t.Fatalf("cursor page status = %d", next.Code)
	}
	second := next.Body.String()
	if !strings.Contains(second, "条目 00") || !strings.Contains(second, "条目 01") {
		t.Errorf("the second page is missing the two oldest posts")
	}
	if strings.Contains(second, "条目 21") {
		t.Errorf("the second page repeated a post from the first page")
	}
	// The author block belongs to the top of the feed, so a continuation page
	// does not repeat it; the type row does, because it is also the way back up
	// and its counts describe the whole site rather than this page.
	if strings.Contains(second, `class="author-head`) {
		t.Error("the cursor page repeated the author header")
	}
	if strings.Contains(second, `class="author-facts"`) {
		t.Error("the cursor page repeated the author header's dates")
	}
	if !strings.Contains(second, `class="type-tabs"`) {
		t.Error("the cursor page is missing the type row")
	}
}

func extractCursor(t *testing.T, body string) string {
	t.Helper()
	index := strings.Index(body, "cursor=")
	if index < 0 {
		t.Fatal("no cursor in the page")
	}
	rest := body[index+len("cursor="):]
	end := strings.IndexAny(rest, `&"`)
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func TestPostPageRendersSanitizedHTML(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "article", "status": "published", "title": "详情文章",
		"body": "<script>alert('xss')</script>\n\n# 小标题\n\n正文",
		"tags": []string{"写作"},
	})
	slug := created["slug"].(string)

	rec := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "详情文章") || !strings.Contains(body, "<h1>小标题</h1>") {
		t.Errorf("detail page misses the rendered body")
	}
	if strings.Contains(body, "alert('xss')") || strings.Contains(body, "<script>alert") {
		t.Errorf("detail page leaked the script payload")
	}
	if !strings.Contains(body, "写作") {
		t.Errorf("detail page misses tags")
	}

	if rec := c.do(t, http.MethodGet, "/p/unknown-slug", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown slug status = %d, want 404", rec.Code)
	}
}

func TestMomentPageEscapesMarkup(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{
		"type": "moment", "status": "published", "body": "看这个 <b>粗体</b> 标签",
	})
	slug := created["slug"].(string)

	body := c.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
	if strings.Contains(body, "<b>粗体</b>") {
		t.Errorf("moment markup was rendered: %s", body)
	}
	if !strings.Contains(body, "&lt;b&gt;") {
		t.Errorf("moment text is missing from the page")
	}
}

// ---------- quick publisher ----------

func TestComposerIsRenderedOnlyForTheOwner(t *testing.T) {
	markers := []string{`id="composer"`, `data-composer-form`, `data-mode="moment"`, `data-mode="article"`, `data-mode="photo"`, `data-composer-submit`, `data-composer-file`, "快捷发布"}

	t.Run("guest", func(t *testing.T) {
		f := newAuthFixture(t)
		body := f.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
		for _, marker := range markers {
			if strings.Contains(body, marker) {
				t.Errorf("guest DOM contains %q", marker)
			}
		}
	})

	t.Run("reader", func(t *testing.T) {
		f := newAuthFixture(t)
		cookie, _ := f.readerFixture(t)
		body := f.do(t, http.MethodGet, "/", "", nil, cookie).Body.String()
		for _, marker := range markers {
			if strings.Contains(body, marker) {
				t.Errorf("reader DOM contains %q", marker)
			}
		}
	})

	t.Run("owner", func(t *testing.T) {
		c := newContentFixture(t)
		body := c.do(t, http.MethodGet, "/", "", nil, c.cookie).Body.String()
		for _, marker := range markers {
			if !strings.Contains(body, marker) {
				t.Errorf("owner DOM is missing %q", marker)
			}
		}
		for _, field := range []string{`data-composer-text`, `data-composer-title`, `data-composer-tags`, `data-composer-excerpt`, `data-composer-error`, `data-composer-count`, `data-composer-drop`, `data-composer-file`, `data-composer-file-hint`, `data-composer-preview`} {
			if !strings.Contains(body, field) {
				t.Errorf("owner composer is missing %q", field)
			}
		}
		if !strings.Contains(body, `action="/api/v1/admin/posts"`) {
			t.Errorf("composer does not post to the publishing endpoint")
		}
		// 摄影模式必须有真实的文件选择与预览，而不是一句占位说明。
		for _, marker := range []string{`type="file"`, `accept="image/jpeg,image/png,image/webp,image/gif"`} {
			if !strings.Contains(body, marker) {
				t.Errorf("photo mode is missing %q", marker)
			}
		}
	})
}

func TestComposerReflectsFeedFilter(t *testing.T) {
	c := newContentFixture(t)
	body := c.do(t, http.MethodGet, "/?type=photo", "", nil, c.cookie).Body.String()
	if !strings.Contains(body, `data-mode="photo"`) || !strings.Contains(body, `aria-pressed="true"`) {
		t.Errorf("composer does not reflect the active filter")
	}
}

// ---------- integration: publish then read ----------

func TestOwnerPublishesAndReadersSeeIt(t *testing.T) {
	c := newContentFixture(t)
	created := c.createOK(t, map[string]any{"type": "article", "status": "published", "title": "发布流程", "body": "**正文**"})

	readerCookie, _ := c.readerFixture(t)
	rec := c.do(t, http.MethodGet, "/p/"+created["slug"].(string), "", nil, readerCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("reader detail status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "发布流程") {
		t.Errorf("reader cannot see the published post")
	}

	feed := decodeEnvelope(t, c.do(t, http.MethodGet, "/api/v1/posts", "", nil, readerCookie))["data"].([]any)
	if len(feed) != 1 {
		t.Errorf("reader feed has %d posts, want 1", len(feed))
	}
}

func TestQuickPublishPhotoModeWithoutUploadsFailsClearly(t *testing.T) {
	c := newContentFixture(t)

	// Exactly what the composer sends in photo mode before Task 5 exists.
	rec := c.createPost(t, map[string]any{"type": "photo", "status": "published", "body": "", "asset_ids": []int64{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	code, message, _ := decodeAPIError(t, rec)
	if code != "invalid_assets" {
		t.Errorf("code = %q, want invalid_assets", code)
	}
	if !strings.Contains(message, "图片") {
		t.Errorf("message %q does not explain the missing image", message)
	}
	if posts := c.countRows(t, "posts"); posts != 0 {
		t.Errorf("posts = %d, want no fake success", posts)
	}
}

func TestContentQueriesUseTheOwnerIdentity(t *testing.T) {
	c := newContentFixture(t)
	if _, err := c.db.ExecContext(context.Background(), `UPDATE users SET display_name = '新站长' WHERE id = ?`, c.owner.ID); err != nil {
		t.Fatalf("rename owner: %v", err)
	}
	c.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "署名测试"})

	body := c.do(t, http.MethodGet, "/", "", nil, c.cookie).Body.String()
	if !strings.Contains(body, "新站长") {
		t.Errorf("the post card does not use the owner display name")
	}
}
