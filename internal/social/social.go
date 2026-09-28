// Package social owns reader interaction with published content: comments with
// one level of replies, owner moderation and idempotent likes and bookmarks.
// It stores only decisions; rendering and authorization of the HTTP surface
// stay in internal/server.
package social

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"boop/internal/content"
	"boop/internal/settings"
)

// Comment statuses as stored in the comments table (docs/DATABASE.md).
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// Bounds. docs/PRODUCT.md fixes the reply depth (最大深度 2); the comment length
// range matches the comments.body CHECK constraint, and the page size matches
// the list convention in docs/API.md.
const (
	MinCommentRunes = 1
	MaxCommentRunes = 2000
	// MaxDepth counts a comment plus its replies: a reply may not be replied to.
	MaxDepth        = 2
	DefaultPageSize = 20
	MaxPageSize     = 50
)

var (
	// ErrNotFound is returned for unknown, deleted or invisible targets.
	ErrNotFound = errors.New("social: target not found")
	// ErrForbidden is returned when an account may not touch someone else's row.
	ErrForbidden = errors.New("social: action not allowed")
	// ErrCommentsDisabled is returned when comments.enabled is false.
	ErrCommentsDisabled = errors.New("social: comments are disabled")
)

// ValidationError carries the machine code and the user-facing message of a
// rejected write, so the HTTP layer stays a thin translation.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("social: %s: %s", e.Code, e.Message)
}

func invalid(code, message string) *ValidationError {
	return &ValidationError{Code: code, Message: message}
}

// Actor is the signed-in account performing a comment action.
type Actor struct {
	ID    int64
	Owner bool
}

// Author is the public identity attached to a comment.
type Author struct {
	ID          int64
	DisplayName string
	AvatarURL   string
}

// Comment is one comment with its author resolved. Replies stays empty unless
// the value came from Comments.
type Comment struct {
	ID        int64
	PostID    int64
	ParentID  *int64
	UserID    int64
	Body      string
	Status    string
	CreatedAt string
	UpdatedAt string
	Author    Author
	// Mine reports whether the viewer wrote this comment.
	Mine bool
	// PostTitle and PostSlug describe the commented content; the moderation
	// queue fills them so the owner can jump to the post from the admin page.
	PostTitle string
	PostSlug  string
	Replies   []Comment
}

// CommentInput is a new comment or reply.
type CommentInput struct {
	PostID   int64
	Actor    Actor
	Body     string
	ParentID *int64
}

// CreateComment stores a comment on a published post. The two comment switches
// are read inside the same transaction that inserts the row, so the state that
// decides the comment's status cannot change between the decision and the write;
// a settings value that does not decode fails the call and rolls the transaction
// back. The post visibility and the reply invariants are checked there too, so a
// rejected comment leaves nothing behind.
func CreateComment(ctx context.Context, db *sql.DB, in CommentInput, now time.Time) (*Comment, error) {
	if db == nil {
		return nil, errors.New("social: create comment: nil database")
	}
	if in.Actor.ID <= 0 {
		return nil, errors.New("social: create comment: actor is required")
	}
	if in.PostID <= 0 {
		return nil, errors.New("social: create comment: post id is required")
	}

	body := strings.TrimSpace(in.Body)
	if runes := utf8.RuneCountInString(body); runes < MinCommentRunes || runes > MaxCommentRunes {
		return nil, invalid("invalid_body", fmt.Sprintf("评论内容需为 %d 到 %d 个字", MinCommentRunes, MaxCommentRunes))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("social: comment begin: %w", err)
	}
	defer tx.Rollback()

	// The switches belong to this transaction: a caller cannot pass a decision it
	// read earlier, and a concurrent settings change cannot land between the read
	// and the INSERT.
	values, err := settings.LoadTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("social: comment settings: %w", err)
	}
	if !values.CommentsEnabled {
		return nil, ErrCommentsDisabled
	}

	// The owner's own comments are always visible; readers wait for moderation
	// when it is enabled (docs/PRODUCT.md §5.3).
	status := StatusApproved
	if !in.Actor.Owner && values.CommentsModerationEnabled {
		status = StatusPending
	}

	var exists int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM posts WHERE id = ? AND status = 'published' AND deleted_at IS NULL`,
		in.PostID).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("social: comment post check: %w", err)
	}
	if exists == 0 {
		// Comments on drafts, archived and deleted posts are invisible work, so
		// they are refused exactly like a missing post.
		return nil, ErrNotFound
	}

	if in.ParentID != nil {
		if err := checkParent(ctx, tx, in.PostID, *in.ParentID); err != nil {
			return nil, err
		}
	}

	stamp := timestamp(now)
	result, err := tx.ExecContext(ctx,
		`INSERT INTO comments(post_id, user_id, parent_id, body, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		in.PostID, in.Actor.ID, in.ParentID, body, status, stamp, stamp)
	if err != nil {
		return nil, fmt.Errorf("social: insert comment: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("social: comment id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("social: comment commit: %w", err)
	}
	return commentByID(ctx, db, id, in.Actor.ID)
}

// checkParent enforces the documented reply rules inside the transaction: the
// parent must belong to the same post, must itself be a top-level comment, and
// must be publicly visible.
func checkParent(ctx context.Context, tx *sql.Tx, postID, parentID int64) error {
	var parentPost int64
	var grandParent sql.NullInt64
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT post_id, parent_id, status FROM comments WHERE id = ? AND deleted_at IS NULL`,
		parentID).Scan(&parentPost, &grandParent, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("invalid_parent", "父评论不存在")
	}
	if err != nil {
		return fmt.Errorf("social: parent lookup: %w", err)
	}
	if parentPost != postID {
		return invalid("invalid_parent", "父评论不属于这篇内容")
	}
	if grandParent.Valid {
		return invalid("invalid_parent", "只能回复一级评论")
	}
	if status != StatusApproved {
		// A reply under a hidden parent would be unreachable and confusing.
		return invalid("invalid_parent", "父评论不可回复")
	}
	return nil
}

// Comments returns the public thread of a post: approved, undeleted comments in
// creation order, each root carrying its replies. A reply whose parent is not
// public is dropped, so deleting or rejecting a parent never exposes its
// children.
func Comments(ctx context.Context, db *sql.DB, postID, viewerID int64) ([]Comment, error) {
	if db == nil {
		return nil, errors.New("social: comments: nil database")
	}
	if postID <= 0 {
		return nil, errors.New("social: comments: post id is required")
	}
	rows, err := db.QueryContext(ctx, `SELECT `+commentColumns+`
		 FROM comments c JOIN users u ON u.id = c.user_id
		 WHERE c.post_id = ? AND c.status = ? AND c.deleted_at IS NULL
		 ORDER BY c.created_at, c.id`, postID, StatusApproved)
	if err != nil {
		return nil, fmt.Errorf("social: comments query: %w", err)
	}
	defer rows.Close()

	roots := make([]Comment, 0, 8)
	replies := make(map[int64][]Comment)
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		comment.Mine = viewerID > 0 && comment.UserID == viewerID
		if comment.ParentID == nil {
			roots = append(roots, *comment)
			continue
		}
		replies[*comment.ParentID] = append(replies[*comment.ParentID], *comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("social: comment rows: %w", err)
	}

	// Attach replies only to roots that are in this page; anything else stays
	// out of the public thread.
	visible := make(map[int64]int, len(roots))
	for index := range roots {
		visible[roots[index].ID] = index
	}
	for parentID, children := range replies {
		index, ok := visible[parentID]
		if !ok {
			continue
		}
		roots[index].Replies = children
	}
	return roots, nil
}

// DeleteComment soft deletes a comment. Only its author or the owner may do it,
// and deleting an already deleted comment is reported as not found so the
// action is explicit rather than silently repeated.
func DeleteComment(ctx context.Context, db *sql.DB, id int64, actor Actor, now time.Time) error {
	if db == nil {
		return errors.New("social: delete comment: nil database")
	}
	if id <= 0 {
		return errors.New("social: delete comment: comment id is required")
	}
	var authorID int64
	err := db.QueryRowContext(ctx,
		`SELECT user_id FROM comments WHERE id = ? AND deleted_at IS NULL`, id).Scan(&authorID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("social: delete comment lookup: %w", err)
	}
	if authorID != actor.ID && !actor.Owner {
		return ErrForbidden
	}

	stamp := timestamp(now)
	result, err := db.ExecContext(ctx,
		`UPDATE comments SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		stamp, stamp, id)
	if err != nil {
		return fmt.Errorf("social: delete comment: %w", err)
	}
	return affectedOrNotFound(result, "social: delete comment")
}

// Approve and Reject are the owner's moderation decisions. Repeating a decision
// is harmless and answers with the same final state.
func Approve(ctx context.Context, db *sql.DB, id int64, now time.Time) (*Comment, error) {
	return setStatus(ctx, db, id, StatusApproved, now)
}

func Reject(ctx context.Context, db *sql.DB, id int64, now time.Time) (*Comment, error) {
	return setStatus(ctx, db, id, StatusRejected, now)
}

func setStatus(ctx context.Context, db *sql.DB, id int64, status string, now time.Time) (*Comment, error) {
	if db == nil {
		return nil, errors.New("social: moderate comment: nil database")
	}
	if id <= 0 {
		return nil, errors.New("social: moderate comment: comment id is required")
	}
	result, err := db.ExecContext(ctx,
		`UPDATE comments SET status = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		status, timestamp(now), id)
	if err != nil {
		return nil, fmt.Errorf("social: set comment status: %w", err)
	}
	if err := affectedOrNotFound(result, "social: moderate comment"); err != nil {
		return nil, err
	}
	return commentByID(ctx, db, id, 0)
}

// QueueOptions is the owner's moderation queue query.
type QueueOptions struct {
	Status string
	Limit  int
}

// Queue lists comments of one status, newest first, for the owner's moderation
// page. An empty status means the pending queue.
func Queue(ctx context.Context, db *sql.DB, opts QueueOptions) ([]Comment, error) {
	if db == nil {
		return nil, errors.New("social: queue: nil database")
	}
	status := opts.Status
	if status == "" {
		status = StatusPending
	}
	switch status {
	case StatusPending, StatusApproved, StatusRejected:
	default:
		return nil, invalid("invalid_status", "评论状态不存在")
	}
	limit, err := pageLimit(opts.Limit)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `SELECT `+commentColumns+`, p.title, p.slug
		 FROM comments c
		 JOIN users u ON u.id = c.user_id
		 JOIN posts p ON p.id = c.post_id
		 WHERE c.status = ? AND c.deleted_at IS NULL
		 ORDER BY c.created_at DESC, c.id DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("social: queue query: %w", err)
	}
	defer rows.Close()

	queue := make([]Comment, 0, limit)
	for rows.Next() {
		comment, err := scanCommentWithPost(rows)
		if err != nil {
			return nil, err
		}
		queue = append(queue, *comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("social: queue rows: %w", err)
	}
	return queue, nil
}

// Reaction is the final state of a like or bookmark after a write, so the
// caller never has to guess whether the row existed.
type Reaction struct {
	On    bool
	Count int
}

// SetLike is an idempotent like switch on a published post. on=false removes the
// row; the answer always describes the state that is now stored.
func SetLike(ctx context.Context, db *sql.DB, userID, postID int64, on bool, now time.Time) (Reaction, error) {
	return setReaction(ctx, db, userID, postID, on, now, reactionTarget{
		table:     "likes",
		countSQL:  `SELECT (SELECT COUNT(*) FROM likes WHERE post_id = ?), EXISTS(SELECT 1 FROM likes WHERE user_id = ? AND post_id = ?)`,
		countArgs: func(userID, postID int64) []any { return []any{postID, userID, postID} },
	})
}

// SetBookmark is an idempotent bookmark switch. Boop keeps bookmarks private, so
// the returned count is the viewer's own total instead of a public number.
func SetBookmark(ctx context.Context, db *sql.DB, userID, postID int64, on bool, now time.Time) (Reaction, error) {
	return setReaction(ctx, db, userID, postID, on, now, reactionTarget{
		table:     "bookmarks",
		countSQL:  `SELECT (SELECT COUNT(*) FROM bookmarks WHERE user_id = ?), EXISTS(SELECT 1 FROM bookmarks WHERE user_id = ? AND post_id = ?)`,
		countArgs: func(userID, postID int64) []any { return []any{userID, userID, postID} },
	})
}

type reactionTarget struct {
	table     string
	countSQL  string
	countArgs func(userID, postID int64) []any
}

func setReaction(ctx context.Context, db *sql.DB, userID, postID int64, on bool, now time.Time, target reactionTarget) (Reaction, error) {
	if db == nil {
		return Reaction{}, errors.New("social: reaction: nil database")
	}
	if userID <= 0 {
		return Reaction{}, errors.New("social: reaction: user id is required")
	}
	if postID <= 0 {
		return Reaction{}, errors.New("social: reaction: post id is required")
	}
	// Reactions only exist on publicly readable content, so unpublishing or
	// deleting a post stops new likes and bookmarks on it.
	var visible int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM posts WHERE id = ? AND status = 'published' AND deleted_at IS NULL`,
		postID).Scan(&visible); err != nil {
		return Reaction{}, fmt.Errorf("social: reaction post check: %w", err)
	}
	if visible == 0 {
		return Reaction{}, ErrNotFound
	}

	if on {
		// ON CONFLICT DO NOTHING makes a repeated like a no-op instead of an error.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO `+target.table+`(user_id, post_id, created_at) VALUES(?,?,?)
			 ON CONFLICT(user_id, post_id) DO NOTHING`,
			userID, postID, timestamp(now)); err != nil {
			return Reaction{}, fmt.Errorf("social: insert %s: %w", target.table, err)
		}
	} else {
		if _, err := db.ExecContext(ctx,
			`DELETE FROM `+target.table+` WHERE user_id = ? AND post_id = ?`,
			userID, postID); err != nil {
			return Reaction{}, fmt.Errorf("social: delete %s: %w", target.table, err)
		}
	}

	// Read the state back instead of trusting the write, so the answer is the
	// stored truth even if another request got there first.
	var reaction Reaction
	if err := db.QueryRowContext(ctx, target.countSQL, target.countArgs(userID, postID)...).
		Scan(&reaction.Count, &reaction.On); err != nil {
		return Reaction{}, fmt.Errorf("social: read %s state: %w", target.table, err)
	}
	return reaction, nil
}

// ViewerState is what one account did to one post.
type ViewerState struct {
	Liked      bool
	Bookmarked bool
}

// ViewerStates resolves the signed-in reader's likes and bookmarks for a whole
// page with one statement, so the feed never grows a query per card. Guests and
// empty pages cost nothing.
func ViewerStates(ctx context.Context, db *sql.DB, userID int64, postIDs []int64) (map[int64]ViewerState, error) {
	states := make(map[int64]ViewerState, len(postIDs))
	if db == nil {
		return nil, errors.New("social: viewer states: nil database")
	}
	if userID <= 0 || len(postIDs) == 0 {
		return states, nil
	}
	placeholders := make([]string, len(postIDs))
	args := make([]any, 0, len(postIDs)+2)
	args = append(args, userID, userID)
	for i, id := range postIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT p.id,
			EXISTS(SELECT 1 FROM likes l WHERE l.user_id = ? AND l.post_id = p.id),
			EXISTS(SELECT 1 FROM bookmarks b WHERE b.user_id = ? AND b.post_id = p.id)
		 FROM posts p WHERE p.id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("social: viewer states query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var state ViewerState
		if err := rows.Scan(&id, &state.Liked, &state.Bookmarked); err != nil {
			return nil, fmt.Errorf("social: scan viewer state: %w", err)
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("social: viewer state rows: %w", err)
	}
	return states, nil
}

// BookmarkPage is one page of an account's bookmarks plus the cursor that
// continues it.
type BookmarkPage struct {
	PostIDs    []int64
	NextCursor string
}

// Bookmarks lists the ids of posts an account bookmarked, newest bookmark
// first. The cursor is the documented <created_at>,<post_id> pair, so a page
// stays stable while new bookmarks arrive. Posts that are no longer published
// are skipped: a bookmark never resurrects hidden content.
func Bookmarks(ctx context.Context, db *sql.DB, userID int64, cursor string, limit int) (*BookmarkPage, error) {
	if db == nil {
		return nil, errors.New("social: bookmarks: nil database")
	}
	if userID <= 0 {
		return nil, errors.New("social: bookmarks: user id is required")
	}
	pageSize, err := pageLimit(limit)
	if err != nil {
		return nil, err
	}
	cursorTime, cursorID, err := parseCursor(cursor)
	if err != nil {
		return nil, err
	}

	query := `SELECT b.post_id, b.created_at FROM bookmarks b JOIN posts p ON p.id = b.post_id
		WHERE b.user_id = ? AND p.status = 'published' AND p.deleted_at IS NULL`
	args := []any{userID}
	if cursorTime != "" {
		query += ` AND (b.created_at < ? OR (b.created_at = ? AND b.post_id < ?))`
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query += ` ORDER BY b.created_at DESC, b.post_id DESC LIMIT ?`
	args = append(args, pageSize+1)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("social: bookmark query: %w", err)
	}
	defer rows.Close()

	type row struct {
		postID int64
		stamp  string
	}
	page := make([]row, 0, pageSize)
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.postID, &item.stamp); err != nil {
			return nil, fmt.Errorf("social: scan bookmark: %w", err)
		}
		page = append(page, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("social: bookmark rows: %w", err)
	}

	result := &BookmarkPage{}
	if len(page) > pageSize {
		page = page[:pageSize]
		last := page[len(page)-1]
		result.NextCursor = content.EncodeCursor(last.stamp, last.postID)
	}
	result.PostIDs = make([]int64, 0, len(page))
	for _, item := range page {
		result.PostIDs = append(result.PostIDs, item.postID)
	}
	return result, nil
}

// parseCursor decodes the documented list cursor, treating an empty value as
// the first page. The cursor format itself is shared with the post feed, but the
// failure is reported as this package's own validation error so callers map one
// error type per package.
func parseCursor(cursor string) (string, int64, error) {
	if cursor == "" {
		return "", 0, nil
	}
	at, id, err := content.DecodeCursor(cursor)
	if err != nil {
		return "", 0, invalid("invalid_cursor", "游标格式不正确")
	}
	return at, id, nil
}

// commentColumns is the shared projection: the row plus its author's public
// identity, resolved in the same statement.
const commentColumns = `c.id, c.post_id, c.parent_id, c.user_id, c.body, c.status, c.created_at, c.updated_at,
	u.display_name, COALESCE(u.avatar_url, '')`

func commentByID(ctx context.Context, db *sql.DB, id, viewerID int64) (*Comment, error) {
	row := db.QueryRowContext(ctx, `SELECT `+commentColumns+`, p.title, p.slug
		 FROM comments c
		 JOIN users u ON u.id = c.user_id
		 JOIN posts p ON p.id = c.post_id
		 WHERE c.id = ?`, id)
	comment, err := scanCommentWithPost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	comment.Mine = viewerID > 0 && comment.UserID == viewerID
	return comment, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanComment(row scanner) (*Comment, error) {
	var comment Comment
	var parent sql.NullInt64
	err := row.Scan(&comment.ID, &comment.PostID, &parent, &comment.UserID, &comment.Body, &comment.Status,
		&comment.CreatedAt, &comment.UpdatedAt, &comment.Author.DisplayName, &comment.Author.AvatarURL)
	if err != nil {
		return nil, fmt.Errorf("social: scan comment: %w", err)
	}
	if parent.Valid {
		comment.ParentID = &parent.Int64
	}
	comment.Author.ID = comment.UserID
	return &comment, nil
}

func scanCommentWithPost(row scanner) (*Comment, error) {
	var comment Comment
	var parent sql.NullInt64
	err := row.Scan(&comment.ID, &comment.PostID, &parent, &comment.UserID, &comment.Body, &comment.Status,
		&comment.CreatedAt, &comment.UpdatedAt, &comment.Author.DisplayName, &comment.Author.AvatarURL,
		&comment.PostTitle, &comment.PostSlug)
	if err != nil {
		return nil, fmt.Errorf("social: scan comment: %w", err)
	}
	if parent.Valid {
		comment.ParentID = &parent.Int64
	}
	comment.Author.ID = comment.UserID
	return &comment, nil
}

func pageLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return DefaultPageSize, nil
	case limit < 0 || limit > MaxPageSize:
		return 0, invalid("invalid_limit", fmt.Sprintf("每页数量需在 1 到 %d 之间", MaxPageSize))
	default:
		return limit, nil
	}
}

func affectedOrNotFound(result sql.Result, operation string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// timestamp renders UTC RFC3339Nano. social deliberately does not import
// content's timestamp helper for writes; content.TimestampFormat keeps the two
// in step, and the extra precision keeps updated_at distinct inside one tick.
func timestamp(at time.Time) string {
	return at.UTC().Format(content.TimestampFormat)
}
