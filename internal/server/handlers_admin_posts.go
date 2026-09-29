package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"boop/internal/auth"
	"boop/internal/content"
)

// createPostRequest is the strict create payload (docs/API.md §站长内容管理).
type createPostRequest struct {
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Excerpt    string   `json:"excerpt"`
	AssetIDs   []int64  `json:"asset_ids"`
	Tags       []string `json:"tags"`
	Location   string   `json:"location"`
	CapturedAt string   `json:"captured_at"`
}

// patchPostRequest is a partial update. Type is absent on purpose: a post never
// changes type, and sending it is rejected as an unknown field.
type patchPostRequest struct {
	UpdatedAt  *string   `json:"updated_at"`
	Status     *string   `json:"status"`
	Title      *string   `json:"title"`
	Body       *string   `json:"body"`
	Excerpt    *string   `json:"excerpt"`
	AssetIDs   *[]int64  `json:"asset_ids"`
	Tags       *[]string `json:"tags"`
	Location   *string   `json:"location"`
	CapturedAt *string   `json:"captured_at"`
}

// requireOwner is the single authorization gate for content writes: guests get
// 401, readers get 403, and only the owner continues.
func (s *server) requireOwner(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	state, _ := authStateFrom(r.Context())
	if !state.authenticated {
		writeFailure(w, r, http.StatusUnauthorized, "unauthorized", "请先登录")
		return auth.User{}, false
	}
	if !state.user.IsOwner() {
		writeFailure(w, r, http.StatusForbidden, "forbidden", "只有站长可以发布内容")
		return auth.User{}, false
	}
	return state.user, true
}

func (s *server) handleCreatePostAPI(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireOwner(w, r)
	if !ok {
		return
	}
	var body createPostRequest
	if !s.readJSON(w, r, &body, contentJSONBytes) {
		return
	}
	post, err := content.Create(r.Context(), s.db, owner.ID, content.Input{
		Type:       body.Type,
		Status:     body.Status,
		Title:      body.Title,
		Body:       body.Body,
		Excerpt:    body.Excerpt,
		AssetIDs:   body.AssetIDs,
		Tags:       body.Tags,
		Location:   body.Location,
		CapturedAt: body.CapturedAt,
	}, time.Now())
	if err != nil {
		s.writeContentFailure(w, r, "create post", err)
		return
	}
	payload := s.postPayloadOf(*post)
	applyViewerState(&payload, s.viewerStates(r.Context(), r, []int64{post.ID}))
	writeJSON(w, http.StatusCreated, map[string]any{"data": payload})
}

func (s *server) handlePatchPostAPI(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireOwner(w, r)
	if !ok {
		return
	}
	id, ok := s.postIDFrom(w, r)
	if !ok {
		return
	}
	var body patchPostRequest
	if !s.readJSON(w, r, &body, contentJSONBytes) {
		return
	}
	patch := content.Patch{
		Status:     body.Status,
		Title:      body.Title,
		Body:       body.Body,
		Excerpt:    body.Excerpt,
		AssetIDs:   body.AssetIDs,
		Tags:       body.Tags,
		Location:   body.Location,
		CapturedAt: body.CapturedAt,
	}
	if body.UpdatedAt != nil {
		patch.UpdatedAt = *body.UpdatedAt
	}
	post, err := content.Update(r.Context(), s.db, owner.ID, id, patch, time.Now())
	if err != nil {
		s.writeContentFailure(w, r, "update post", err)
		return
	}
	payload := s.postPayloadOf(*post)
	applyViewerState(&payload, s.viewerStates(r.Context(), r, []int64{post.ID}))
	writeJSON(w, http.StatusOK, map[string]any{"data": payload})
}

func (s *server) handleDeletePostAPI(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOwner(w, r); !ok {
		return
	}
	id, ok := s.postIDFrom(w, r)
	if !ok {
		return
	}
	if err := content.Delete(r.Context(), s.db, id, time.Now()); err != nil {
		s.writeContentFailure(w, r, "delete post", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true, "id": id}})
}

// postIDFrom parses the numeric path value, rejecting malformed ids instead of
// letting them fall through as "not found".
func (s *server) postIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeFailure(w, r, http.StatusBadRequest, "invalid_id", "内容 ID 不正确")
		return 0, false
	}
	return id, true
}

// adminRouteMethods documents the owner-only endpoint methods so a mismatch is
// answered as JSON with an Allow header instead of ServeMux plain text.
var adminRouteMethods = map[string]string{
	"/api/v1/admin/posts":                       http.MethodPost,
	"/api/v1/admin/uploads":                     http.MethodPost,
	"/api/v1/admin/comments":                    http.MethodGet,
	"/api/v1/admin/settings":                    http.MethodGet + ", " + http.MethodPatch,
	"/api/v1/admin/ai/test":                     http.MethodPost,
	"/api/v1/admin/ai/author-status/regenerate": http.MethodPost,
	"/api/v1/admin/ai/assist":                   http.MethodPost,
}

// handleAdminFallback keeps every /api/v1/admin answer JSON.
func (s *server) handleAdminFallback(w http.ResponseWriter, r *http.Request) {
	if allowed, known := adminRouteMethods[r.URL.Path]; known {
		w.Header().Set("Allow", allowed)
		writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
		return
	}
	if isPostItemPath(r.URL.Path) {
		w.Header().Set("Allow", http.MethodPatch+", "+http.MethodDelete)
		writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
		return
	}
	if allow, ok := adminCommentRouteAllow(r.URL.Path); ok {
		w.Header().Set("Allow", allow)
		writeFailure(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该接口不支持此请求方法")
		return
	}
	writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
}

// adminCommentRouteAllow maps the moderation routes to the methods they accept.
func adminCommentRouteAllow(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/api/v1/admin/comments/")
	if rest == path || rest == "" {
		return "", false
	}
	switch segments := strings.Split(rest, "/"); {
	case len(segments) == 1 && segments[0] != "":
		return http.MethodDelete, true
	case len(segments) == 2 && (segments[1] == "approve" || segments[1] == "reject"):
		return http.MethodPost, true
	default:
		return "", false
	}
}

// isPostItemPath reports whether the path is /api/v1/admin/posts/{id}.
func isPostItemPath(path string) bool {
	const prefix = "/api/v1/admin/posts/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}
