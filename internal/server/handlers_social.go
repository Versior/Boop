package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"boop/internal/content"
	"boop/internal/settings"
	"boop/internal/social"
)

const (
	// socialJSONBytes caps comment writes independently from the upload budget:
	// a comment is at most 2000 characters, so 64KiB is already generous.
	socialJSONBytes = 64 << 10

	// loginPath is where a guest is sent when a write needs an account.
	loginPath = "/login"
)

// authorPayload is the public identity of a comment author. Email and role are
// deliberately absent: another reader must not learn them.
type authorPayload struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
}

type commentPayload struct {
	ID        int64            `json:"id"`
	PostID    int64            `json:"post_id"`
	ParentID  *int64           `json:"parent_id"`
	Body      string           `json:"body"`
	Status    string           `json:"status"`
	CreatedAt string           `json:"created_at"`
	Author    authorPayload    `json:"author"`
	Mine      bool             `json:"mine"`
	PostTitle string           `json:"post_title,omitempty"`
	PostSlug  string           `json:"post_slug,omitempty"`
	Replies   []commentPayload `json:"replies"`
}

func commentPayloadOf(comment social.Comment) commentPayload {
	replies := make([]commentPayload, 0, len(comment.Replies))
	for _, reply := range comment.Replies {
		replies = append(replies, commentPayloadOf(reply))
	}
	return commentPayload{
		ID: comment.ID, PostID: comment.PostID, ParentID: comment.ParentID, Body: comment.Body,
		Status: comment.Status, CreatedAt: comment.CreatedAt,
		Author: authorPayload{
			ID:          comment.Author.ID,
			DisplayName: comment.Author.DisplayName,
			AvatarURL:   comment.Author.AvatarURL,
		},
		Mine:      comment.Mine,
		PostTitle: comment.PostTitle,
		PostSlug:  comment.PostSlug,
		Replies:   replies,
	}
}

// commentView is the server-rendered comment. The body is plain text, so the
// template escapes it and CSS keeps the line breaks.
type commentView struct {
	ID        int64
	Author    string
	Avatar    string
	Body      string
	Datetime  string
	TimeLabel string
	Status    string
	Mine      bool
	// PostTitle and PostSlug place a queue item next to its content; the public
	// thread leaves them empty.
	PostTitle string
	PostSlug  string
	// CanDelete and CanReply are resolved per viewer so the template stays free
	// of authorization logic.
	CanDelete bool
	CanReply  bool
	Replies   []commentView
}

// commentViewsOf builds the SSR thread. onlyRoots can reply: Boop allows exactly
// one level (docs/PRODUCT.md §5.3).
func (s *server) commentViewsOf(comments []social.Comment, location *time.Location, viewer socialCommentViewer) []commentView {
	views := make([]commentView, 0, len(comments))
	for _, comment := range comments {
		view := commentView{
			ID:        comment.ID,
			Author:    comment.Author.DisplayName,
			Avatar:    comment.Author.AvatarURL,
			Body:      comment.Body,
			Status:    comment.Status,
			Mine:      comment.Mine,
			PostTitle: comment.PostTitle,
			PostSlug:  comment.PostSlug,
			CanDelete: comment.Mine || viewer.owner,
			CanReply:  viewer.signedIn,
		}
		view.Datetime, view.TimeLabel = displayTime(comment.CreatedAt, location)
		for _, reply := range comment.Replies {
			replyView := commentView{
				ID:        reply.ID,
				Author:    reply.Author.DisplayName,
				Avatar:    reply.Author.AvatarURL,
				Body:      reply.Body,
				Status:    reply.Status,
				Mine:      reply.Mine,
				CanDelete: reply.Mine || viewer.owner,
				// No second level, so a reply never offers a reply button.
				CanReply: false,
			}
			replyView.Datetime, replyView.TimeLabel = displayTime(reply.CreatedAt, location)
			view.Replies = append(view.Replies, replyView)
		}
		views = append(views, view)
	}
	return views
}

// socialCommentViewer is who is reading the thread.
type socialCommentViewer struct {
	signedIn bool
	owner    bool
	userID   int64
}

func (s *server) socialViewer(r *http.Request) socialCommentViewer {
	state, _ := authStateFrom(r.Context())
	if !state.authenticated {
		return socialCommentViewer{}
	}
	return socialCommentViewer{signedIn: true, owner: state.user.IsOwner(), userID: state.user.ID}
}

// viewerStates resolves the signed-in reader's likes and bookmarks for a page of
// posts in one query. Guests and empty pages cost nothing, and a failure only
// costs the button states: the content itself still renders.
func (s *server) viewerStates(ctx context.Context, r *http.Request, ids []int64) map[int64]social.ViewerState {
	state, _ := authStateFrom(r.Context())
	if !state.authenticated || len(ids) == 0 {
		return nil
	}
	states, err := social.ViewerStates(ctx, s.db, state.user.ID, ids)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "viewer reaction states unavailable",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		return nil
	}
	return states
}

func postIDs(posts []content.Post) []int64 {
	ids := make([]int64, 0, len(posts))
	for _, post := range posts {
		ids = append(ids, post.ID)
	}
	return ids
}

// applyViewerState copies the resolved reaction state onto a payload.
func applyViewerState(payload *postPayload, states map[int64]social.ViewerState) {
	if state, ok := states[payload.ID]; ok {
		payload.Liked = state.Liked
		payload.Bookmarked = state.Bookmarked
	}
}

// ---------- public comments ----------

// handleCommentsAPI lists the public thread of one published post.
func (s *server) handleCommentsAPI(w http.ResponseWriter, r *http.Request) {
	post, err := content.BySlug(r.Context(), s.db, r.PathValue("slug"))
	if err != nil {
		s.writeContentFailure(w, r, "comments api", err)
		return
	}
	viewer := s.socialViewer(r)
	comments, err := social.Comments(r.Context(), s.db, post.ID, viewer.userID)
	if err != nil {
		s.writeSocialFailure(w, r, "comments api", err)
		return
	}
	payload := make([]commentPayload, 0, len(comments))
	for _, comment := range comments {
		payload = append(payload, commentPayloadOf(comment))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload})
}

// handleCreateCommentAPI stores a comment or reply for a signed-in account.
func (s *server) handleCreateCommentAPI(w http.ResponseWriter, r *http.Request) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		writeFailure(w, r, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	postID, ok := s.postIDFrom(w, r)
	if !ok {
		return
	}
	if !s.guardRateLimit(w, r, s.limiters.comment, "comment", userKey(state.user.ID)) {
		return
	}
	if !s.guardRateLimit(w, r, s.limiters.commentIP, "comment", ipKey(r.RemoteAddr)) {
		return
	}

	var body createCommentRequest
	if !s.readJSON(w, r, &body, socialJSONBytes) {
		return
	}
	// The comment switches are read inside social's transaction, so nothing here
	// has to guess whether comments are open.
	comment, err := social.CreateComment(r.Context(), s.db, social.CommentInput{
		PostID:   postID,
		Actor:    social.Actor{ID: state.user.ID, Owner: state.user.IsOwner()},
		Body:     body.Body,
		ParentID: body.ParentID,
	}, time.Now())
	if err != nil {
		s.writeSocialFailure(w, r, "create comment", err)
		return
	}
	payload := commentPayloadOf(*comment)
	payload.Mine = true
	writeJSON(w, http.StatusCreated, map[string]any{"data": payload})
}

type createCommentRequest struct {
	Body     string `json:"body"`
	ParentID *int64 `json:"parent_id"`
}

// handleDeleteCommentAPI soft deletes a comment for its author or the owner.
func (s *server) handleDeleteCommentAPI(w http.ResponseWriter, r *http.Request) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		writeFailure(w, r, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	id, ok := s.commentIDFrom(w, r)
	if !ok {
		return
	}
	actor := social.Actor{ID: state.user.ID, Owner: state.user.IsOwner()}
	if err := social.DeleteComment(r.Context(), s.db, id, actor, time.Now()); err != nil {
		s.writeSocialFailure(w, r, "delete comment", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true, "id": id}})
}

// ---------- likes and bookmarks ----------

// handleLikeAPI / handleBookmarkAPI are built from the method: PUT sets the
// reaction, DELETE clears it, and both answer with the stored final state.
func (s *server) handleLikeAPI(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.handleReaction(w, r, on, reactionKindLike)
	}
}

func (s *server) handleBookmarkAPI(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.handleReaction(w, r, on, reactionKindBookmark)
	}
}

type reactionKind int

const (
	reactionKindLike reactionKind = iota
	reactionKindBookmark
)

func (s *server) handleReaction(w http.ResponseWriter, r *http.Request, on bool, kind reactionKind) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		writeFailure(w, r, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	postID, ok := s.postIDFrom(w, r)
	if !ok {
		return
	}

	var (
		reaction social.Reaction
		err      error
	)
	switch kind {
	case reactionKindLike:
		reaction, err = social.SetLike(r.Context(), s.db, state.user.ID, postID, on, time.Now())
	case reactionKindBookmark:
		reaction, err = social.SetBookmark(r.Context(), s.db, state.user.ID, postID, on, time.Now())
	}
	if err != nil {
		s.writeSocialFailure(w, r, "reaction", err)
		return
	}

	if kind == reactionKindLike {
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"post_id": postID, "liked": reaction.On, "like_count": reaction.Count,
		}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"post_id": postID, "bookmarked": reaction.On, "bookmark_count": reaction.Count,
	}})
}

// ---------- the reader's bookmarks ----------

type bookmarkPageView struct {
	pageView
	Posts       []postCard
	NextCursor  string
	LoadMoreURL string
	Empty       bool
	CanReact    bool
}

// handleBookmarksAPI lists the signed-in reader's bookmarks as JSON.
func (s *server) handleBookmarksAPI(w http.ResponseWriter, r *http.Request) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		writeFailure(w, r, http.StatusUnauthorized, "unauthorized", "请先登录")
		return
	}
	limit, ok := s.pageLimitParam(w, r)
	if !ok {
		return
	}
	page, err := social.Bookmarks(r.Context(), s.db, state.user.ID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.writeSocialFailure(w, r, "bookmarks api", err)
		return
	}
	posts, err := content.ByIDs(r.Context(), s.db, page.PostIDs)
	if err != nil {
		s.writeContentFailure(w, r, "bookmarks api", err)
		return
	}
	states := s.viewerStates(r.Context(), r, postIDs(posts))
	payload := make([]postPayload, 0, len(posts))
	for _, post := range posts {
		item := postPayloadOf(post)
		applyViewerState(&item, states)
		payload = append(payload, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload, "next_cursor": page.NextCursor})
}

// handleBookmarksPage renders the reader's private bookmark list. Guests are
// sent to the sign-in page instead of an empty list.
func (s *server) handleBookmarksPage(w http.ResponseWriter, r *http.Request) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "settings unavailable, using defaults",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		values = settings.Defaults()
	}
	page, err := social.Bookmarks(r.Context(), s.db, state.user.ID, r.URL.Query().Get("cursor"), values.PageSize)
	if err != nil {
		s.writeSocialFailure(w, r, "bookmarks page", err)
		return
	}
	posts, err := content.ByIDs(r.Context(), s.db, page.PostIDs)
	if err != nil {
		s.writeContentFailure(w, r, "bookmarks page", err)
		return
	}
	states := s.viewerStates(r.Context(), r, postIDs(posts))
	location := loadLocation(values.SiteTimezone)
	ownerName, _ := s.ownerIdentity(r.Context())
	view := bookmarkPageView{
		pageView: s.shellView(r, navBookmarksFilter),
		Empty:    len(posts) == 0,
		CanReact: true,
		Posts:    make([]postCard, 0, len(posts)),
	}
	for _, post := range posts {
		card := s.cardOf(post, location)
		card.CanReact = true
		card.OwnerName = ownerName
		if state, ok := states[post.ID]; ok {
			card.Liked = state.Liked
			card.Bookmarked = state.Bookmarked
		}
		view.Posts = append(view.Posts, card)
	}
	if page.NextCursor != "" {
		view.NextCursor = page.NextCursor
		view.LoadMoreURL = "/bookmarks?cursor=" + urlQueryEscape(page.NextCursor)
	}
	s.render(w, r, http.StatusOK, "bookmarks", view)
}

// pageLimitParam parses the optional ?limit= of a list endpoint.
func (s *server) pageLimitParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		writeFailure(w, r, http.StatusBadRequest, "invalid_limit", "每页数量需在 1 到 50 之间")
		return 0, false
	}
	return limit, true
}

// ---------- shared failure mapping ----------

// writeSocialFailure maps a social error onto the documented envelope.
func (s *server) writeSocialFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var invalid *social.ValidationError
	switch {
	case errors.As(err, &invalid):
		writeFailure(w, r, http.StatusBadRequest, invalid.Code, invalid.Message)
	case errors.Is(err, social.ErrCommentsDisabled):
		writeFailure(w, r, http.StatusForbidden, "comments_disabled", "站点已关闭评论")
	case errors.Is(err, social.ErrForbidden):
		writeFailure(w, r, http.StatusForbidden, "forbidden", "只能删除自己的评论")
	case errors.Is(err, social.ErrNotFound):
		writeFailure(w, r, http.StatusNotFound, "not_found", "请求的资源不存在")
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, operation+" failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	}
}

// commentIDFrom parses the numeric comment id from the path.
func (s *server) commentIDFrom(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeFailure(w, r, http.StatusBadRequest, "invalid_id", "评论 ID 不正确")
		return 0, false
	}
	return id, true
}

// urlQueryEscape escapes a value for use inside a same-origin link.
func urlQueryEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
