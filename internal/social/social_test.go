package social

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boop/internal/store"
)

// ---------- fixtures ----------

var testNow = time.Date(2026, 9, 28, 6, 2, 3, 0, time.UTC)

type fixture struct {
	db       *sql.DB
	ownerID  int64
	readerID int64
	otherID  int64
	postID   int64
	draftID  int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	f := &fixture{db: db}
	stamp := testNow.Format(time.RFC3339)
	f.ownerID = insertUser(t, db, "owner@example.com", "owner", "遇事开心", stamp)
	f.readerID = insertUser(t, db, "reader@example.com", "reader", "读者甲", stamp)
	f.otherID = insertUser(t, db, "other@example.com", "reader", "读者乙", stamp)
	f.postID = insertPost(t, db, "海边的下午", "published", stamp)
	f.draftID = insertPost(t, db, "还没写完的草稿", "draft", stamp)
	return f
}

func insertUser(t *testing.T, db *sql.DB, email, role, name, stamp string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO users(email, display_name, role, created_at, updated_at) VALUES(?,?,?,?,?)`,
		email, name, role, stamp, stamp)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	return id
}

// postSeq keeps fixture slugs unique without the caller having to invent one.
var postSeq int64

func insertPost(t *testing.T, db *sql.DB, title, status, stamp string) int64 {
	t.Helper()
	postSeq++
	slug := fmt.Sprintf("post-%d", postSeq)
	var published any
	if status == "published" {
		published = stamp
	}
	res, err := db.Exec(`INSERT INTO posts(slug, type, status, title, body_markdown, body_html, excerpt,
		location, created_at, updated_at, published_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		slug, "article", status, title, "正文", "<p>正文</p>", "", "", stamp, stamp, published)
	if err != nil {
		t.Fatalf("insert post: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("post id: %v", err)
	}
	return id
}

func (f *fixture) commentsEnabled() CommentOptions {
	return CommentOptions{Enabled: true, Moderation: false}
}

func (f *fixture) comment(t *testing.T, actor Actor, body string, parent *int64, opts CommentOptions) *Comment {
	t.Helper()
	comment, err := CreateComment(context.Background(), f.db, CommentInput{
		PostID: f.postID, Actor: actor, Body: body, ParentID: parent,
	}, opts, testNow)
	if err != nil {
		t.Fatalf("CreateComment(%q): %v", body, err)
	}
	return comment
}

func readerActor(id int64) Actor { return Actor{ID: id} }
func ownerActor(id int64) Actor  { return Actor{ID: id, Owner: true} }

// insertComment writes a comment row directly, so tests can build states the
// public API cannot produce (rejected, deleted, orphaned replies).
func insertComment(t *testing.T, db *sql.DB, postID, userID int64, parent *int64, body, status string, deleted bool) int64 {
	t.Helper()
	stamp := testNow.Format(time.RFC3339)
	var deletedAt any
	if deleted {
		deletedAt = stamp
	}
	res, err := db.Exec(`INSERT INTO comments(post_id, user_id, parent_id, body, status, created_at, updated_at, deleted_at)
		VALUES(?,?,?,?,?,?,?,?)`, postID, userID, parent, body, status, stamp, stamp, deletedAt)
	if err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("comment id: %v", err)
	}
	return id
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want a validation error with code %q", err, code)
	}
	if invalid.Code != code {
		t.Fatalf("code = %q, want %q", invalid.Code, code)
	}
}

// ---------- creating comments ----------

func TestCreateCommentValidation(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty", "", "invalid_body"},
		{"blank", "   \n\t ", "invalid_body"},
		{"too long", strings.Repeat("字", MaxCommentRunes+1), "invalid_body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateComment(context.Background(), f.db, CommentInput{
				PostID: f.postID, Actor: readerActor(f.readerID), Body: tc.body,
			}, f.commentsEnabled(), testNow)
			assertCode(t, err, tc.code)
		})
	}

	t.Run("exactly the limit", func(t *testing.T) {
		comment := f.comment(t, readerActor(f.readerID), strings.Repeat("字", MaxCommentRunes), nil, f.commentsEnabled())
		if len([]rune(comment.Body)) != MaxCommentRunes {
			t.Errorf("body runes = %d, want %d", len([]rune(comment.Body)), MaxCommentRunes)
		}
	})

	t.Run("trimmed and approved without moderation", func(t *testing.T) {
		comment := f.comment(t, readerActor(f.readerID), "  有前后空格  ", nil, f.commentsEnabled())
		if comment.Body != "有前后空格" {
			t.Errorf("body = %q, want it trimmed", comment.Body)
		}
		if comment.Status != StatusApproved {
			t.Errorf("status = %q, want approved", comment.Status)
		}
		if comment.Author.DisplayName != "读者甲" {
			t.Errorf("author = %q", comment.Author.DisplayName)
		}
	})

	t.Run("missing actor", func(t *testing.T) {
		_, err := CreateComment(context.Background(), f.db, CommentInput{PostID: f.postID, Body: "x"},
			f.commentsEnabled(), testNow)
		if err == nil {
			t.Fatal("CreateComment accepted a comment without an author")
		}
	})

	t.Run("nil database", func(t *testing.T) {
		_, err := CreateComment(context.Background(), nil, CommentInput{PostID: f.postID, Actor: readerActor(1), Body: "x"},
			f.commentsEnabled(), testNow)
		if err == nil {
			t.Fatal("CreateComment accepted a nil database")
		}
	})

	if posts := countRows(t, f.db, "posts"); posts != 2 {
		t.Errorf("posts = %d, want the fixture's two", posts)
	}
}

func TestCreateCommentHonoursSettings(t *testing.T) {
	f := newFixture(t)

	t.Run("comments disabled", func(t *testing.T) {
		_, err := CreateComment(context.Background(), f.db, CommentInput{
			PostID: f.postID, Actor: readerActor(f.readerID), Body: "还能评论吗",
		}, CommentOptions{Enabled: false}, testNow)
		if !errors.Is(err, ErrCommentsDisabled) {
			t.Fatalf("error = %v, want ErrCommentsDisabled", err)
		}
		if comments := countRows(t, f.db, "comments"); comments != 0 {
			t.Errorf("comments = %d, want none", comments)
		}
	})

	t.Run("moderation holds readers", func(t *testing.T) {
		comment := f.comment(t, readerActor(f.readerID), "读者发言", nil, CommentOptions{Enabled: true, Moderation: true})
		if comment.Status != StatusPending {
			t.Errorf("status = %q, want pending", comment.Status)
		}
		assertNotPublic(t, f, comment.ID)
	})

	t.Run("the owner is always approved", func(t *testing.T) {
		comment := f.comment(t, ownerActor(f.ownerID), "站长回复", nil, CommentOptions{Enabled: true, Moderation: true})
		if comment.Status != StatusApproved {
			t.Errorf("status = %q, want approved", comment.Status)
		}
	})
}

// assertNotPublic checks that a comment id never reaches the public listing.
func assertNotPublic(t *testing.T, f *fixture, id int64) {
	t.Helper()
	public, err := Comments(context.Background(), f.db, f.postID, 0)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	for _, comment := range public {
		if comment.ID == id {
			t.Fatalf("comment %d is publicly visible", id)
		}
		for _, reply := range comment.Replies {
			if reply.ID == id {
				t.Fatalf("reply %d is publicly visible", id)
			}
		}
	}
}

func TestCreateCommentRequiresAPublishedPost(t *testing.T) {
	f := newFixture(t)

	for _, tc := range []struct {
		name   string
		postID int64
	}{
		{"draft", f.draftID},
		{"unknown", 9999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateComment(context.Background(), f.db, CommentInput{
				PostID: tc.postID, Actor: readerActor(f.readerID), Body: "看不见的内容",
			}, f.commentsEnabled(), testNow)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}
	if comments := countRows(t, f.db, "comments"); comments != 0 {
		t.Errorf("comments = %d, want none", comments)
	}
}

func TestReplyInvariants(t *testing.T) {
	f := newFixture(t)
	root := f.comment(t, readerActor(f.readerID), "顶层评论", nil, f.commentsEnabled())

	t.Run("reply to a top-level comment", func(t *testing.T) {
		reply := f.comment(t, readerActor(f.otherID), "一级回复", &root.ID, f.commentsEnabled())
		if reply.ParentID == nil || *reply.ParentID != root.ID {
			t.Errorf("parent = %v, want %d", reply.ParentID, root.ID)
		}
	})

	t.Run("second level is refused", func(t *testing.T) {
		_, err := CreateComment(context.Background(), f.db, CommentInput{
			PostID: f.postID, Actor: readerActor(f.otherID), Body: "二级回复", ParentID: &root.ID,
		}, f.commentsEnabled(), testNow)
		if err != nil {
			t.Fatalf("first reply failed: %v", err)
		}
		// Reply to the reply: the parent already has a parent.
		var replyID int64
		if err := f.db.QueryRow(`SELECT id FROM comments WHERE parent_id = ? LIMIT 1`, root.ID).Scan(&replyID); err != nil {
			t.Fatalf("find reply: %v", err)
		}
		_, err = CreateComment(context.Background(), f.db, CommentInput{
			PostID: f.postID, Actor: readerActor(f.readerID), Body: "三级回复", ParentID: &replyID,
		}, f.commentsEnabled(), testNow)
		assertCode(t, err, "invalid_parent")
	})

	t.Run("parent from another post", func(t *testing.T) {
		elsewhere := insertPost(t, f.db, "另一篇", "published", testNow.Format(time.RFC3339))
		other := f.comment(t, readerActor(f.readerID), "另一篇的评论", nil, f.commentsEnabled())
		// Reparent the comment into the other post, then try to reply from here.
		if _, err := f.db.Exec(`UPDATE comments SET post_id = ? WHERE id = ?`, elsewhere, other.ID); err != nil {
			t.Fatalf("reparent: %v", err)
		}
		_, err := CreateComment(context.Background(), f.db, CommentInput{
			PostID: f.postID, Actor: readerActor(f.otherID), Body: "跨文章回复", ParentID: &other.ID,
		}, f.commentsEnabled(), testNow)
		assertCode(t, err, "invalid_parent")
	})

	t.Run("unknown parent", func(t *testing.T) {
		unknown := int64(99999)
		_, err := CreateComment(context.Background(), f.db, CommentInput{
			PostID: f.postID, Actor: readerActor(f.otherID), Body: "回复不存在", ParentID: &unknown,
		}, f.commentsEnabled(), testNow)
		assertCode(t, err, "invalid_parent")
	})

	t.Run("parent that is hidden", func(t *testing.T) {
		pending := f.comment(t, readerActor(f.otherID), "待审核评论", nil, CommentOptions{Enabled: true, Moderation: true})
		_, err := CreateComment(context.Background(), f.db, CommentInput{
			PostID: f.postID, Actor: readerActor(f.readerID), Body: "回复待审核", ParentID: &pending.ID,
		}, f.commentsEnabled(), testNow)
		assertCode(t, err, "invalid_parent")
	})
}

// ---------- reading comments ----------

func TestCommentsAreApprovedOrderedAndOneLevelDeep(t *testing.T) {
	f := newFixture(t)
	first := f.comment(t, readerActor(f.readerID), "第一条", nil, f.commentsEnabled())
	second := f.comment(t, readerActor(f.otherID), "第二条", nil, f.commentsEnabled())
	f.comment(t, readerActor(f.readerID), "待审核", nil, CommentOptions{Enabled: true, Moderation: true})
	f.comment(t, readerActor(f.readerID), "已被拒绝的", nil, f.commentsEnabled())
	f.comment(t, readerActor(f.readerID), "已删除的", nil, f.commentsEnabled())
	if _, err := f.db.Exec(`UPDATE comments SET status = 'rejected' WHERE body = '已被拒绝的'`); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE comments SET deleted_at = ? WHERE body = '已删除的'`, testNow.Format(time.RFC3339)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reply := f.comment(t, readerActor(f.otherID), "对第一条的回复", &first.ID, f.commentsEnabled())
	orphanParent := f.comment(t, readerActor(f.readerID), "会被删除的父评论", nil, f.commentsEnabled())
	f.comment(t, readerActor(f.readerID), "孤儿回复", &orphanParent.ID, f.commentsEnabled())
	if _, err := f.db.Exec(`UPDATE comments SET deleted_at = ? WHERE id = ?`, testNow.Format(time.RFC3339), orphanParent.ID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}

	public, err := Comments(context.Background(), f.db, f.postID, f.readerID)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(public) != 2 {
		t.Fatalf("roots = %d, want 2: %+v", len(public), public)
	}
	if public[0].ID != first.ID || public[1].ID != second.ID {
		t.Errorf("order = %d,%d, want oldest first", public[0].ID, public[1].ID)
	}
	if len(public[0].Replies) != 1 || public[0].Replies[0].ID != reply.ID {
		t.Fatalf("replies = %+v, want the reply under its parent", public[0].Replies)
	}
	if len(public[1].Replies) != 0 {
		t.Errorf("second comment has replies = %+v", public[1].Replies)
	}
	if !public[0].Mine {
		t.Error("first comment should be marked as written by the viewer")
	}
	if public[1].Mine {
		t.Error("another account's comment must not be marked as mine")
	}
	for _, comment := range public {
		if comment.Body == "孤儿回复" || comment.Status != StatusApproved {
			t.Errorf("public list leaked %+v", comment)
		}
	}
}

func TestCommentsHidesRepliesOfADeletedParent(t *testing.T) {
	f := newFixture(t)
	parent := f.comment(t, readerActor(f.readerID), "父评论", nil, f.commentsEnabled())
	child := f.comment(t, readerActor(f.otherID), "子回复", &parent.ID, f.commentsEnabled())

	if err := DeleteComment(context.Background(), f.db, parent.ID, readerActor(f.readerID), testNow); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	public, err := Comments(context.Background(), f.db, f.postID, f.ownerID)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(public) != 0 {
		t.Errorf("public list = %+v, want nothing: a deleted parent must hide its replies", public)
	}
	var body string
	if err := f.db.QueryRow(`SELECT body FROM comments WHERE id = ? AND deleted_at IS NULL`, child.ID).Scan(&body); err != nil {
		t.Fatalf("the reply row should survive as data: %v", err)
	}

	// A reply whose parent was rejected is hidden for the same reason.
	rejected := f.comment(t, readerActor(f.readerID), "会被拒绝", nil, f.commentsEnabled())
	f.comment(t, readerActor(f.otherID), "被拒绝评论的回复", &rejected.ID, f.commentsEnabled())
	if _, err := f.db.Exec(`UPDATE comments SET status = 'rejected' WHERE id = ?`, rejected.ID); err != nil {
		t.Fatalf("reject parent: %v", err)
	}
	public, err = Comments(context.Background(), f.db, f.postID, 0)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(public) != 0 {
		t.Errorf("public list = %+v, want nothing under a rejected parent", public)
	}
}

// ---------- deleting comments ----------

func TestDeleteCommentAuthorization(t *testing.T) {
	f := newFixture(t)
	mine := f.comment(t, readerActor(f.readerID), "我写的", nil, f.commentsEnabled())
	theirs := f.comment(t, readerActor(f.otherID), "别人写的", nil, f.commentsEnabled())

	t.Run("another reader may not delete", func(t *testing.T) {
		err := DeleteComment(context.Background(), f.db, theirs.ID, readerActor(f.readerID), testNow)
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("error = %v, want ErrForbidden", err)
		}
	})

	t.Run("the author may", func(t *testing.T) {
		if err := DeleteComment(context.Background(), f.db, mine.ID, readerActor(f.readerID), testNow); err != nil {
			t.Fatalf("DeleteComment: %v", err)
		}
	})

	t.Run("the owner may delete anyone's", func(t *testing.T) {
		if err := DeleteComment(context.Background(), f.db, theirs.ID, ownerActor(f.ownerID), testNow); err != nil {
			t.Fatalf("DeleteComment: %v", err)
		}
	})

	t.Run("deleting twice is not found", func(t *testing.T) {
		if err := DeleteComment(context.Background(), f.db, theirs.ID, ownerActor(f.ownerID), testNow); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		if err := DeleteComment(context.Background(), f.db, 99999, ownerActor(f.ownerID), testNow); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})
}

// ---------- moderation ----------

func TestModerationTransitions(t *testing.T) {
	f := newFixture(t)
	pending := f.comment(t, readerActor(f.readerID), "待审核", nil, CommentOptions{Enabled: true, Moderation: true})

	approved, err := Approve(context.Background(), f.db, pending.ID, testNow)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != StatusApproved {
		t.Errorf("status = %q, want approved", approved.Status)
	}
	public, err := Comments(context.Background(), f.db, f.postID, 0)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(public) != 1 || public[0].ID != pending.ID {
		t.Fatalf("public list = %+v, want the approved comment", public)
	}

	t.Run("approving twice is idempotent", func(t *testing.T) {
		again, err := Approve(context.Background(), f.db, pending.ID, testNow.Add(time.Minute))
		if err != nil {
			t.Fatalf("Approve: %v", err)
		}
		if again.Status != StatusApproved || again.ID != pending.ID {
			t.Errorf("second approve = %+v", again)
		}
	})

	t.Run("reject hides it again", func(t *testing.T) {
		rejected, err := Reject(context.Background(), f.db, pending.ID, testNow.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("Reject: %v", err)
		}
		if rejected.Status != StatusRejected {
			t.Errorf("status = %q, want rejected", rejected.Status)
		}
		assertNotPublic(t, f, pending.ID)
	})

	t.Run("unknown comment", func(t *testing.T) {
		if _, err := Approve(context.Background(), f.db, 99999, testNow); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		if _, err := Reject(context.Background(), f.db, 99999, testNow); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("a deleted comment cannot be moderated", func(t *testing.T) {
		if err := DeleteComment(context.Background(), f.db, pending.ID, ownerActor(f.ownerID), testNow); err != nil {
			t.Fatalf("DeleteComment: %v", err)
		}
		if _, err := Approve(context.Background(), f.db, pending.ID, testNow); !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
	})
}

func TestModerationQueue(t *testing.T) {
	f := newFixture(t)
	pending := f.comment(t, readerActor(f.readerID), "待审核一", nil, CommentOptions{Enabled: true, Moderation: true})
	f.comment(t, readerActor(f.otherID), "待审核二", nil, CommentOptions{Enabled: true, Moderation: true})
	f.comment(t, readerActor(f.readerID), "已通过", nil, f.commentsEnabled())
	if _, err := Reject(context.Background(), f.db, pending.ID, testNow); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	queue, err := Queue(context.Background(), f.db, QueueOptions{Status: StatusPending})
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue) != 1 || queue[0].Body != "待审核二" {
		t.Fatalf("pending queue = %+v", queue)
	}
	if queue[0].Author.DisplayName != "读者乙" {
		t.Errorf("queue author = %q", queue[0].Author.DisplayName)
	}
	if queue[0].PostTitle == "" || queue[0].PostSlug == "" {
		t.Errorf("queue item lacks its post context: %+v", queue[0])
	}

	approvedQueue, err := Queue(context.Background(), f.db, QueueOptions{Status: StatusApproved})
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(approvedQueue) != 1 || approvedQueue[0].Body != "已通过" {
		t.Errorf("approved queue = %+v", approvedQueue)
	}

	rejectedQueue, err := Queue(context.Background(), f.db, QueueOptions{Status: StatusRejected})
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(rejectedQueue) != 1 || rejectedQueue[0].ID != pending.ID {
		t.Errorf("rejected queue = %+v", rejectedQueue)
	}

	t.Run("default status is pending", func(t *testing.T) {
		queue, err := Queue(context.Background(), f.db, QueueOptions{})
		if err != nil {
			t.Fatalf("Queue: %v", err)
		}
		if len(queue) != 1 || queue[0].Body != "待审核二" {
			t.Errorf("queue = %+v, want the pending default", queue)
		}
	})

	t.Run("bad status and limit", func(t *testing.T) {
		_, err := Queue(context.Background(), f.db, QueueOptions{Status: "hidden"})
		assertCode(t, err, "invalid_status")
		_, err = Queue(context.Background(), f.db, QueueOptions{Status: StatusPending, Limit: 99})
		assertCode(t, err, "invalid_limit")
	})

	t.Run("limit caps the page", func(t *testing.T) {
		queue, err := Queue(context.Background(), f.db, QueueOptions{Status: StatusPending, Limit: 1})
		if err != nil {
			t.Fatalf("Queue: %v", err)
		}
		if len(queue) != 1 {
			t.Errorf("queue = %d entries, want 1", len(queue))
		}
	})

	t.Run("deleted comments leave the queue", func(t *testing.T) {
		queue, _ := Queue(context.Background(), f.db, QueueOptions{Status: StatusPending})
		if len(queue) != 1 {
			t.Fatalf("queue = %+v", queue)
		}
		if err := DeleteComment(context.Background(), f.db, queue[0].ID, ownerActor(f.ownerID), testNow); err != nil {
			t.Fatalf("DeleteComment: %v", err)
		}
		after, err := Queue(context.Background(), f.db, QueueOptions{Status: StatusPending})
		if err != nil {
			t.Fatalf("Queue: %v", err)
		}
		if len(after) != 0 {
			t.Errorf("queue = %+v, want empty", after)
		}
	})
}

// ---------- likes and bookmarks ----------

func TestSetLikeIsIdempotentAndCounted(t *testing.T) {
	f := newFixture(t)

	first, err := SetLike(context.Background(), f.db, f.readerID, f.postID, true, testNow)
	if err != nil {
		t.Fatalf("SetLike: %v", err)
	}
	if !first.On || first.Count != 1 {
		t.Fatalf("first like = %+v, want on with count 1", first)
	}

	again, err := SetLike(context.Background(), f.db, f.readerID, f.postID, true, testNow.Add(time.Second))
	if err != nil {
		t.Fatalf("SetLike: %v", err)
	}
	if !again.On || again.Count != 1 {
		t.Errorf("second like = %+v, want the same final state", again)
	}
	if likes := countRows(t, f.db, "likes"); likes != 1 {
		t.Errorf("likes = %d, want 1", likes)
	}

	other, err := SetLike(context.Background(), f.db, f.otherID, f.postID, true, testNow)
	if err != nil {
		t.Fatalf("SetLike: %v", err)
	}
	if other.Count != 2 {
		t.Errorf("count = %d, want 2", other.Count)
	}

	off, err := SetLike(context.Background(), f.db, f.readerID, f.postID, false, testNow)
	if err != nil {
		t.Fatalf("SetLike: %v", err)
	}
	if off.On || off.Count != 1 {
		t.Errorf("unlike = %+v, want off with count 1", off)
	}

	repeat, err := SetLike(context.Background(), f.db, f.readerID, f.postID, false, testNow)
	if err != nil {
		t.Fatalf("SetLike: %v", err)
	}
	if repeat.On || repeat.Count != 1 {
		t.Errorf("second unlike = %+v, want the same final state", repeat)
	}

	t.Run("draft and unknown posts", func(t *testing.T) {
		for _, id := range []int64{f.draftID, 99999} {
			if _, err := SetLike(context.Background(), f.db, f.readerID, id, true, testNow); !errors.Is(err, ErrNotFound) {
				t.Fatalf("like post %d: error = %v, want ErrNotFound", id, err)
			}
		}
		if likes := countRows(t, f.db, "likes"); likes != 1 {
			t.Errorf("likes = %d, want only the other reader's", likes)
		}
	})

	t.Run("missing actor", func(t *testing.T) {
		if _, err := SetLike(context.Background(), f.db, 0, f.postID, true, testNow); err == nil {
			t.Fatal("SetLike accepted a zero user id")
		}
	})
}

func TestSetBookmarkIsPrivateAndIdempotent(t *testing.T) {
	f := newFixture(t)

	first, err := SetBookmark(context.Background(), f.db, f.readerID, f.postID, true, testNow)
	if err != nil {
		t.Fatalf("SetBookmark: %v", err)
	}
	if !first.On || first.Count != 1 {
		t.Fatalf("first bookmark = %+v", first)
	}
	again, err := SetBookmark(context.Background(), f.db, f.readerID, f.postID, true, testNow)
	if err != nil {
		t.Fatalf("SetBookmark: %v", err)
	}
	if !again.On || again.Count != 1 {
		t.Errorf("second bookmark = %+v, want the same final state", again)
	}
	if bookmarks := countRows(t, f.db, "bookmarks"); bookmarks != 1 {
		t.Errorf("bookmarks = %d, want 1", bookmarks)
	}

	// Count is per user, so another account's bookmark does not move it.
	if _, err := SetBookmark(context.Background(), f.db, f.otherID, f.postID, true, testNow); err != nil {
		t.Fatalf("SetBookmark: %v", err)
	}
	mine, err := ViewerStates(context.Background(), f.db, f.readerID, []int64{f.postID})
	if err != nil {
		t.Fatalf("ViewerStates: %v", err)
	}
	if !mine[f.postID].Bookmarked || mine[f.postID].Liked {
		t.Errorf("viewer state = %+v", mine[f.postID])
	}

	off, err := SetBookmark(context.Background(), f.db, f.readerID, f.postID, false, testNow)
	if err != nil {
		t.Fatalf("SetBookmark: %v", err)
	}
	if off.On || off.Count != 0 {
		t.Errorf("removed bookmark = %+v, want off with an empty total", off)
	}
	if _, err := SetBookmark(context.Background(), f.db, f.readerID, f.draftID, true, testNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound for a draft", err)
	}
}

func TestViewerStates(t *testing.T) {
	f := newFixture(t)
	second := insertPost(t, f.db, "第二篇", "published", testNow.Format(time.RFC3339))
	if _, err := SetLike(context.Background(), f.db, f.readerID, f.postID, true, testNow); err != nil {
		t.Fatalf("SetLike: %v", err)
	}
	if _, err := SetBookmark(context.Background(), f.db, f.readerID, second, true, testNow); err != nil {
		t.Fatalf("SetBookmark: %v", err)
	}

	states, err := ViewerStates(context.Background(), f.db, f.readerID, []int64{f.postID, second, f.draftID})
	if err != nil {
		t.Fatalf("ViewerStates: %v", err)
	}
	if !states[f.postID].Liked || states[f.postID].Bookmarked {
		t.Errorf("first state = %+v", states[f.postID])
	}
	if states[second].Liked || !states[second].Bookmarked {
		t.Errorf("second state = %+v", states[second])
	}
	if state, ok := states[f.draftID]; ok && (state.Liked || state.Bookmarked) {
		t.Errorf("draft state = %+v", state)
	}

	t.Run("another account sees nothing", func(t *testing.T) {
		other, err := ViewerStates(context.Background(), f.db, f.otherID, []int64{f.postID, second})
		if err != nil {
			t.Fatalf("ViewerStates: %v", err)
		}
		if other[f.postID].Liked || other[second].Bookmarked {
			t.Errorf("states = %+v, want empty states", other)
		}
	})

	t.Run("no ids or no viewer issues no query", func(t *testing.T) {
		if states, err := ViewerStates(context.Background(), f.db, f.readerID, nil); err != nil || len(states) != 0 {
			t.Errorf("states = %+v, err = %v", states, err)
		}
		if states, err := ViewerStates(context.Background(), f.db, 0, []int64{f.postID}); err != nil || len(states) != 0 {
			t.Errorf("guest states = %+v, err = %v", states, err)
		}
	})
}

// ---------- bookmarks list ----------

func TestBookmarkPagePaginationAndVisibility(t *testing.T) {
	f := newFixture(t)
	stamp := testNow.Format(time.RFC3339)
	ids := []int64{f.postID}
	for i := 0; i < 4; i++ {
		ids = append(ids, insertPost(t, f.db, "收藏目标", "published", stamp))
	}
	// Bookmark them oldest first, so the list has a deterministic order.
	for index, id := range ids {
		at := testNow.Add(time.Duration(index) * time.Second)
		if _, err := SetBookmark(context.Background(), f.db, f.readerID, id, true, at); err != nil {
			t.Fatalf("SetBookmark: %v", err)
		}
	}
	// A draft bookmark must never surface.
	if _, err := f.db.Exec(`INSERT INTO bookmarks(user_id, post_id, created_at) VALUES(?,?,?)`,
		f.readerID, f.draftID, testNow.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert draft bookmark: %v", err)
	}
	// Another account's bookmarks stay invisible.
	if _, err := SetBookmark(context.Background(), f.db, f.otherID, f.postID, true, testNow); err != nil {
		t.Fatalf("SetBookmark: %v", err)
	}

	page, err := Bookmarks(context.Background(), f.db, f.readerID, "", 2)
	if err != nil {
		t.Fatalf("BookmarkPage: %v", err)
	}
	if len(page.PostIDs) != 2 {
		t.Fatalf("page = %+v, want 2 ids", page.PostIDs)
	}
	if page.PostIDs[0] != ids[4] || page.PostIDs[1] != ids[3] {
		t.Errorf("newest first: %v", page.PostIDs)
	}
	if page.NextCursor == "" {
		t.Fatal("first page has no cursor")
	}

	second, err := Bookmarks(context.Background(), f.db, f.readerID, page.NextCursor, 2)
	if err != nil {
		t.Fatalf("BookmarkPage: %v", err)
	}
	if len(second.PostIDs) != 2 || second.PostIDs[0] != ids[2] || second.PostIDs[1] != ids[1] {
		t.Errorf("second page = %v", second.PostIDs)
	}

	third, err := Bookmarks(context.Background(), f.db, f.readerID, second.NextCursor, 2)
	if err != nil {
		t.Fatalf("BookmarkPage: %v", err)
	}
	if len(third.PostIDs) != 1 || third.PostIDs[0] != ids[0] {
		t.Errorf("third page = %v", third.PostIDs)
	}
	if third.NextCursor != "" {
		t.Errorf("last page cursor = %q, want empty", third.NextCursor)
	}

	seen := map[int64]bool{}
	for _, page := range [][]int64{page.PostIDs, second.PostIDs, third.PostIDs} {
		for _, id := range page {
			if seen[id] {
				t.Errorf("id %d appeared twice across pages", id)
			}
			seen[id] = true
			if id == f.draftID {
				t.Errorf("the draft bookmark leaked into the list")
			}
		}
	}

	t.Run("bad cursor", func(t *testing.T) {
		_, err := Bookmarks(context.Background(), f.db, f.readerID, "not-a-cursor", 2)
		assertCode(t, err, "invalid_cursor")
	})

	t.Run("paging while new bookmarks arrive stays stable", func(t *testing.T) {
		first, err := Bookmarks(context.Background(), f.db, f.readerID, "", 2)
		if err != nil {
			t.Fatalf("BookmarkPage: %v", err)
		}
		fresh := insertPost(t, f.db, "刚刚收藏", "published", stamp)
		if _, err := SetBookmark(context.Background(), f.db, f.readerID, fresh, true, testNow.Add(time.Hour)); err != nil {
			t.Fatalf("SetBookmark: %v", err)
		}
		next, err := Bookmarks(context.Background(), f.db, f.readerID, first.NextCursor, 2)
		if err != nil {
			t.Fatalf("BookmarkPage: %v", err)
		}
		for _, id := range next.PostIDs {
			if id == fresh {
				t.Errorf("a bookmark created after the first page shifted the cursor")
			}
		}
	})

	t.Run("unknown stamp in the cursor", func(t *testing.T) {
		_, err := Bookmarks(context.Background(), f.db, f.readerID, "2026-01-01T00:00:00Z,1", 2)
		if err != nil {
			t.Fatalf("a cursor pointing past the end must be an empty page: %v", err)
		}
	})

	t.Run("bookmarking twice keeps one row", func(t *testing.T) {
		if _, err := SetBookmark(context.Background(), f.db, f.readerID, ids[0], true, testNow); err != nil {
			t.Fatalf("SetBookmark: %v", err)
		}
		all, err := Bookmarks(context.Background(), f.db, f.readerID, "", MaxPageSize)
		if err != nil {
			t.Fatalf("BookmarkPage: %v", err)
		}
		unique := map[int64]bool{}
		for _, id := range all.PostIDs {
			if unique[id] {
				t.Errorf("id %d listed twice", id)
			}
			unique[id] = true
		}
	})
}
