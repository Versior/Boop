package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boop/internal/store"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

func TestDefaultsMatchDocumentedValues(t *testing.T) {
	got := Defaults()
	want := Values{
		SiteName:                  "Boop",
		SiteDescription:           "遇事开心的个人博客",
		SiteTimezone:              "Asia/Shanghai",
		PageSize:                  20,
		RegistrationEnabled:       true,
		CommentsEnabled:           true,
		CommentsModerationEnabled: false,
		AIEnabled:                 false,
		AIBaseURL:                 "",
		AIChatModel:               "",
		AIEmbeddingModel:          "",
		AIAuthorStatusTTLHours:    168,
	}
	if got != want {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
}

func TestSeedWritesEveryDocumentedKeyOnce(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Seed(ctx, db); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if err := Seed(ctx, db); err != nil {
		t.Fatalf("second Seed: %v", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT key, value_json FROM settings ORDER BY key`)
	if err != nil {
		t.Fatalf("query settings: %v", err)
	}
	defer rows.Close()

	stored := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stored[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	want := map[string]string{
		"site.name":                   `"Boop"`,
		"site.description":            `"遇事开心的个人博客"`,
		"site.timezone":               `"Asia/Shanghai"`,
		"content.page_size":           `20`,
		"auth.registration_enabled":   `true`,
		"comments.enabled":            `true`,
		"comments.moderation_enabled": `false`,
		"ai.enabled":                  `false`,
		"ai.base_url":                 `""`,
		"ai.chat_model":               `""`,
		"ai.embedding_model":          `""`,
		"ai.author_status_ttl_hours":  `168`,
	}
	if len(stored) != len(want) {
		t.Errorf("settings rows = %d, want %d: %v", len(stored), len(want), stored)
	}
	for key, value := range want {
		got, ok := stored[key]
		if !ok {
			t.Errorf("settings row %s is missing", key)
			continue
		}
		if got != value {
			t.Errorf("settings[%s] = %s, want %s", key, got, value)
		}
	}
}

func TestSeedKeepsExistingValues(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Seed(ctx, db); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if err := Set(ctx, db, KeySiteName, "少爷的博客"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Seed(ctx, db); err != nil {
		t.Fatalf("Seed after change: %v", err)
	}

	values, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if values.SiteName != "少爷的博客" {
		t.Errorf("SiteName = %q, want the stored value", values.SiteName)
	}
}

func TestLoadAppliesStoredValuesOverDefaults(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	values, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load on an unseeded database: %v", err)
	}
	if values != Defaults() {
		t.Errorf("Load() = %+v, want defaults %+v", values, Defaults())
	}

	if err := Set(ctx, db, KeyContentPageSize, 50); err != nil {
		t.Fatalf("Set page size: %v", err)
	}
	if err := Set(ctx, db, KeyCommentsModerationEnabled, true); err != nil {
		t.Fatalf("Set moderation: %v", err)
	}
	if err := Set(ctx, db, KeySiteTimezone, "UTC"); err != nil {
		t.Fatalf("Set timezone: %v", err)
	}

	values, err = Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if values.PageSize != 50 {
		t.Errorf("PageSize = %d, want 50", values.PageSize)
	}
	if !values.CommentsModerationEnabled {
		t.Error("CommentsModerationEnabled = false, want true")
	}
	if values.SiteTimezone != "UTC" {
		t.Errorf("SiteTimezone = %q, want UTC", values.SiteTimezone)
	}
	if values.RegistrationEnabled != true {
		t.Error("RegistrationEnabled changed unexpectedly")
	}
}

func TestLoadRejectsCorruptStoredValue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Seed(ctx, db); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET value_json = '{"nested":1}' WHERE key = ?`, KeyContentPageSize); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	_, err := Load(ctx, db)
	if err == nil {
		t.Fatal("Load succeeded with a corrupt value")
	}
	if !strings.Contains(err.Error(), KeyContentPageSize) {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestLoadIgnoresUnknownKeys(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `INSERT INTO settings(key, value_json, updated_at) VALUES('future.key', '123', '2026-09-28T00:00:00Z')`); err != nil {
		t.Fatalf("insert unknown key: %v", err)
	}
	if _, err := Load(ctx, db); err != nil {
		t.Fatalf("Load with an unknown key: %v", err)
	}
}

func TestSetValidatesKnownKeys(t *testing.T) {
	ctx := context.Background()
	// One migrated database for the whole table: every case compares the row
	// before and after its own write, so cases stay independent.
	db := testDB(t)
	if err := Seed(ctx, db); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	longRunes := func(n int) string { return strings.Repeat("字", n) }

	tests := []struct {
		name    string
		key     string
		value   any
		wantErr bool
	}{
		{"page size at lower bound", KeyContentPageSize, 1, false},
		{"page size at upper bound", KeyContentPageSize, 50, false},
		{"page size zero", KeyContentPageSize, 0, true},
		{"page size above upper bound", KeyContentPageSize, 51, true},
		{"page size negative", KeyContentPageSize, -1, true},
		{"page size as string", KeyContentPageSize, "20", true},
		{"page size as float", KeyContentPageSize, 20.0, true},
		{"ttl at lower bound", KeyAIAuthorStatusTTLHours, 1, false},
		{"ttl at upper bound", KeyAIAuthorStatusTTLHours, 2160, false},
		{"ttl zero", KeyAIAuthorStatusTTLHours, 0, true},
		{"ttl above upper bound", KeyAIAuthorStatusTTLHours, 2161, true},
		{"ttl as string", KeyAIAuthorStatusTTLHours, "168", true},
		{"boolean key with bool", KeyAIEnabled, true, false},
		{"boolean key with string", KeyAIEnabled, "true", true},
		{"boolean key with int", KeyCommentsEnabled, 1, true},
		{"boolean key with nil", KeyCommentsModerationEnabled, nil, true},
		{"site name", KeySiteName, "少爷的博客", false},
		{"site name at upper bound", KeySiteName, longRunes(80), false},
		{"site name empty", KeySiteName, "", true},
		{"site name blank", KeySiteName, "   ", true},
		{"site name too long", KeySiteName, longRunes(81), true},
		{"site name wrong type", KeySiteName, 5, true},
		{"description empty", KeySiteDescription, "", false},
		{"description at upper bound", KeySiteDescription, longRunes(280), false},
		{"description too long", KeySiteDescription, longRunes(281), true},
		{"description wrong type", KeySiteDescription, true, true},
		{"timezone utc", KeySiteTimezone, "UTC", false},
		{"timezone shanghai", KeySiteTimezone, "Asia/Shanghai", false},
		{"timezone empty", KeySiteTimezone, "", true},
		{"timezone unknown", KeySiteTimezone, "Mars/Olympus", true},
		{"timezone wrong type", KeySiteTimezone, 8, true},
		{"ai base url empty", KeyAIBaseURL, "", false},
		{"ai base url https", KeyAIBaseURL, "https://api.example.com/v1", false},
		{"ai base url http", KeyAIBaseURL, "http://127.0.0.1:11434/v1", false},
		{"ai base url relative", KeyAIBaseURL, "/v1", true},
		{"ai base url wrong scheme", KeyAIBaseURL, "ftp://example.com", true},
		{"ai base url with userinfo", KeyAIBaseURL, "https://user:pass@example.com/v1", true},
		{"ai base url with query", KeyAIBaseURL, "https://example.com/v1?key=1", true},
		{"ai base url with fragment", KeyAIBaseURL, "https://example.com/v1#frag", true},
		{"ai base url wrong type", KeyAIBaseURL, 12, true},
		{"model at upper bound", KeyAIChatModel, longRunes(200), false},
		{"model too long", KeyAIChatModel, longRunes(201), true},
		{"embedding model empty", KeyAIEmbeddingModel, "", false},
		{"embedding model too long", KeyAIEmbeddingModel, strings.Repeat("m", 201), true},
		{"embedding model wrong type", KeyAIEmbeddingModel, 3, true},
		{"unknown key", "site.nope", "x", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, beforeAt := storedRow(t, db, tt.key)

			err := Set(ctx, db, tt.key, tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Set(%s, %#v) succeeded, want error", tt.key, tt.value)
				}
				after, afterAt := storedRow(t, db, tt.key)
				if after != before || afterAt != beforeAt {
					t.Errorf("rejected value changed the row: %q@%s -> %q@%s", before, beforeAt, after, afterAt)
				}
				if _, err := Load(ctx, db); err != nil {
					t.Errorf("Load after a rejected write: %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Set(%s, %#v): %v", tt.key, tt.value, err)
			}
			encoded, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			after, _ := storedRow(t, db, tt.key)
			if after != string(encoded) {
				t.Errorf("stored %q, want %q", after, encoded)
			}
		})
	}
}

// storedRow reads the raw stored value of a key; a key with no row returns
// empty strings, which is also the state a rejected first write must leave
// behind.
func storedRow(t *testing.T, db *sql.DB, key string) (string, string) {
	t.Helper()
	var raw, updatedAt string
	err := db.QueryRow(`SELECT value_json, updated_at FROM settings WHERE key = ?`, key).Scan(&raw, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ""
	}
	if err != nil {
		t.Fatalf("read settings row: %v", err)
	}
	return raw, updatedAt
}

func TestSetRejectsUnknownKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Set(ctx, db, "site.nope", "x"); err == nil {
		t.Fatal("Set accepted an unknown key")
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("settings rows = %d, want none written", rows)
	}
}

func TestSetStoresJSONAndTimestamp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Set(ctx, db, KeyAIAuthorStatusTTLHours, 24); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Set(ctx, db, KeyAIBaseURL, "https://api.example.com/v1"); err != nil {
		t.Fatalf("Set base url: %v", err)
	}

	var raw, updatedAt string
	if err := db.QueryRowContext(ctx, `SELECT value_json, updated_at FROM settings WHERE key = ?`, KeyAIAuthorStatusTTLHours).Scan(&raw, &updatedAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if raw != "24" {
		t.Errorf("value_json = %q, want 24", raw)
	}
	if _, err := time.Parse(time.RFC3339, updatedAt); err != nil {
		t.Errorf("updated_at %q is not RFC3339: %v", updatedAt, err)
	}

	if err := Set(ctx, db, KeyAIAuthorStatusTTLHours, 48); err != nil {
		t.Fatalf("second Set: %v", err)
	}
	values, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if values.AIAuthorStatusTTLHours != 48 {
		t.Errorf("AIAuthorStatusTTLHours = %d, want 48", values.AIAuthorStatusTTLHours)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key = ?`, KeyAIAuthorStatusTTLHours).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows for the key = %d, want 1 (upsert)", rows)
	}
}
