package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"boop/internal/settings"
	"boop/internal/social"
)

// ---------- helpers ----------

// socialFixture is one owner plus one signed-in reader, both with their CSRF
// tokens, over a single server.
type socialFixture struct {
	*contentFixture
	readerCookie *http.Cookie
	readerCSRF   string
	// otherCookie and otherReaderCSRF belong to a second reader, created lazily
	// by isolation tests.
	otherCookie     *http.Cookie
	otherReaderCSRF string
}

func newSocialFixture(t *testing.T) *socialFixture {
	t.Helper()
	c := newContentFixture(t)
	cookie, csrf := c.readerFixture(t)
	return &socialFixture{contentFixture: c, readerCookie: cookie, readerCSRF: csrf}
}

// reply sends a comment as the given account.
func (f *socialFixture) comment(t *testing.T, postID int64, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	return f.postSameOrigin(t, fmt.Sprintf("/api/v1/posts/%d/comments", postID),
		marshalJSON(t, map[string]any{"body": body}), cookie, map[string]string{"X-CSRF-Token": csrf})
}

func (f *socialFixture) commentOK(t *testing.T, postID int64, body string, cookie *http.Cookie, csrf string) map[string]any {
	t.Helper()
	rec := f.comment(t, postID, body, cookie, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("comment: status = %d, body %s", rec.Code, rec.Body.String())
	}
	return decodeData(t, rec)
}

// replyTo sends a reply to an existing comment.
func (f *socialFixture) replyTo(t *testing.T, postID, parentID int64, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	return f.postSameOrigin(t, fmt.Sprintf("/api/v1/posts/%d/comments", postID),
		marshalJSON(t, map[string]any{"body": body, "parent_id": parentID}), cookie, map[string]string{"X-CSRF-Token": csrf})
}

// reaction sends a PUT or DELETE to a reaction endpoint.
func (f *socialFixture) reaction(t *testing.T, method, kind string, postID int64, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	target := fmt.Sprintf("/api/v1/posts/%d/%s", postID, kind)
	headers := map[string]string{}
	if csrf != "" {
		headers["X-CSRF-Token"] = csrf
	}
	return f.doHeaders(t, method, target, "", headers, cookie)
}

// doHeaders is do() with extra headers, so a missing CSRF header can be tested.
// Its parameters follow do()'s order: headers, then the cookie.
func (f *authFixture) doHeaders(t *testing.T, method, target, body string, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// updateSettings writes one setting through the typed settings package.
func (f *authFixture) updateSettings(t *testing.T, key string, value any) {
	t.Helper()
	if err := settings.Set(t.Context(), f.db, key, value); err != nil {
		t.Fatalf("settings.Set(%s): %v", key, err)
	}
}

// allowManyComments raises the comment budget for tests that are not about rate
// limiting; the limiter itself has dedicated tests.
func (f *socialFixture) allowManyComments(t *testing.T) {
	t.Helper()
	f.srv.limiters.comment = newLimiter(10_000, time.Second, time.Now)
	f.srv.limiters.commentIP = newLimiter(10_000, time.Second, time.Now)
}

// commentIDs flattens a comment list into its ids.
func commentIDs(t *testing.T, rec *httptest.ResponseRecorder) []int64 {
	t.Helper()
	envelope := decodeEnvelope(t, rec)
	items, ok := envelope["data"].([]any)
	if !ok {
		t.Fatalf("data = %v, want a list", envelope["data"])
	}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		entry, _ := item.(map[string]any)
		id, _ := entry["id"].(float64)
		ids = append(ids, int64(id))
	}
	return ids
}

// ---------- authorization ----------

func TestCommentsAPIRequiresNothingAndHidesDrafts(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	draft := f.createOK(t, map[string]any{"type": "moment", "status": "draft", "body": "还没写完"})
	f.commentOK(t, int64(post["id"].(float64)), "第一条评论", f.readerCookie, f.readerCSRF)

	t.Run("guests read the thread", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/posts/"+post["slug"].(string)+"/comments", "", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if ids := commentIDs(t, rec); len(ids) != 1 {
			t.Errorf("comments = %v, want one public comment", ids)
		}
	})

	t.Run("drafts stay invisible", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/posts/"+draft["slug"].(string)+"/comments", "", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d for a draft's comments: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestCommentWritesRequireAnAccountAndCSRF(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	id := int64(post["id"].(float64))

	t.Run("guest", func(t *testing.T) {
		rec := f.postSameOrigin(t, fmt.Sprintf("/api/v1/posts/%d/comments", id), marshalJSON(t, map[string]any{"body": "我可以吗"}), nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "unauthorized" {
			t.Errorf("code = %q", code)
		}
	})

	t.Run("missing csrf", func(t *testing.T) {
		rec := f.comment(t, id, "缺少令牌", f.readerCookie, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "csrf_invalid" {
			t.Errorf("code = %q", code)
		}
	})

	t.Run("cross origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/comments", id),
			strings.NewReader(marshalJSON(t, map[string]any{"body": "越站"})))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://evil.example.com")
		req.Header.Set("X-CSRF-Token", f.readerCSRF)
		req.AddCookie(f.readerCookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "origin_mismatch" {
			t.Errorf("code = %q, want origin_mismatch", code)
		}
	})

	t.Run("readers may react but not moderate", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/admin/comments?status=pending", "", nil, f.readerCookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("guests may not react", func(t *testing.T) {
		// With origin evidence but no session, the write reaches the handler and
		// is refused for the honest reason: not signed in.
		rec := f.doHeaders(t, http.MethodPut, fmt.Sprintf("/api/v1/posts/%d/like", id), "", nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "unauthorized" {
			t.Errorf("code = %q, want unauthorized", code)
		}
	})

	t.Run("a write without any origin evidence is refused", func(t *testing.T) {
		rec := f.do(t, http.MethodPut, fmt.Sprintf("/api/v1/posts/%d/like", id), "", nil, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "origin_required" {
			t.Errorf("code = %q, want origin_required", code)
		}
	})

	if comments := f.countRows(t, "comments"); comments != 0 {
		t.Errorf("comments = %d, want none", comments)
	}
}

// ---------- settings ----------

func TestCommentSettingsGateWrites(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	id := int64(post["id"].(float64))

	t.Run("comments disabled refuses writes but keeps reading", func(t *testing.T) {
		f.commentOK(t, id, "关闭之前写的", f.readerCookie, f.readerCSRF)
		f.updateSettings(t, settings.KeyCommentsEnabled, false)

		rec := f.comment(t, id, "关闭之后写的", f.readerCookie, f.readerCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "comments_disabled" {
			t.Errorf("code = %q, want comments_disabled", code)
		}
		if comments := f.countRows(t, "comments"); comments != 1 {
			t.Errorf("comments = %d, want only the earlier one", comments)
		}

		// Existing public comments stay readable, and the page says why the form
		// is unavailable instead of offering a control that cannot work.
		thread := f.do(t, http.MethodGet, "/api/v1/posts/"+post["slug"].(string)+"/comments", "", nil, nil)
		if ids := commentIDs(t, thread); len(ids) != 1 {
			t.Errorf("public thread = %v, want the earlier comment", ids)
		}
		page := f.do(t, http.MethodGet, "/p/"+post["slug"].(string), "", nil, f.readerCookie).Body.String()
		if !strings.Contains(page, "评论已关闭") {
			t.Error("detail page does not explain that comments are closed")
		}
	})
}

func TestModerationMakesReaderCommentsPending(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	id := int64(post["id"].(float64))
	f.updateSettings(t, settings.KeyCommentsModerationEnabled, true)

	readerComment := f.commentOK(t, id, "读者发言", f.readerCookie, f.readerCSRF)
	if readerComment["status"] != social.StatusPending {
		t.Errorf("reader status = %v, want pending", readerComment["status"])
	}
	ownerComment := f.commentOK(t, id, "站长的回复", f.cookie, f.csrf)
	if ownerComment["status"] != social.StatusApproved {
		t.Errorf("owner status = %v, want approved", ownerComment["status"])
	}

	slug := post["slug"].(string)
	t.Run("pending never reaches the public thread", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/posts/"+slug+"/comments", "", nil, nil)
		ids := commentIDs(t, rec)
		if len(ids) != 1 || ids[0] != int64(ownerComment["id"].(float64)) {
			t.Fatalf("public thread = %v, want only the owner's comment", ids)
		}
		page := f.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
		if strings.Contains(page, "读者发言") {
			t.Error("the SSR page leaks a pending comment")
		}
		if !strings.Contains(page, "站长的回复") {
			t.Error("the SSR page hides an approved comment")
		}
	})

	t.Run("the owner queue lists it and moderation decides", func(t *testing.T) {
		queue := f.do(t, http.MethodGet, "/api/v1/admin/comments?status=pending", "", nil, f.cookie)
		if queue.Code != http.StatusOK {
			t.Fatalf("queue status = %d: %s", queue.Code, queue.Body.String())
		}
		ids := commentIDs(t, queue)
		pendingID := int64(readerComment["id"].(float64))
		if len(ids) != 1 || ids[0] != pendingID {
			t.Fatalf("queue = %v, want the pending comment", ids)
		}

		approve := f.approve(t, pendingID, "approve")
		if approve.Code != http.StatusOK {
			t.Fatalf("approve status = %d: %s", approve.Code, approve.Body.String())
		}
		if decodeData(t, approve)["status"] != social.StatusApproved {
			t.Errorf("approve did not approve: %s", approve.Body.String())
		}
		// Idempotent: the same decision again is still 200 with the same state.
		again := f.approve(t, pendingID, "approve")
		if again.Code != http.StatusOK {
			t.Fatalf("second approve status = %d", again.Code)
		}
		thread := f.do(t, http.MethodGet, "/api/v1/posts/"+slug+"/comments", "", nil, nil)
		if ids := commentIDs(t, thread); len(ids) != 2 {
			t.Errorf("thread = %v, want both comments", ids)
		}

		reject := f.approve(t, pendingID, "reject")
		if reject.Code != http.StatusOK {
			t.Fatalf("reject status = %d", reject.Code)
		}
		if decodeData(t, reject)["status"] != social.StatusRejected {
			t.Errorf("reject did not reject: %s", reject.Body.String())
		}
		thread = f.do(t, http.MethodGet, "/api/v1/posts/"+slug+"/comments", "", nil, nil)
		if ids := commentIDs(t, thread); len(ids) != 1 {
			t.Errorf("thread = %v, want the rejected comment gone", ids)
		}
	})

	t.Run("moderation requires the owner and a real comment", func(t *testing.T) {
		id := int64(readerComment["id"].(float64))
		if rec := f.approveWith(t, id, "approve", f.readerCookie, f.readerCSRF); rec.Code != http.StatusForbidden {
			t.Errorf("reader approve status = %d, want 403", rec.Code)
		}
		if rec := f.approve(t, 99999, "approve"); rec.Code != http.StatusNotFound {
			t.Errorf("unknown comment status = %d, want 404", rec.Code)
		}
		if rec := f.approve(t, 0, "approve"); rec.Code != http.StatusBadRequest {
			t.Errorf("bad id status = %d, want 400", rec.Code)
		}
	})
}

// approve runs one moderation action as the owner.
func (f *socialFixture) approve(t *testing.T, id int64, action string) *httptest.ResponseRecorder {
	t.Helper()
	return f.approveWith(t, id, action, f.cookie, f.csrf)
}

func (f *socialFixture) approveWith(t *testing.T, id int64, action string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	return f.postSameOrigin(t, fmt.Sprintf("/api/v1/admin/comments/%d/%s", id, action), "", cookie, map[string]string{"X-CSRF-Token": csrf})
}

// ---------- replies ----------

func TestReplyInvariantsOverHTTP(t *testing.T) {
	f := newSocialFixture(t)
	f.allowManyComments(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	other := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "另一篇内容"})
	postID := int64(post["id"].(float64))
	otherID := int64(other["id"].(float64))

	root := f.commentOK(t, postID, "顶层评论", f.readerCookie, f.readerCSRF)
	rootID := int64(root["id"].(float64))

	t.Run("one level of replies", func(t *testing.T) {
		rec := f.replyTo(t, postID, rootID, "一级回复", f.readerCookie, f.readerCSRF)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if decodeData(t, rec)["parent_id"].(float64) != float64(rootID) {
			t.Error("the reply did not record its parent")
		}
		thread := f.do(t, http.MethodGet, "/api/v1/posts/"+post["slug"].(string)+"/comments", "", nil, nil)
		envelope := decodeEnvelope(t, thread)
		items := envelope["data"].([]any)
		first := items[0].(map[string]any)
		if replies, _ := first["replies"].([]any); len(replies) != 1 {
			t.Fatalf("replies = %v, want one", first["replies"])
		}
	})

	t.Run("a reply cannot be replied to", func(t *testing.T) {
		thread := f.do(t, http.MethodGet, "/api/v1/posts/"+post["slug"].(string)+"/comments", "", nil, nil)
		items := decodeEnvelope(t, thread)["data"].([]any)
		replyID := int64(items[0].(map[string]any)["replies"].([]any)[0].(map[string]any)["id"].(float64))

		rec := f.replyTo(t, postID, replyID, "二级回复", f.readerCookie, f.readerCSRF)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_parent" {
			t.Errorf("code = %q, want invalid_parent", code)
		}
	})

	t.Run("a parent must belong to the same post", func(t *testing.T) {
		foreign := f.commentOK(t, otherID, "另一篇的评论", f.readerCookie, f.readerCSRF)
		rec := f.replyTo(t, postID, int64(foreign["id"].(float64)), "跨文章回复", f.readerCookie, f.readerCSRF)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_parent" {
			t.Errorf("code = %q, want invalid_parent", code)
		}
	})

	t.Run("body bounds are enforced", func(t *testing.T) {
		for name, body := range map[string]string{"blank": "   ", "too long": strings.Repeat("字", 2001)} {
			rec := f.comment(t, postID, body, f.readerCookie, f.readerCSRF)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400", name, rec.Code)
			}
			if code, _, _ := decodeAPIError(t, rec); code != "invalid_body" {
				t.Errorf("%s: code = %q", name, code)
			}
		}
		// The exact limit is accepted.
		rec := f.comment(t, postID, strings.Repeat("字", 2000), f.readerCookie, f.readerCSRF)
		if rec.Code != http.StatusCreated {
			t.Fatalf("2000 characters: status = %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("comments on drafts are refused", func(t *testing.T) {
		draft := f.createOK(t, map[string]any{"type": "moment", "status": "draft", "body": "草稿"})
		rec := f.comment(t, int64(draft["id"].(float64)), "草稿评论", f.readerCookie, f.readerCSRF)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

// ---------- deletion ----------

func TestCommentDeletionAuthorizationAndVisibility(t *testing.T) {
	f := newSocialFixture(t)
	f.allowManyComments(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	slug := post["slug"].(string)
	postID := int64(post["id"].(float64))

	parent := f.commentOK(t, postID, "父评论", f.readerCookie, f.readerCSRF)
	parentID := int64(parent["id"].(float64))
	// Attach the child to the parent as a reply.
	reply := f.replyTo(t, postID, parentID, "对父评论的回复", f.cookie, f.csrf)
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply status = %d: %s", reply.Code, reply.Body.String())
	}

	t.Run("another reader may not delete", func(t *testing.T) {
		rec := f.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/comments/%d", parentID), "", map[string]string{"X-CSRF-Token": f.otherReaderCSRF}, f.otherReaderCookie(t))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the author deletes and replies disappear with the parent", func(t *testing.T) {
		rec := f.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/comments/%d", parentID), "", map[string]string{"X-CSRF-Token": f.readerCSRF}, f.readerCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		thread := f.do(t, http.MethodGet, "/api/v1/posts/"+slug+"/comments", "", nil, nil)
		if ids := commentIDs(t, thread); len(ids) != 0 {
			t.Errorf("thread = %v, want nothing: a deleted parent must hide its replies", ids)
		}
		page := f.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
		if strings.Contains(page, "对父评论的回复") {
			t.Error("the SSR page still shows a reply under a deleted parent")
		}
	})

	t.Run("deleting twice is a clear 404", func(t *testing.T) {
		rec := f.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/comments/%d", parentID), "", map[string]string{"X-CSRF-Token": f.readerCSRF}, f.readerCookie)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("the owner may delete any comment", func(t *testing.T) {
		other := f.commentOK(t, postID, "读者写的", f.readerCookie, f.readerCSRF)
		rec := f.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/comments/%d", int64(other["id"].(float64))), "", map[string]string{"X-CSRF-Token": f.csrf}, f.cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// otherReaderCookie registers a second reader for isolation tests.
func (f *socialFixture) otherReaderCookie(t *testing.T) *http.Cookie {
	t.Helper()
	if f.otherCookie == nil {
		rec := f.register(t, "other@example.com", "读者乙")
		if rec.Code != http.StatusCreated {
			t.Fatalf("register second reader: status = %d", rec.Code)
		}
		f.otherCookie = sessionCookie(t, rec)
		f.otherReaderCSRF = decodeData(t, rec)["csrf_token"].(string)
	}
	return f.otherCookie
}

// ---------- likes and bookmarks over HTTP ----------

func TestReactionsAreIdempotentOverHTTP(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	id := int64(post["id"].(float64))

	first := f.reaction(t, http.MethodPut, "like", id, f.readerCookie, f.readerCSRF)
	if first.Code != http.StatusOK {
		t.Fatalf("like status = %d: %s", first.Code, first.Body.String())
	}
	data := decodeData(t, first)
	if data["liked"] != true || data["like_count"].(float64) != 1 {
		t.Fatalf("like = %v, want on with count 1", data)
	}
	again := f.reaction(t, http.MethodPut, "like", id, f.readerCookie, f.readerCSRF)
	if again.Code != http.StatusOK {
		t.Fatalf("second like status = %d", again.Code)
	}
	if decodeData(t, again)["like_count"].(float64) != 1 {
		t.Error("a repeated like changed the count")
	}
	if likes := f.countRows(t, "likes"); likes != 1 {
		t.Errorf("likes = %d, want 1", likes)
	}

	off := f.reaction(t, http.MethodDelete, "like", id, f.readerCookie, f.readerCSRF)
	if off.Code != http.StatusOK {
		t.Fatalf("unlike status = %d", off.Code)
	}
	if data := decodeData(t, off); data["liked"] != false || data["like_count"].(float64) != 0 {
		t.Errorf("unlike = %v", data)
	}
	if repeat := f.reaction(t, http.MethodDelete, "like", id, f.readerCookie, f.readerCSRF); repeat.Code != http.StatusOK {
		t.Errorf("second unlike status = %d, want an idempotent 200", repeat.Code)
	}

	t.Run("bookmarks report the viewer's own total", func(t *testing.T) {
		on := f.reaction(t, http.MethodPut, "bookmark", id, f.readerCookie, f.readerCSRF)
		if on.Code != http.StatusOK {
			t.Fatalf("bookmark status = %d: %s", on.Code, on.Body.String())
		}
		data := decodeData(t, on)
		if data["bookmarked"] != true || data["bookmark_count"].(float64) != 1 {
			t.Fatalf("bookmark = %v", data)
		}
		// Another reader's bookmark does not change this viewer's total.
		other := f.reaction(t, http.MethodPut, "bookmark", id, f.otherReaderCookie(t), f.otherReaderCSRF)
		if other.Code != http.StatusOK {
			t.Fatalf("second reader bookmark status = %d", other.Code)
		}
		if count := decodeData(t, other)["bookmark_count"].(float64); count != 1 {
			t.Errorf("second reader total = %v, want 1", count)
		}
	})

	t.Run("drafts and unknown ids", func(t *testing.T) {
		draft := f.createOK(t, map[string]any{"type": "moment", "status": "draft", "body": "草稿"})
		for _, target := range []int64{int64(draft["id"].(float64)), 99999} {
			rec := f.reaction(t, http.MethodPut, "like", target, f.readerCookie, f.readerCSRF)
			if rec.Code != http.StatusNotFound {
				t.Errorf("like %d: status = %d, want 404", target, rec.Code)
			}
		}
		rec := f.do(t, http.MethodPut, "/api/v1/posts/abc/like", "", map[string]string{"X-CSRF-Token": f.readerCSRF}, f.readerCookie)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad id status = %d, want 400", rec.Code)
		}
	})

	t.Run("csrf is required", func(t *testing.T) {
		rec := f.reaction(t, http.MethodPut, "like", id, f.readerCookie, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("viewer state appears in the detail and feed APIs", func(t *testing.T) {
		f.reaction(t, http.MethodPut, "like", id, f.readerCookie, f.readerCSRF)
		detail := f.do(t, http.MethodGet, "/api/v1/posts/"+post["slug"].(string), "", nil, f.readerCookie)
		data := decodeData(t, detail)
		if data["liked"] != true {
			t.Errorf("detail liked = %v, want true", data["liked"])
		}
		if data["bookmarked"] != true {
			t.Errorf("detail bookmarked = %v, want true", data["bookmarked"])
		}

		guest := decodeData(t, f.do(t, http.MethodGet, "/api/v1/posts/"+post["slug"].(string), "", nil, nil))
		if guest["liked"] != false || guest["bookmarked"] != false {
			t.Errorf("guest state = %v/%v, want false", guest["liked"], guest["bookmarked"])
		}

		feed := decodeEnvelope(t, f.do(t, http.MethodGet, "/api/v1/posts", "", nil, f.readerCookie))
		items := feed["data"].([]any)
		if items[0].(map[string]any)["liked"] != true {
			t.Error("the feed does not carry the viewer's like state")
		}
	})
}

func TestBookmarksAPIAndPageArePrivate(t *testing.T) {
	f := newSocialFixture(t)
	first := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "第一篇"})
	second := f.createOK(t, map[string]any{"type": "article", "status": "published", "title": "第二篇", "body": "正文内容"})
	firstID := int64(first["id"].(float64))
	secondID := int64(second["id"].(float64))

	f.reaction(t, http.MethodPut, "bookmark", firstID, f.readerCookie, f.readerCSRF)
	f.reaction(t, http.MethodPut, "bookmark", secondID, f.readerCookie, f.readerCSRF)
	f.reaction(t, http.MethodPut, "bookmark", secondID, f.otherReaderCookie(t), f.otherReaderCSRF)

	t.Run("the API lists only the caller's bookmarks", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/me/bookmarks", "", nil, f.readerCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		envelope := decodeEnvelope(t, rec)
		items := envelope["data"].([]any)
		if len(items) != 2 {
			t.Fatalf("bookmarks = %d, want 2", len(items))
		}
		ids := []int64{}
		for _, item := range items {
			entry := item.(map[string]any)
			ids = append(ids, int64(entry["id"].(float64)))
			if entry["bookmarked"] != true {
				t.Errorf("bookmark state = %v, want true", entry["bookmarked"])
			}
		}
		// Newest bookmark first.
		if ids[0] != secondID || ids[1] != firstID {
			t.Errorf("order = %v, want newest first", ids)
		}

		other := decodeEnvelope(t, f.do(t, http.MethodGet, "/api/v1/me/bookmarks", "", nil, f.otherReaderCookie(t)))
		if len(other["data"].([]any)) != 1 {
			t.Errorf("another reader sees %d bookmarks, want 1", len(other["data"].([]any)))
		}
	})

	t.Run("pagination uses a stable cursor", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/me/bookmarks?limit=1", "", nil, f.readerCookie)
		envelope := decodeEnvelope(t, rec)
		cursor, _ := envelope["next_cursor"].(string)
		if cursor == "" {
			t.Fatal("first page has no cursor")
		}
		second := decodeEnvelope(t, f.do(t, http.MethodGet, "/api/v1/me/bookmarks?limit=1&cursor="+cursor, "", nil, f.readerCookie))
		items := second["data"].([]any)
		if len(items) != 1 {
			t.Fatalf("second page = %v", items)
		}
		if int64(items[0].(map[string]any)["id"].(float64)) == int64(envelope["data"].([]any)[0].(map[string]any)["id"].(float64)) {
			t.Error("the second page repeated the first page")
		}
		if second["next_cursor"] != "" {
			t.Errorf("last page cursor = %v, want empty", second["next_cursor"])
		}
	})

	t.Run("a bad cursor is a 400", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/me/bookmarks?cursor=nope", "", nil, f.readerCookie)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if code, _, _ := decodeAPIError(t, rec); code != "invalid_cursor" {
			t.Errorf("code = %q", code)
		}
		bad := f.do(t, http.MethodGet, "/api/v1/me/bookmarks?limit=99", "", nil, f.readerCookie)
		if bad.Code != http.StatusBadRequest {
			t.Errorf("limit status = %d, want 400", bad.Code)
		}
	})

	t.Run("guests are refused", func(t *testing.T) {
		if rec := f.do(t, http.MethodGet, "/api/v1/me/bookmarks", "", nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("the page renders cards and sends guests to sign in", func(t *testing.T) {
		page := f.do(t, http.MethodGet, "/bookmarks", "", nil, f.readerCookie)
		if page.Code != http.StatusOK {
			t.Fatalf("status = %d", page.Code)
		}
		body := page.Body.String()
		if !strings.Contains(body, "第二篇") {
			t.Error("the bookmarks page does not render the bookmarked article")
		}
		if !strings.Contains(body, `data-reaction="bookmark"`) {
			t.Error("the page does not render bookmark buttons")
		}
		// The 收藏 navigation entry is marked current.
		if !strings.Contains(body, `aria-current="page"`) {
			t.Error("the shell does not mark the bookmarks section")
		}

		guest := f.do(t, http.MethodGet, "/bookmarks", "", nil, nil)
		if guest.Code != http.StatusSeeOther {
			t.Fatalf("guest status = %d, want a redirect", guest.Code)
		}
		if location := guest.Header().Get("Location"); location != "/login" {
			t.Errorf("guest redirect = %q, want /login", location)
		}
	})

	t.Run("the empty state is honest", func(t *testing.T) {
		empty := f.register(t, "empty@example.com", "空收藏")
		cookie := sessionCookie(t, empty)
		page := f.do(t, http.MethodGet, "/bookmarks", "", nil, cookie).Body.String()
		if !strings.Contains(page, "还没有收藏") {
			t.Error("an empty bookmark list has no empty state")
		}
	})
}

// ---------- SSR ----------

func TestPostPageRendersCommentsWithoutJavaScript(t *testing.T) {
	f := newSocialFixture(t)
	f.allowManyComments(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	slug := post["slug"].(string)
	postID := int64(post["id"].(float64))
	root := f.commentOK(t, postID, "第一条公开评论", f.readerCookie, f.readerCSRF)
	f.replyTo(t, postID, int64(root["id"].(float64)), "站长的一级回复", f.cookie, f.csrf)

	t.Run("guests read the thread and get a sign-in link", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/p/"+slug, "", nil, nil)
		body := rec.Body.String()
		if !strings.Contains(body, "第一条公开评论") || !strings.Contains(body, "站长的一级回复") {
			t.Error("the SSR page does not render the public thread")
		}
		if strings.Contains(body, "data-comment-form") {
			t.Error("a guest must not receive the comment form")
		}
		if !strings.Contains(body, "去登录") {
			t.Error("a guest sees no sign-in hint")
		}
		// The comment body is escaped, not interpreted.
		escaped := f.commentOK(t, postID, "<script>alert(1)</script>", f.readerCookie, f.readerCSRF)
		_ = escaped
		page := f.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
		if strings.Contains(page, "<script>alert(1)</script>") {
			t.Error("a comment body was rendered as HTML")
		}
	})

	t.Run("signed-in readers get the form and reply controls", func(t *testing.T) {
		body := f.do(t, http.MethodGet, "/p/"+slug, "", nil, f.readerCookie).Body.String()
		for _, marker := range []string{"data-comment-form", "data-comment-text", "data-comment-submit", "data-comment-reply-to", "data-comment-parent"} {
			if !strings.Contains(body, marker) {
				t.Errorf("the comment form is missing %q", marker)
			}
		}
		if !strings.Contains(body, `data-reaction="like"`) || !strings.Contains(body, `data-reaction="bookmark"`) {
			t.Error("the detail page has no reaction controls")
		}
	})

	t.Run("the author sees a delete control on their own comment", func(t *testing.T) {
		body := f.do(t, http.MethodGet, "/p/"+slug, "", nil, f.readerCookie).Body.String()
		if !strings.Contains(body, "data-comment-delete") {
			t.Error("the comment author has no delete control")
		}
		guest := f.do(t, http.MethodGet, "/p/"+slug, "", nil, nil).Body.String()
		if strings.Contains(guest, "data-comment-delete") {
			t.Error("a guest received a delete control")
		}
	})

	t.Run("home cards carry real reaction controls for readers only", func(t *testing.T) {
		reader := f.do(t, http.MethodGet, "/", "", nil, f.readerCookie).Body.String()
		if !strings.Contains(reader, `data-reaction="like"`) || !strings.Contains(reader, `data-post-id="`+strconv.FormatInt(postID, 10)+`"`) {
			t.Error("the reader's home cards lack reaction buttons")
		}
		guest := f.do(t, http.MethodGet, "/", "", nil, nil).Body.String()
		if strings.Contains(guest, `data-reaction="like"`) {
			t.Error("a guest received a write control instead of a sign-in link")
		}
		if !strings.Contains(guest, `href="/login"`) {
			t.Error("a guest has no sign-in link on the cards")
		}
	})
}

func TestAdminCommentsPage(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	postID := int64(post["id"].(float64))
	f.updateSettings(t, settings.KeyCommentsModerationEnabled, true)
	f.commentOK(t, postID, "待审核评论", f.readerCookie, f.readerCSRF)

	t.Run("guests are sent to sign in and readers are refused", func(t *testing.T) {
		guest := f.do(t, http.MethodGet, "/admin/comments", "", nil, nil)
		if guest.Code != http.StatusSeeOther {
			t.Fatalf("guest status = %d, want a redirect", guest.Code)
		}
		reader := f.do(t, http.MethodGet, "/admin/comments", "", nil, f.readerCookie)
		if reader.Code != http.StatusForbidden {
			t.Fatalf("reader status = %d, want 403", reader.Code)
		}
	})

	t.Run("the owner sees the queue and its controls", func(t *testing.T) {
		body := f.do(t, http.MethodGet, "/admin/comments", "", nil, f.cookie).Body.String()
		for _, marker := range []string{"待审核评论", "data-moderation=\"approve\"", "data-moderation=\"reject\"", "data-moderation=\"delete\"", "status=approved"} {
			if !strings.Contains(body, marker) {
				t.Errorf("the moderation page is missing %q", marker)
			}
		}
		approved := f.do(t, http.MethodGet, "/admin/comments?status=approved", "", nil, f.cookie).Body.String()
		if !strings.Contains(approved, "这个状态下没有评论") {
			t.Error("an empty queue has no empty state")
		}
		bad := f.do(t, http.MethodGet, "/admin/comments?status=hidden", "", nil, f.cookie)
		if bad.Code != http.StatusBadRequest {
			t.Errorf("bad status = %d, want 400", bad.Code)
		}
	})
}

// ---------- rate limits ----------

func TestLoginRateLimitRefusesWithRetryAfter(t *testing.T) {
	f := newSocialFixture(t)
	body := marshalJSON(t, map[string]any{"email": "reader@example.com", "password": "wrong password"})

	var limited *httptest.ResponseRecorder
	for i := 0; i < loginBurst+1; i++ {
		limited = f.postSameOrigin(t, "/api/v1/auth/login", body, nil, nil)
	}
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d after %d attempts, want 429: %s", limited.Code, loginBurst+1, limited.Body.String())
	}
	header := limited.Header().Get("Retry-After")
	if header == "" {
		t.Fatal("no Retry-After header")
	}
	if seconds, err := strconv.Atoi(header); err != nil || seconds < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", header)
	}
	envelope := decodeEnvelope(t, limited)
	if envelope["retry_after"] == nil {
		t.Error("the body does not carry retry_after")
	}
	code, _, _ := decodeAPIError(t, limited)
	if code != "rate_limited" {
		t.Errorf("code = %q, want rate_limited", code)
	}

	t.Run("another account from another address is unaffected", func(t *testing.T) {
		other := marshalJSON(t, map[string]any{"email": "other@example.com", "password": "wrong password"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(other))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", testOrigin)
		// A different address and a different mailbox share neither bucket.
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Error("one address's attempts blocked a different address")
		}
	})

	t.Run("the same address stays limited", func(t *testing.T) {
		body := marshalJSON(t, map[string]any{"email": "third@example.com", "password": "wrong password"})
		rec := f.postSameOrigin(t, "/api/v1/auth/login", body, nil, nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want the address limit to hold", rec.Code)
		}
	})

	t.Run("register is limited too", func(t *testing.T) {
		var last *httptest.ResponseRecorder
		for i := 0; i < registerBurst+1; i++ {
			email := fmt.Sprintf("burst%d@example.com", i)
			last = f.postSameOrigin(t, "/api/v1/auth/register",
				marshalJSON(t, map[string]any{"email": email, "password": authPassword, "display_name": "批量"}), nil, nil)
		}
		if last.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429: %s", last.Code, last.Body.String())
		}
	})
}

func TestCommentRateLimit(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	id := int64(post["id"].(float64))

	var limited *httptest.ResponseRecorder
	for i := 0; i < commentBurst+1; i++ {
		limited = f.comment(t, id, fmt.Sprintf("第 %d 条评论", i), f.readerCookie, f.readerCSRF)
	}
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", limited.Code, limited.Body.String())
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	if comments := f.countRows(t, "comments"); comments != commentBurst {
		t.Errorf("comments = %d, want exactly the burst of %d", comments, commentBurst)
	}

	t.Run("another reader still comments", func(t *testing.T) {
		// The per-account budget is separate from the address budget, so a
		// different account on the same network can still comment.
		f.otherReaderCookie(t)
		rec := f.comment(t, id, "别的读者", f.otherCookie, f.otherReaderCSRF)
		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want 201: %s", rec.Code, rec.Body.String())
		}
	})
}

// ---------- method fallbacks ----------

func TestInteractionRoutesAnswerJSONForBadMethods(t *testing.T) {
	f := newSocialFixture(t)
	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	id := strconv.FormatInt(int64(post["id"].(float64)), 10)

	cases := []struct {
		target string
		allow  string
	}{
		{"/api/v1/posts/" + id + "/like", "PUT, DELETE"},
		{"/api/v1/posts/" + id + "/bookmark", "PUT, DELETE"},
		{"/api/v1/posts/x/comments", "GET, POST"},
		{"/api/v1/comments/1", "DELETE"},
		{"/api/v1/me/bookmarks", "GET"},
		{"/api/v1/admin/comments", "GET"},
		{"/api/v1/admin/comments/1/approve", "POST"},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			rec := f.doHeaders(t, http.MethodPatch, tc.target, "", nil, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
			}
			if allow := rec.Header().Get("Allow"); allow != tc.allow {
				t.Errorf("Allow = %q, want %q", allow, tc.allow)
			}
			if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", contentType)
			}
		})
	}

	for _, unknown := range []string{"/api/v1/posts/1/comments/extra", "/api/v1/comments/1/extra", "/api/v1/me/unknown", "/api/v1/admin/comments/1/unknown"} {
		t.Run(unknown, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, unknown, "", nil, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", contentType)
			}
		})
	}
}
