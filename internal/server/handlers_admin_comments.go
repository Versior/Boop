package server

import (
	"log/slog"
	"net/http"
	"time"

	"boop/internal/settings"
	"boop/internal/social"
)

// adminQueueView drives the moderation page: the queue for one status plus the
// counts the tabs show.
type adminQueueView struct {
	pageView
	Status      string
	Comments    []commentView
	Empty       bool
	PendingTab  bool
	ApprovedTab bool
	RejectedTab bool
}

// handleAdminCommentsAPI lists the moderation queue as JSON.
func (s *server) handleAdminCommentsAPI(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOwner(w, r); !ok {
		return
	}
	limit, ok := s.pageLimitParam(w, r)
	if !ok {
		return
	}
	queue, err := social.Queue(r.Context(), s.db, social.QueueOptions{
		Status: r.URL.Query().Get("status"),
		Limit:  limit,
	})
	if err != nil {
		s.writeSocialFailure(w, r, "moderation queue", err)
		return
	}
	payload := make([]commentPayload, 0, len(queue))
	for _, comment := range queue {
		payload = append(payload, commentPayloadOf(comment))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload})
}

// handleApproveCommentAPI and handleRejectCommentAPI record the owner's
// decision. Repeating a decision is harmless and answers with the same state.
func (s *server) handleApproveCommentAPI(w http.ResponseWriter, r *http.Request) {
	s.moderateComment(w, r, true)
}

func (s *server) handleRejectCommentAPI(w http.ResponseWriter, r *http.Request) {
	s.moderateComment(w, r, false)
}

func (s *server) moderateComment(w http.ResponseWriter, r *http.Request, approve bool) {
	if _, ok := s.requireOwner(w, r); !ok {
		return
	}
	id, ok := s.commentIDFrom(w, r)
	if !ok {
		return
	}
	var (
		comment *social.Comment
		err     error
	)
	if approve {
		comment, err = social.Approve(r.Context(), s.db, id, time.Now())
	} else {
		comment, err = social.Reject(r.Context(), s.db, id, time.Now())
	}
	if err != nil {
		s.writeSocialFailure(w, r, "moderate comment", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": commentPayloadOf(*comment)})
}

// handleAdminDeleteCommentAPI removes a comment as the owner.
func (s *server) handleAdminDeleteCommentAPI(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.requireOwner(w, r)
	if !ok {
		return
	}
	id, ok := s.commentIDFrom(w, r)
	if !ok {
		return
	}
	if err := social.DeleteComment(r.Context(), s.db, id, social.Actor{ID: owner.ID, Owner: true}, time.Now()); err != nil {
		s.writeSocialFailure(w, r, "delete comment", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true, "id": id}})
}

// handleAdminCommentsPage renders the moderation queue. Guests are sent to the
// sign-in page; signed-in readers get an explicit 403 instead of an empty page.
func (s *server) handleAdminCommentsPage(w http.ResponseWriter, r *http.Request) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	if !state.user.IsOwner() {
		writeFailure(w, r, http.StatusForbidden, "forbidden", "只有站长可以管理评论")
		return
	}

	status := r.URL.Query().Get("status")
	if status == "" {
		status = social.StatusPending
	}
	queue, err := social.Queue(r.Context(), s.db, social.QueueOptions{Status: status})
	if err != nil {
		s.writeSocialFailure(w, r, "moderation page", err)
		return
	}
	values, err := settings.Load(r.Context(), s.db)
	if err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "settings unavailable, using defaults",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		values = settings.Defaults()
	}
	view := adminQueueView{
		pageView:    s.shellView(r, navNeutralFilter),
		Status:      status,
		Comments:    s.commentViewsOf(queue, loadLocation(values.SiteTimezone), socialCommentViewer{signedIn: true, owner: true, userID: state.user.ID}),
		Empty:       len(queue) == 0,
		PendingTab:  status == social.StatusPending,
		ApprovedTab: status == social.StatusApproved,
		RejectedTab: status == social.StatusRejected,
	}
	s.render(w, r, http.StatusOK, "admin_comments", view)
}
