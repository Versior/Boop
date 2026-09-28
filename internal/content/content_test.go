package content

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"boop/internal/store"
)

// ---------- fixtures ----------

var testNow = time.Date(2026, 9, 28, 6, 2, 3, 0, time.UTC)

// countingConnector wraps the real SQLite driver so tests can prove how many
// statements an operation issues (the N+1 guard).
type countingConnector struct {
	count *int64
	dsn   string
}

func (c countingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, count: c.count}, nil
}

func (c countingConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type countingConn struct {
	driver.Conn
	count *int64
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	atomic.AddInt64(c.count, 1)
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return q.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	atomic.AddInt64(c.count, 1)
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, query, args)
}

type testFixture struct {
	db *sql.DB
	// counted is a second, identically migrated database opened through an
	// instrumented connector, used to count the statements an operation issues.
	counted  *sql.DB
	ownerID  int64
	readerID int64
	queries  *int64
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	count := new(int64)
	counted := sql.OpenDB(countingConnector{count: count, dsn: store.DSN(filepath.Join(dir, "counted.db"))})
	t.Cleanup(func() { _ = counted.Close() })
	counted.SetMaxOpenConns(1)
	if err := store.Migrate(counted); err != nil {
		t.Fatalf("store.Migrate(counted): %v", err)
	}

	f := &testFixture{db: db, counted: counted, queries: count}
	stamp := testNow.Format(time.RFC3339)
	f.ownerID = insertUser(t, db, "owner@example.com", "owner", "遇事开心", stamp)
	f.readerID = insertUser(t, db, "reader@example.com", "reader", "读者甲", stamp)
	// Same identities in the counted database so foreign keys resolve there too.
	insertUserWithID(t, counted, f.ownerID, "owner@example.com", "owner", "遇事开心", stamp)
	insertUserWithID(t, counted, f.readerID, "reader@example.com", "reader", "读者甲", stamp)
	return f
}

func insertUser(t *testing.T, db *sql.DB, email, role, name, stamp string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO users(email, display_name, role, created_at, updated_at) VALUES(?,?,?,?,?)`,
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

func insertUserWithID(t *testing.T, db *sql.DB, id int64, email, role, name, stamp string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO users(id, email, display_name, role, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
		id, email, name, role, stamp, stamp); err != nil {
		t.Fatalf("insert user %d: %v", id, err)
	}
}

// insertAsset creates the asset row a photo post needs; uploads arrive in Task 5.
func (f *testFixture) insertAsset(t *testing.T, key string) int64 {
	t.Helper()
	res, err := f.db.Exec(
		`INSERT INTO assets(owner_user_id, storage_key, original_name, mime_type, size_bytes, sha256, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		f.ownerID, key, key+".jpg", "image/jpeg", 1024, []byte("sha"), testNow.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("asset id: %v", err)
	}
	return id
}

// mirrorPosts copies the posts of the primary database into the counted one so
// both see the same feed size.
func (f *testFixture) mirrorPosts(t *testing.T) {
	t.Helper()
	rows, err := f.db.Query(`SELECT id, slug, type, status, title, body_markdown, body_html, excerpt,
		cover_asset_id, location, captured_at, seo_title, seo_description, published_at, created_at, updated_at, deleted_at
		FROM posts ORDER BY id`)
	if err != nil {
		t.Fatalf("read posts: %v", err)
	}
	defer rows.Close()

	var records [][]any
	for rows.Next() {
		var (
			id                                                       int64
			slug, kind, status, title, body, html, excerpt, location string
			seoTitle, seoDescription, createdAt, updatedAt           string
			coverID                                                  sql.NullInt64
			capturedAt, publishedAt, deletedAt                       sql.NullString
		)
		if err := rows.Scan(&id, &slug, &kind, &status, &title, &body, &html, &excerpt,
			&coverID, &location, &capturedAt, &seoTitle, &seoDescription, &publishedAt, &createdAt, &updatedAt, &deletedAt); err != nil {
			t.Fatalf("scan post: %v", err)
		}
		records = append(records, []any{id, slug, kind, status, title, body, html, excerpt,
			coverID, location, capturedAt, seoTitle, seoDescription, publishedAt, createdAt, updatedAt, deletedAt})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate posts: %v", err)
	}
	rows.Close()

	for _, values := range records {
		if _, err := f.counted.Exec(`INSERT INTO posts(id, slug, type, status, title, body_markdown, body_html, excerpt,
			cover_asset_id, location, captured_at, seo_title, seo_description, published_at, created_at, updated_at, deleted_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, values...); err != nil {
			t.Fatalf("mirror post: %v", err)
		}
	}
}

func (f *testFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// ---------- slug ----------

func TestSlugify(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"latin words", "Hello Boop World", "hello-boop-world"},
		{"chinese kept", "关于写作", "关于写作"},
		{"mixed", "Go 语言 1.27 发布", "go-语言-1-27-发布"},
		{"punctuation collapses", "a!!!b???c", "a-b-c"},
		{"trim separators", "  ---hello---  ", "hello"},
		{"symbols only", "!!!???", ""},
		{"emoji dropped", "hi 🌊 there", "hi-there"},
		{"long input truncated", strings.Repeat("a", 200), strings.Repeat("a", MaxSlugRunes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Slugify(tc.in); got != tc.want {
				t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSlugifyTruncatesOnRuneBoundary(t *testing.T) {
	got := Slugify(strings.Repeat("中", MaxSlugRunes+10))
	if n := len([]rune(got)); n != MaxSlugRunes {
		t.Fatalf("slug has %d runes, want %d", n, MaxSlugRunes)
	}
	if !strings.HasPrefix(strings.Repeat("中", MaxSlugRunes), got) {
		t.Errorf("slug %q is not a prefix of the input", got)
	}
}

func TestSlugIsURLSafeShape(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	post, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeArticle, Status: StatusPublished, Title: "Hello, World! 你好", Body: "正文"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !regexp.MustCompile(`^[a-z0-9\x{4e00}-\x{9fff}-]+$`).MatchString(post.Slug) {
		t.Errorf("slug %q does not match the expected character set", post.Slug)
	}
}

// ---------- type specific validation ----------

func TestValidateMomentBody(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		body    string
		status  string
		wantErr string
	}{
		{"published needs a body", "", StatusPublished, "invalid_body"},
		{"published body at the limit", strings.Repeat("字", MaxMomentRunes), StatusPublished, ""},
		{"published body over the limit", strings.Repeat("字", MaxMomentRunes+1), StatusPublished, "invalid_body"},
		{"draft may be empty", "", StatusDraft, ""},
		{"archived may be empty", "", StatusArchived, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			post, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: tc.status, Body: tc.body}, testNow)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if post.Status != tc.status {
					t.Errorf("status = %q, want %q", post.Status, tc.status)
				}
				return
			}
			assertValidation(t, err, tc.wantErr)
		})
	}
}

func TestValidateArticleTitleAndBody(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		in      Input
		wantErr string
	}{
		{"published without body", Input{Type: TypeArticle, Status: StatusPublished, Title: "标题"}, "invalid_body"},
		{"published without title", Input{Type: TypeArticle, Status: StatusPublished, Body: "正文"}, "invalid_title"},
		{"published with blank title", Input{Type: TypeArticle, Status: StatusPublished, Title: "   ", Body: "正文"}, "invalid_title"},
		{"published complete", Input{Type: TypeArticle, Status: StatusPublished, Title: "标题", Body: "正文"}, ""},
		{"draft without body", Input{Type: TypeArticle, Status: StatusDraft, Title: "只写了标题"}, ""},
		{"title over the limit", Input{Type: TypeArticle, Status: StatusDraft, Title: strings.Repeat("字", MaxTitleRunes+1)}, "invalid_title"},
		{"body over the limit", Input{Type: TypeArticle, Status: StatusDraft, Body: strings.Repeat("a", MaxBodyBytes+1)}, "invalid_body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Create(ctx, f.db, f.ownerID, tc.in, testNow)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				return
			}
			assertValidation(t, err, tc.wantErr)
		})
	}
}

func TestPhotoPublishRequiresAnExistingAsset(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assetID := f.insertAsset(t, "photos/one.jpg")

	t.Run("moment without a title", func(t *testing.T) {
		_, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Title: "标题", Body: "正文"}, testNow)
		assertValidation(t, err, "invalid_title")
	})
	t.Run("moment with a photo", func(t *testing.T) {
		post, err := Create(ctx, f.db, f.ownerID, Input{
			Type: TypeMoment, Status: StatusPublished, Body: "带图的动态", AssetIDs: []int64{assetID},
		}, testNow)
		if err != nil {
			t.Fatalf("moments may carry an image (docs/PRODUCT.md §4): %v", err)
		}
		if len(post.Assets) != 1 {
			t.Errorf("assets = %+v, want the linked image", post.Assets)
		}
	})
	t.Run("article with a cover", func(t *testing.T) {
		if _, err := Create(ctx, f.db, f.ownerID, Input{
			Type: TypeArticle, Status: StatusPublished, Title: "封面文章", Body: "正文", AssetIDs: []int64{assetID},
		}, testNow); err != nil {
			t.Fatalf("articles may carry a cover: %v", err)
		}
	})
	t.Run("photo without assets published", func(t *testing.T) {
		_, err := Create(ctx, f.db, f.ownerID, Input{Type: TypePhoto, Status: StatusPublished, Body: "雾"}, testNow)
		assertValidation(t, err, "invalid_assets")
	})
	t.Run("photo draft without assets", func(t *testing.T) {
		if _, err := Create(ctx, f.db, f.ownerID, Input{Type: TypePhoto, Status: StatusDraft}, testNow); err != nil {
			t.Fatalf("draft photo without assets: %v", err)
		}
	})
	t.Run("photo with an unknown asset", func(t *testing.T) {
		_, err := Create(ctx, f.db, f.ownerID, Input{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{9999}}, testNow)
		assertValidation(t, err, "invalid_asset")
	})
	t.Run("photo with a known asset", func(t *testing.T) {
		post, err := Create(ctx, f.db, f.ownerID, Input{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{assetID}}, testNow)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(post.Assets) != 1 || post.Assets[0].ID != assetID {
			t.Fatalf("assets = %+v, want the linked asset", post.Assets)
		}
	})
	t.Run("duplicate asset ids", func(t *testing.T) {
		_, err := Create(ctx, f.db, f.ownerID, Input{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{assetID, assetID}}, testNow)
		assertValidation(t, err, "invalid_asset")
	})
}

func TestCreateRejectsUnknownEnumsAndOutOfRangeFields(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		in      Input
		wantErr string
	}{
		{"unknown type", Input{Type: "video", Status: StatusDraft}, "invalid_type"},
		{"missing type", Input{Status: StatusDraft}, "invalid_type"},
		{"unknown status", Input{Type: TypeMoment, Status: "scheduled"}, "invalid_status"},
		{"missing status", Input{Type: TypeMoment}, "invalid_status"},
		{"excerpt too long", Input{Type: TypeMoment, Status: StatusDraft, Excerpt: strings.Repeat("字", MaxExcerptRunes+1)}, "invalid_excerpt"},
		{"too many tags", Input{Type: TypeMoment, Status: StatusDraft, Tags: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}}, "invalid_tags"},
		{"blank tag", Input{Type: TypeMoment, Status: StatusDraft, Tags: []string{"  "}}, "invalid_tags"},
		{"long tag", Input{Type: TypeMoment, Status: StatusDraft, Tags: []string{strings.Repeat("字", MaxTagRunes+1)}}, "invalid_tags"},
		{"location too long", Input{Type: TypeMoment, Status: StatusDraft, Location: strings.Repeat("字", MaxLocationRunes+1)}, "invalid_location"},
		{"captured_at is not a timestamp", Input{Type: TypePhoto, Status: StatusDraft, CapturedAt: "昨天"}, "invalid_captured_at"},
		{"too many assets", Input{Type: TypePhoto, Status: StatusDraft, AssetIDs: assetIDRange(MaxAssets + 1)}, "invalid_assets"},
		{"negative asset id", Input{Type: TypePhoto, Status: StatusDraft, AssetIDs: []int64{-1}}, "invalid_asset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Create(ctx, f.db, f.ownerID, tc.in, testNow)
			assertValidation(t, err, tc.wantErr)
		})
	}
}

func assetIDRange(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}

// ---------- markdown and sanitization ----------

func TestBodyHTMLIsRenderedAndSanitized(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	post, err := Create(ctx, f.db, f.ownerID, Input{
		Type:   TypeArticle,
		Status: StatusPublished,
		Title:  "写作",
		Body: "# 标题\n\n**粗体**与[链接](/p/x)\n\n" +
			"<script>alert('xss')</script>\n\n<img src=x onerror=alert(1)>\n\n[危险](javascript:alert(1))\n",
	}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !strings.Contains(post.BodyHTML, "<h1>标题</h1>") {
		t.Errorf("markdown heading missing: %s", post.BodyHTML)
	}
	if !strings.Contains(post.BodyHTML, "<strong>粗体</strong>") {
		t.Errorf("markdown emphasis missing: %s", post.BodyHTML)
	}
	lower := strings.ToLower(post.BodyHTML)
	for _, forbidden := range []string{"<script", "onerror", "javascript:", "alert("} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("sanitized HTML still contains %q: %s", forbidden, post.BodyHTML)
		}
	}
	if !strings.Contains(post.BodyHTML, `href="/p/x"`) {
		t.Errorf("relative link dropped: %s", post.BodyHTML)
	}
}

func TestMomentBodyIsPlainText(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	post, err := Create(ctx, f.db, f.ownerID, Input{
		Type:   TypeMoment,
		Status: StatusPublished,
		Body:   "第一行 <b>不是标签</b>\n第二行",
	}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.Contains(post.BodyHTML, "&lt;b&gt;") {
		t.Errorf("moment body is not escaped: %s", post.BodyHTML)
	}
	if strings.Contains(post.BodyHTML, "<b>") {
		t.Errorf("moment body was rendered as markup: %s", post.BodyHTML)
	}
	if !strings.Contains(post.BodyHTML, "<br>") {
		t.Errorf("line break missing: %s", post.BodyHTML)
	}
}

// ---------- slug allocation ----------

func TestSlugCollisionGetsSuffix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeArticle, Status: StatusPublished, Title: "同名文章", Body: "正文"}, testNow)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeArticle, Status: StatusPublished, Title: "同名文章", Body: "正文"}, testNow)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first.Slug == second.Slug {
		t.Fatalf("both posts share the slug %q", first.Slug)
	}
	if first.Slug != "同名文章" || second.Slug != "同名文章-2" {
		t.Fatalf("slugs = %q, %q; want 同名文章 and 同名文章-2", first.Slug, second.Slug)
	}
}

func TestSlugFallsBackToTypeAndTimestamp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	post, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "！！！"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := post.Slug; got != "moment-20260928t060203" {
		t.Fatalf("slug = %q, want moment-20260928t060203", got)
	}
}

func TestSlugIsStableAcrossUpdates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeArticle, Status: StatusDraft, Title: "初稿标题"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "改过的标题"
	body := "正文"
	updated, err := Update(ctx, f.db, created.ID, Patch{
		UpdatedAt: created.UpdatedAt,
		Title:     &title,
		Body:      &body,
	}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Slug != created.Slug {
		t.Fatalf("slug changed from %q to %q", created.Slug, updated.Slug)
	}
}

// ---------- optimistic locking ----------

func TestUpdateRequiresMatchingUpdatedAt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "第一版"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stale := "过期写入"
	_, err = Update(ctx, f.db, created.ID, Patch{
		UpdatedAt: testNow.Add(-time.Hour).Format(time.RFC3339),
		Body:      &stale,
	}, testNow.Add(time.Minute))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update error = %v, want ErrConflict", err)
	}

	var body string
	if err := f.db.QueryRow(`SELECT body_markdown FROM posts WHERE id = ?`, created.ID).Scan(&body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if body != "第一版" {
		t.Fatalf("body = %q, want the rejected update to leave the row untouched", body)
	}

	next := "第二版"
	updated, err := Update(ctx, f.db, created.ID, Patch{
		UpdatedAt: created.UpdatedAt,
		Body:      &next,
	}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("fresh update: %v", err)
	}
	if updated.BodyMarkdown != next {
		t.Errorf("body = %q, want %q", updated.BodyMarkdown, next)
	}
	if updated.UpdatedAt == created.UpdatedAt {
		t.Errorf("updated_at did not advance: %q", updated.UpdatedAt)
	}
}

// A coarse system clock must not defeat the optimistic lock: two updates that
// share the same instant still produce distinct revisions, and the first
// revision is then correctly rejected as stale.
func TestUpdatedAtAdvancesWithinOneClockTick(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "第一版"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	second := "第二版"
	first, err := Update(ctx, f.db, created.ID, Patch{UpdatedAt: created.UpdatedAt, Body: &second}, testNow)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if first.UpdatedAt == created.UpdatedAt {
		t.Fatalf("updated_at did not move: %q", first.UpdatedAt)
	}

	third := "第三版"
	again, err := Update(ctx, f.db, created.ID, Patch{UpdatedAt: first.UpdatedAt, Body: &third}, testNow)
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if again.UpdatedAt == first.UpdatedAt {
		t.Fatalf("updated_at repeated: %q", again.UpdatedAt)
	}

	stale := "过期写入"
	if _, err := Update(ctx, f.db, created.ID, Patch{UpdatedAt: created.UpdatedAt, Body: &stale}, testNow); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write error = %v, want ErrConflict", err)
	}
}

func TestUpdateRequiresAnUpdatedAtValue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "正文"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := "新正文"
	if _, err := Update(ctx, f.db, created.ID, Patch{Body: &body}, testNow); err == nil {
		t.Fatal("update without updated_at was accepted")
	} else {
		assertValidation(t, err, "invalid_updated_at")
	}
}

func TestUpdateUnknownPost(t *testing.T) {
	f := newFixture(t)
	body := "正文"
	_, err := Update(context.Background(), f.db, 4242, Patch{UpdatedAt: testNow.Format(time.RFC3339), Body: &body}, testNow)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestUpdateRevalidatesTheMergedPost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	draft, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeArticle, Status: StatusDraft, Title: "标题"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	published := StatusPublished
	_, err = Update(ctx, f.db, draft.ID, Patch{UpdatedAt: draft.UpdatedAt, Status: &published}, testNow.Add(time.Minute))
	assertValidation(t, err, "invalid_body")
}

func TestPublishTransitionSetsPublishedAtOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	draft, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusDraft, Body: "草稿正文"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if draft.PublishedAt != "" {
		t.Fatalf("draft published_at = %q, want empty", draft.PublishedAt)
	}

	published := StatusPublished
	first, err := Update(ctx, f.db, draft.ID, Patch{UpdatedAt: draft.UpdatedAt, Status: &published}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if first.PublishedAt != testNow.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("published_at = %q, want the publish moment", first.PublishedAt)
	}

	body := "补充说明"
	second, err := Update(ctx, f.db, draft.ID, Patch{UpdatedAt: first.UpdatedAt, Body: &body}, testNow.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if second.PublishedAt != first.PublishedAt {
		t.Fatalf("published_at moved from %q to %q", first.PublishedAt, second.PublishedAt)
	}
}

func TestUpdateReplacesAssetsAndTags(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := f.insertAsset(t, "photos/first.jpg")
	second := f.insertAsset(t, "photos/second.jpg")

	post, err := Create(ctx, f.db, f.ownerID, Input{
		Type: TypePhoto, Status: StatusPublished,
		AssetIDs: []int64{first}, Tags: []string{"海边"},
	}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	nextAssets := []int64{second}
	nextTags := []string{"海边", "雾"}
	updated, err := Update(ctx, f.db, post.ID, Patch{
		UpdatedAt: post.UpdatedAt, AssetIDs: &nextAssets, Tags: &nextTags,
	}, testNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.Assets) != 1 || updated.Assets[0].ID != second {
		t.Errorf("assets = %+v, want only the second asset", updated.Assets)
	}
	if strings.Join(updated.Tags, ",") != "海边,雾" {
		t.Errorf("tags = %v, want 海边,雾", updated.Tags)
	}

	var removed int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM post_assets WHERE post_id = ? AND asset_id = ?`, post.ID, first).Scan(&removed); err != nil {
		t.Fatalf("count post_assets: %v", err)
	}
	if removed != 0 {
		t.Errorf("the replaced asset link survived")
	}
	if assets := f.count(t, `SELECT COUNT(*) FROM assets`); assets != 2 {
		t.Errorf("assets rows = %d, want the 2 fixtures untouched", assets)
	}
}

func TestUpdateToEmptyAssetsOnPublishedPhoto(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assetID := f.insertAsset(t, "photos/only.jpg")

	post, err := Create(ctx, f.db, f.ownerID, Input{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{assetID}}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	empty := []int64{}
	_, err = Update(ctx, f.db, post.ID, Patch{UpdatedAt: post.UpdatedAt, AssetIDs: &empty}, testNow.Add(time.Minute))
	assertValidation(t, err, "invalid_assets")

	var links int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM post_assets WHERE post_id = ?`, post.ID).Scan(&links); err != nil {
		t.Fatalf("count post_assets: %v", err)
	}
	if links != 1 {
		t.Errorf("post_assets = %d, want the original link preserved", links)
	}
}

// ---------- transactional relationships ----------

func TestFailedRelationshipWriteRollsBackThePost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	before := f.count(t, `SELECT COUNT(*) FROM posts`)
	_, err := Create(ctx, f.db, f.ownerID, Input{
		Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{f.insertAsset(t, "photos/ok.jpg"), 9999},
	}, testNow)
	assertValidation(t, err, "invalid_asset")

	if after := f.count(t, `SELECT COUNT(*) FROM posts`); after != before {
		t.Errorf("posts = %d, want %d: the failed create must not leave a row", after, before)
	}
	if links := f.count(t, `SELECT COUNT(*) FROM post_assets`); links != 0 {
		t.Errorf("post_assets = %d, want 0", links)
	}
}

func TestTagReuseIsCaseInsensitiveAndSlugged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "一", Tags: []string{"Go 语言"}}, testNow)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "二", Tags: []string{"go 语言"}}, testNow)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first.Tags[0] != "Go 语言" {
		t.Errorf("first tag = %q, want the original casing", first.Tags[0])
	}
	if second.Tags[0] != "Go 语言" {
		t.Errorf("second tag = %q, want the existing tag reused", second.Tags[0])
	}
	if tags := f.count(t, `SELECT COUNT(*) FROM tags`); tags != 1 {
		t.Errorf("tags = %d, want a single row", tags)
	}
	if slug := f.count(t, `SELECT COUNT(*) FROM tags WHERE slug = 'go-语言'`); slug != 1 {
		t.Errorf("tag slug missing: %d rows match", slug)
	}
}

// ---------- soft delete ----------

func TestDeleteIsSoft(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	post, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "将被删除"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := Delete(ctx, f.db, post.ID, testNow.Add(time.Minute)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var deletedAt sql.NullString
	if err := f.db.QueryRow(`SELECT deleted_at FROM posts WHERE id = ?`, post.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("post row disappeared: %v", err)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		t.Errorf("deleted_at = %v, want a timestamp", deletedAt)
	}

	if _, err := BySlug(ctx, f.db, post.Slug); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted post lookup error = %v, want ErrNotFound", err)
	}
	page, err := Feed(ctx, f.db, FeedOptions{})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(page.Posts) != 0 {
		t.Errorf("feed still returns the deleted post")
	}

	if err := Delete(ctx, f.db, post.ID, testNow.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
	if err := Delete(ctx, f.db, 4242, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown delete error = %v, want ErrNotFound", err)
	}
}

// ---------- feed ----------

func TestFeedVisibility(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assetID := f.insertAsset(t, "photos/feed.jpg")

	seed := []Input{
		{Type: TypeMoment, Status: StatusPublished, Body: "公开动态"},
		{Type: TypeArticle, Status: StatusPublished, Title: "公开文章", Body: "正文"},
		{Type: TypePhoto, Status: StatusPublished, AssetIDs: []int64{assetID}},
		{Type: TypeMoment, Status: StatusDraft, Body: "草稿动态"},
		{Type: TypeArticle, Status: StatusArchived, Title: "归档文章", Body: "正文"},
	}
	for i, in := range seed {
		if _, err := Create(ctx, f.db, f.ownerID, in, testNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	removed, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: "已删除"}, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("Create deleted: %v", err)
	}
	if err := Delete(ctx, f.db, removed.ID, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	all, err := Feed(ctx, f.db, FeedOptions{})
	if err != nil {
		t.Fatalf("Feed all: %v", err)
	}
	if len(all.Posts) != 3 {
		t.Fatalf("feed returned %d posts, want the 3 published ones: %+v", len(all.Posts), slugs(all.Posts))
	}
	if all.Posts[0].Type != TypePhoto {
		t.Errorf("newest post type = %q, want the photo published last", all.Posts[0].Type)
	}

	articles, err := Feed(ctx, f.db, FeedOptions{Type: TypeArticle})
	if err != nil {
		t.Fatalf("Feed article: %v", err)
	}
	if len(articles.Posts) != 1 || articles.Posts[0].Title != "公开文章" {
		t.Fatalf("article feed = %+v, want only the published article", slugs(articles.Posts))
	}

	photos, err := Feed(ctx, f.db, FeedOptions{Type: TypePhoto})
	if err != nil {
		t.Fatalf("Feed photo: %v", err)
	}
	if len(photos.Posts) != 1 || len(photos.Posts[0].Assets) != 1 {
		t.Fatalf("photo feed = %+v, want the photo with its asset", slugs(photos.Posts))
	}

	if _, err := Feed(ctx, f.db, FeedOptions{Type: "video"}); err == nil {
		t.Error("feed accepted an unknown type")
	} else {
		assertValidation(t, err, "invalid_type")
	}
}

func TestFeedPaginationIsStable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Every post shares one published_at, so ordering must fall back to id.
	const total = 7
	for i := 0; i < total; i++ {
		if _, err := Create(ctx, f.db, f.ownerID, Input{
			Type: TypeMoment, Status: StatusPublished, Body: fmt.Sprintf("第 %d 条", i),
		}, testNow); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	seen := make(map[string]bool)
	order := make([]string, 0, total)
	cursor := ""
	for page := 0; ; page++ {
		result, err := Feed(ctx, f.db, FeedOptions{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("Feed page %d: %v", page, err)
		}
		for _, post := range result.Posts {
			if seen[post.Slug] {
				t.Fatalf("post %q returned twice", post.Slug)
			}
			seen[post.Slug] = true
			order = append(order, post.Slug)
		}
		if result.NextCursor == "" {
			break
		}
		if page > total {
			t.Fatal("pagination did not terminate")
		}
		cursor = result.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged through %d posts, want %d: %v", len(seen), total, order)
	}

	// Newest first, and the same order as a single large page.
	onePage, err := Feed(ctx, f.db, FeedOptions{Limit: MaxPageSize})
	if err != nil {
		t.Fatalf("Feed single page: %v", err)
	}
	for i, post := range onePage.Posts {
		if order[i] != post.Slug {
			t.Fatalf("position %d: paged order %q != single page %q", i, order[i], post.Slug)
		}
	}
}

func TestFeedRejectsBadCursorAndLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, cursor := range []string{"nonsense", "2026-09-28T06:02:03Z", "2026-09-28T06:02:03Z,abc"} {
		if _, err := Feed(ctx, f.db, FeedOptions{Cursor: cursor}); err == nil {
			t.Errorf("cursor %q was accepted", cursor)
		} else {
			assertValidation(t, err, "invalid_cursor")
		}
	}
	for _, limit := range []int{-1, MaxPageSize + 1} {
		if _, err := Feed(ctx, f.db, FeedOptions{Limit: limit}); err == nil {
			t.Errorf("limit %d was accepted", limit)
		} else {
			assertValidation(t, err, "invalid_limit")
		}
	}
}

func TestFeedLoadsCountsAssetsAndTags(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assetID := f.insertAsset(t, "photos/counts.jpg")

	post, err := Create(ctx, f.db, f.ownerID, Input{
		Type: TypePhoto, Status: StatusPublished, Body: "统计", AssetIDs: []int64{assetID}, Tags: []string{"海边"},
	}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Two approved comments, one pending and one deleted: only approved count.
	insertComment(t, f, post.ID, f.readerID, "approved", false)
	insertComment(t, f, post.ID, f.readerID, "approved", false)
	insertComment(t, f, post.ID, f.readerID, "pending", false)
	insertComment(t, f, post.ID, f.readerID, "approved", true)
	if _, err := f.db.Exec(`INSERT INTO likes(user_id, post_id, created_at) VALUES(?,?,?)`,
		f.readerID, post.ID, testNow.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert like: %v", err)
	}

	page, err := Feed(ctx, f.db, FeedOptions{})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(page.Posts) != 1 {
		t.Fatalf("feed returned %d posts", len(page.Posts))
	}
	got := page.Posts[0]
	if got.CommentCount != 2 {
		t.Errorf("comment_count = %d, want 2 approved non-deleted comments", got.CommentCount)
	}
	if got.LikeCount != 1 {
		t.Errorf("like_count = %d, want 1", got.LikeCount)
	}
	if len(got.Assets) != 1 || got.Assets[0].StorageKey != "photos/counts.jpg" {
		t.Errorf("assets = %+v", got.Assets)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "海边" {
		t.Errorf("tags = %v", got.Tags)
	}
}

// TestFeedQueryCountIsConstant is the N+1 guard: the statement count must not
// grow with the number of posts on the page.
func TestFeedQueryCountIsConstant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: fmt.Sprintf("少量 %d", i)}, testNow); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	f.mirrorPosts(t)
	small := f.measure(t, func() { mustFeed(t, ctx, f.counted) })

	for i := 0; i < 27; i++ {
		if _, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeMoment, Status: StatusPublished, Body: fmt.Sprintf("大量 %d", i)}, testNow); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// Mirror the extra rows so both measured runs sit on the same schema.
	if _, err := f.counted.Exec(`DELETE FROM posts`); err != nil {
		t.Fatalf("clear counted posts: %v", err)
	}
	f.mirrorPosts(t)
	large := f.measure(t, func() { mustFeed(t, ctx, f.counted) })

	if large != small {
		t.Fatalf("feed used %d statements for 3 posts and %d for 30: the count grows with page size (N+1)", small, large)
	}
	t.Logf("feed issued %d statements for both 3 and 30 posts", small)
}

func mustFeed(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	page, err := Feed(ctx, db, FeedOptions{Limit: MaxPageSize})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(page.Posts) == 0 {
		t.Fatal("feed returned no posts")
	}
}

func (f *testFixture) measure(t *testing.T, run func()) int64 {
	t.Helper()
	atomic.StoreInt64(f.queries, 0)
	run()
	return atomic.LoadInt64(f.queries)
}

// ---------- detail ----------

func TestBySlugReturnsPublishedPostWithRelations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assetID := f.insertAsset(t, "photos/detail.jpg")

	post, err := Create(ctx, f.db, f.ownerID, Input{
		Type: TypeArticle, Status: StatusPublished, Title: "详情页", Body: "**正文**",
		AssetIDs: []int64{assetID}, Tags: []string{"写作", "Go"},
	}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := Delete(ctx, f.db, post.ID, testNow); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Deleted posts are invisible even by slug.
	if _, err := BySlug(ctx, f.db, post.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted detail error = %v, want ErrNotFound", err)
	}

	fresh, err := Create(ctx, f.db, f.ownerID, Input{
		Type: TypeArticle, Status: StatusPublished, Title: "详情页二", Body: "**正文**",
		AssetIDs: []int64{assetID}, Tags: []string{"写作", "Go"},
	}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := BySlug(ctx, f.db, fresh.Slug)
	if err != nil {
		t.Fatalf("BySlug: %v", err)
	}
	if got.ID != fresh.ID || got.Title != "详情页二" {
		t.Errorf("detail = %+v", got)
	}
	if !strings.Contains(got.BodyHTML, "<strong>正文</strong>") {
		t.Errorf("body_html = %s", got.BodyHTML)
	}
	if strings.Join(got.Tags, ",") != "Go,写作" {
		t.Errorf("tags = %v, want the alphabetical order", got.Tags)
	}
	if len(got.Assets) != 1 {
		t.Errorf("assets = %+v", got.Assets)
	}

	if _, err := BySlug(ctx, f.db, "no-such-slug"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown slug error = %v, want ErrNotFound", err)
	}
}

func TestBySlugHidesDrafts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	draft, err := Create(ctx, f.db, f.ownerID, Input{Type: TypeArticle, Status: StatusDraft, Title: "草稿", Body: "正文"}, testNow)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := BySlug(ctx, f.db, draft.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("draft detail error = %v, want ErrNotFound", err)
	}
}

// ---------- helpers ----------

func assertValidation(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s validation error, got nil", wantCode)
	}
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v (%T), want *ValidationError", err, err)
	}
	if invalid.Code != wantCode {
		t.Fatalf("code = %q, want %q (%s)", invalid.Code, wantCode, invalid.Message)
	}
	if invalid.Message == "" {
		t.Errorf("validation error %q has no user-facing message", invalid.Code)
	}
}

func insertComment(t *testing.T, f *testFixture, postID, userID int64, status string, deleted bool) {
	t.Helper()
	stamp := testNow.Format(time.RFC3339)
	var deletedAt any
	if deleted {
		deletedAt = stamp
	}
	if _, err := f.db.Exec(
		`INSERT INTO comments(post_id, user_id, body, status, created_at, updated_at, deleted_at) VALUES(?,?,?,?,?,?,?)`,
		postID, userID, "评论", status, stamp, stamp, deletedAt); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
}

func slugs(posts []Post) []string {
	out := make([]string, len(posts))
	for i, post := range posts {
		out[i] = post.Slug
	}
	return out
}
