package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func pragmaValue(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var value string
	if err := db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

func TestOpenCreatesDataDirectoryAndPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "boop.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
	if got := pragmaValue(t, db, "foreign_keys"); got != "1" {
		t.Errorf("foreign_keys = %q, want 1", got)
	}
	if got := pragmaValue(t, db, "journal_mode"); !strings.EqualFold(got, "wal") {
		t.Errorf("journal_mode = %q, want wal", got)
	}
	if got := pragmaValue(t, db, "busy_timeout"); got != "5000" {
		t.Errorf("busy_timeout = %q, want 5000", got)
	}
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1 for the single-process deployment", got)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded, want an error")
	}
}

func TestMigrateCreatesDocumentedSchema(t *testing.T) {
	db := migratedDB(t)

	tables := []string{
		"schema_migrations", "users", "oauth_accounts", "sessions", "posts", "assets",
		"post_assets", "tags", "post_tags", "comments", "likes", "bookmarks",
		"settings", "secret_settings", "ai_cache", "post_search",
	}
	for _, name := range tables {
		var kind string
		err := db.QueryRow(`SELECT type FROM sqlite_master WHERE name = ?`, name).Scan(&kind)
		if err == sql.ErrNoRows {
			t.Errorf("table %s is missing", name)
			continue
		}
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if kind != "table" {
			t.Errorf("%s is a %s, want table", name, kind)
		}
	}

	indexes := []string{
		"idx_sessions_expires", "idx_posts_feed", "idx_posts_type_feed",
		"idx_post_assets_order", "idx_comments_post", "idx_comments_queue", "idx_bookmarks_user",
	}
	for _, name := range indexes {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect index %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("index %s is missing", name)
		}
	}

	triggers := []string{"posts_search_insert", "posts_search_update", "posts_search_delete"}
	for _, name := range triggers {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect trigger %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("trigger %s is missing", name)
		}
	}

	if got := LatestVersion(); got != 2 {
		t.Errorf("LatestVersion() = %d, want 2", got)
	}
	version, err := CurrentVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if version != LatestVersion() {
		t.Errorf("CurrentVersion() = %d, want %d", version, LatestVersion())
	}

	var appliedAt string
	if err := db.QueryRow(`SELECT applied_at FROM schema_migrations ORDER BY version LIMIT 1`).Scan(&appliedAt); err != nil {
		t.Fatalf("read applied_at: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, appliedAt); err != nil {
		t.Errorf("applied_at %q is not RFC3339: %v", appliedAt, err)
	}
}

func TestMigrateIsIdempotentAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boop.db")
	ctx := context.Background()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate on the same handle: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES('owner@example.com','站长','owner', strftime('%Y-%m-%dT%H:%M:%SZ','now'), strftime('%Y-%m-%dT%H:%M:%SZ','now'))`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	restarted, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer restarted.Close()
	if err := Migrate(restarted); err != nil {
		t.Fatalf("Migrate after restart: %v", err)
	}

	var rows int
	if err := restarted.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if rows != LatestVersion() {
		t.Errorf("schema_migrations rows = %d, want %d", rows, LatestVersion())
	}
	var users int
	if err := restarted.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("users = %d, want the surviving row", users)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	stamp := time.Now().UTC().Format(time.RFC3339)
	insert := `INSERT INTO users(email, display_name, role, created_at, updated_at) VALUES(?, '甲', 'reader', ?, ?)`
	if _, err := db.ExecContext(ctx, insert, "dup@example.com", stamp, stamp); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := db.ExecContext(ctx, insert, "dup@example.com", stamp, stamp)
	if err == nil {
		t.Fatal("duplicate insert succeeded")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("IsUniqueViolation(%v) = false, want true", err)
	}
	if IsUniqueViolation(errors.New("not a driver error")) {
		t.Error("IsUniqueViolation accepted an unrelated error")
	}

	// A CHECK violation is a constraint failure but not a UNIQUE one.
	_, err = db.ExecContext(ctx, `INSERT INTO posts(slug, type, created_at, updated_at) VALUES('bad-type', 'video', ?, ?)`, stamp, stamp)
	if err == nil {
		t.Fatal("inserting an unsupported post type succeeded")
	}
	if IsUniqueViolation(err) {
		t.Error("IsUniqueViolation accepted a CHECK violation")
	}
}

func TestKeyColumnsRejectNullKeys(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	stamp := time.Now().UTC().Format(time.RFC3339)

	cases := []struct {
		table  string
		column string
		insert string
	}{
		{
			table:  "settings",
			column: "key",
			insert: `INSERT INTO settings(key, value_json, updated_at) VALUES(NULL, '1', ?)`,
		},
		{
			table:  "secret_settings",
			column: "key",
			insert: `INSERT INTO secret_settings(key, nonce, ciphertext, updated_at) VALUES(NULL, x'00', x'00', ?)`,
		},
		{
			table:  "ai_cache",
			column: "cache_key",
			insert: `INSERT INTO ai_cache(cache_key, value_json, source_updated_at, generated_at, expires_at) VALUES(NULL, '1', ?, ?, ?)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			args := make([]any, 0, 4)
			for i := 0; i < strings.Count(tc.insert, "?"); i++ {
				args = append(args, stamp)
			}
			if _, err := db.ExecContext(ctx, tc.insert, args...); err == nil {
				t.Fatalf("inserting a NULL %s succeeded, want a NOT NULL failure", tc.column)
			}

			var rows int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table).Scan(&rows); err != nil {
				t.Fatalf("count %s: %v", tc.table, err)
			}
			if rows != 0 {
				t.Errorf("%s holds %d rows after a rejected insert, want 0", tc.table, rows)
			}
		})
	}
}

func TestMigrateRejectsNilDatabase(t *testing.T) {
	if err := Migrate(nil); err == nil {
		t.Fatal("Migrate(nil) succeeded, want an error")
	}
}

func TestForeignKeyEnforcement(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()

	now := "2026-09-28T00:00:00Z"
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES(1,'reader@example.com','读者','reader',?,?)`, now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO posts(id, slug, type, status, title, body_markdown, created_at, updated_at)
		VALUES(1,'hello','article','published','Hello','body',?,?)`, now, now); err != nil {
		t.Fatalf("insert post: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO assets(id, owner_user_id, storage_key, original_name, mime_type, size_bytes, sha256, created_at)
		VALUES(1,1,'2026/09/a.jpg','a.jpg','image/jpeg',10,x'00',?)`, now); err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO post_assets(post_id, asset_id) VALUES(999, 1)`); err == nil {
		t.Error("post_assets accepted an unknown post")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO comments(post_id, user_id, body, status, created_at, updated_at)
		VALUES(999, 1, 'hi', 'approved', ?, ?)`, now, now); err == nil {
		t.Error("comments accepted an unknown post")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO post_assets(post_id, asset_id) VALUES(1, 1)`); err != nil {
		t.Fatalf("valid post_assets row rejected: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM assets WHERE id = 1`); err == nil {
		t.Error("assets referenced by post_assets was deleted (expected ON DELETE RESTRICT)")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = 1`); err == nil {
		t.Error("a user owning an asset was deleted (assets.owner_user_id has no cascade)")
	}

	// A user without owned assets is removed, cascading sessions and comments.
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES(2,'reader2@example.com','读者二','reader',?,?)`, now, now); err != nil {
		t.Fatalf("insert second user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions(user_id, token_hash, csrf_token_hash, expires_at, created_at, last_seen_at)
		VALUES(2, x'11', x'12', ?, ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO comments(post_id, user_id, body, status, created_at, updated_at)
		VALUES(1, 2, 'nice', 'approved', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert comment: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = 2`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var sessions, comments int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions after user delete = %d, want 0 (ON DELETE CASCADE)", sessions)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM comments`).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if comments != 0 {
		t.Errorf("comments after user delete = %d, want 0 (ON DELETE CASCADE)", comments)
	}
}

func TestPostConstraintsMatchDocumentation(t *testing.T) {
	db := migratedDB(t)
	now := "2026-09-28T00:00:00Z"

	if _, err := db.Exec(`INSERT INTO posts(slug, type, status, created_at, updated_at) VALUES('a','video','published',?,?)`, now, now); err == nil {
		t.Error("posts accepted an unknown type")
	}
	if _, err := db.Exec(`INSERT INTO posts(slug, type, status, created_at, updated_at) VALUES('b','moment','live',?,?)`, now, now); err == nil {
		t.Error("posts accepted an unknown status")
	}
	if _, err := db.Exec(`INSERT INTO posts(slug, type, status, created_at, updated_at) VALUES('c','moment','draft',?,?)`, now, now); err != nil {
		t.Fatalf("valid post rejected: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO posts(slug, type, status, created_at, updated_at) VALUES('c','moment','draft',?,?)`, now, now); err == nil {
		t.Error("posts accepted a duplicate slug")
	}
	if _, err := db.Exec(`INSERT INTO users(email, display_name, role, created_at, updated_at) VALUES('x@example.com','x','admin',?,?)`, now, now); err == nil {
		t.Error("users accepted an unknown role")
	}
	if _, err := db.Exec(`INSERT INTO assets(owner_user_id, storage_key, original_name, mime_type, size_bytes, sha256, created_at)
		VALUES(1,'k','n','image/png',-1,x'00',?)`, now); err == nil {
		t.Error("assets accepted a negative size_bytes")
	}
}

func TestFTSSyncFollowsPostChanges(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()
	now := "2026-09-28T00:00:00Z"

	searchCount := func(term string) int {
		t.Helper()
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM post_search WHERE post_search MATCH ?`, term).Scan(&count); err != nil {
			t.Fatalf("search %q: %v", term, err)
		}
		return count
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO posts(id, slug, type, status, title, body_markdown, excerpt, created_at, updated_at)
		VALUES(1,'coastline','photo','published','Coastline in the mist','fog rolls over the sea','雾海',?,?)`, now, now); err != nil {
		t.Fatalf("insert post: %v", err)
	}
	if got := searchCount("coastline"); got != 1 {
		t.Errorf("title match = %d, want 1", got)
	}
	if got := searchCount("sea"); got != 1 {
		t.Errorf("body match = %d, want 1", got)
	}

	if _, err := db.ExecContext(ctx, `UPDATE posts SET body_markdown = 'city lights at night' WHERE id = 1`); err != nil {
		t.Fatalf("update body: %v", err)
	}
	if got := searchCount("sea"); got != 0 {
		t.Errorf("stale body term still matches: %d", got)
	}
	if got := searchCount("lights"); got != 1 {
		t.Errorf("updated body term = %d, want 1", got)
	}

	if _, err := db.ExecContext(ctx, `UPDATE posts SET title = 'Harbour lights' WHERE id = 1`); err != nil {
		t.Fatalf("update title: %v", err)
	}
	if got := searchCount("coastline"); got != 0 {
		t.Errorf("stale title term still matches: %d", got)
	}
	if got := searchCount("harbour"); got != 1 {
		t.Errorf("updated title term = %d, want 1", got)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM posts WHERE id = 1`); err != nil {
		t.Fatalf("delete post: %v", err)
	}
	if got := searchCount("harbour"); got != 0 {
		t.Errorf("deleted post still matches: %d", got)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO post_search(post_search) VALUES('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity check failed: %v", err)
	}
}

func TestReadyReflectsMigrationState(t *testing.T) {
	ctx := context.Background()

	unmigrated := openTestDB(t)
	if err := Ready(ctx, unmigrated); err == nil {
		t.Error("Ready succeeded on an unmigrated database")
	}

	migrated := migratedDB(t)
	if err := Ready(ctx, migrated); err != nil {
		t.Errorf("Ready on a migrated database: %v", err)
	}

	if err := Ready(ctx, nil); err == nil {
		t.Error("Ready(nil) succeeded, want an error")
	}
}
