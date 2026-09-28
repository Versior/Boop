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

	"boop/internal/secretbox"
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
		SiteAvatarURL:             "",
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
		"site.avatar_url":             `""`,
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

func TestLoadTxReadsThroughTheCallersTransaction(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Seed(ctx, db); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	// An uncommitted change is only visible through this transaction, so seeing
	// it proves the read went through the caller's transaction rather than a
	// separate connection. (The pool holds one connection, so a read through db
	// while this transaction is open could not even return.)
	if _, err := tx.ExecContext(ctx,
		`UPDATE settings SET value_json = 'true' WHERE key = ?`, KeyCommentsModerationEnabled); err != nil {
		t.Fatalf("update inside the transaction: %v", err)
	}

	values, err := LoadTx(ctx, tx)
	if err != nil {
		t.Fatalf("LoadTx: %v", err)
	}
	if !values.CommentsModerationEnabled {
		t.Error("LoadTx did not see the transaction's own uncommitted value")
	}
	if values.SiteName != Defaults().SiteName {
		t.Errorf("SiteName = %q, want the documented default", values.SiteName)
	}
}

func TestLoadTxRejectsCorruptStoredValue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := Seed(ctx, db); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET value_json = '"yes"' WHERE key = ?`, KeyCommentsEnabled); err != nil {
		t.Fatalf("corrupt value: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := LoadTx(ctx, tx); err == nil {
		t.Fatal("LoadTx accepted a corrupt stored value")
	}
	if _, err := LoadTx(ctx, nil); err == nil {
		t.Fatal("LoadTx accepted a nil transaction")
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
		{"avatar empty", KeySiteAvatarURL, "", false},
		{"avatar https", KeySiteAvatarURL, "https://cdn.example.com/a.png", false},
		{"avatar http", KeySiteAvatarURL, "http://127.0.0.1:8080/avatar.png", false},
		{"avatar with query", KeySiteAvatarURL, "https://cdn.example.com/a.png?v=2", false},
		{"avatar at upper bound", KeySiteAvatarURL, "https://example.com/" + strings.Repeat("a", maxAvatarURLRunes-len("https://example.com/")), false},
		{"avatar too long", KeySiteAvatarURL, "https://example.com/" + strings.Repeat("a", maxAvatarURLRunes), true},
		{"avatar relative", KeySiteAvatarURL, "/a.png", true},
		{"avatar wrong scheme", KeySiteAvatarURL, "javascript:alert(1)", true},
		{"avatar data url", KeySiteAvatarURL, "data:image/png;base64,AAAA", true},
		{"avatar without host", KeySiteAvatarURL, "https://", true},
		{"avatar with credentials", KeySiteAvatarURL, "https://user:pass@example.com/a.png", true},
		{"avatar wrong type", KeySiteAvatarURL, 12, true},
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

// ---------- secret settings ----------

// testBox builds a master key box; the fill byte decides which key it is.
func testBox(t *testing.T, fill byte) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeyBytes)
	for i := range key {
		key[i] = fill
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func secretRow(t *testing.T, db *sql.DB, key string) (nonce, ciphertext []byte, ok bool) {
	t.Helper()
	err := db.QueryRow(`SELECT nonce, ciphertext FROM secret_settings WHERE key = ?`, key).Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false
	}
	if err != nil {
		t.Fatalf("read secret row: %v", err)
	}
	return nonce, ciphertext, true
}

func TestApplyRequiresTheMasterKeyForSecretWrites(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := Apply(ctx, db, nil, Update{Secrets: map[string]string{SecretKeyAIAPIKey: "sk-x"}}); !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("secret write without a master key: error = %v, want ErrMasterKeyRequired", err)
	}
	if _, _, ok := secretRow(t, db, SecretKeyAIAPIKey); ok {
		t.Error("a refused secret write stored a row")
	}

	// Non-secret values stay writable without a master key.
	if err := Apply(ctx, db, nil, Update{Values: map[string]any{KeySiteName: "没有主密钥"}}); err != nil {
		t.Fatalf("value write without a master key: %v", err)
	}
	values, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if values.SiteName != "没有主密钥" {
		t.Errorf("SiteName = %q", values.SiteName)
	}
}

// TestApplyClearsSecretsWithoutTheMasterKey pins the documented rule: deleting a
// secret only removes a row, so it needs no decryption and must stay available
// (and idempotent) even when BOOP_MASTER_KEY is missing.
func TestApplyClearsSecretsWithoutTheMasterKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 6)

	if err := Apply(ctx, db, box, Update{Secrets: map[string]string{SecretKeyGitHubClientSecret: "stored-value"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := Apply(ctx, db, nil, Update{Clear: []string{SecretKeyGitHubClientSecret}}); err != nil {
		t.Fatalf("clear without a master key: %v", err)
	}
	if _, _, ok := secretRow(t, db, SecretKeyGitHubClientSecret); ok {
		t.Error("the cleared secret is still stored")
	}
	// Clearing again without a master key is still harmless.
	if err := Apply(ctx, db, nil, Update{Clear: []string{SecretKeyGitHubClientSecret}}); err != nil {
		t.Errorf("second clear: %v", err)
	}
	// A request that writes a new secret is a secret write as a whole: the
	// missing master key fails the whole update, including its clear list.
	if err := Apply(ctx, db, box, Update{Secrets: map[string]string{SecretKeyAIAPIKey: "sk-ai"}}); err != nil {
		t.Fatalf("store the other secret: %v", err)
	}
	err := Apply(ctx, db, nil, Update{
		Values:  map[string]any{KeySiteName: "混合写入"},
		Secrets: map[string]string{SecretKeyGitHubClientSecret: "new-value"},
		Clear:   []string{SecretKeyAIAPIKey},
	})
	if !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("mixed update: error = %v, want ErrMasterKeyRequired", err)
	}
	if _, _, ok := secretRow(t, db, SecretKeyAIAPIKey); !ok {
		t.Error("the refused update deleted a secret")
	}
	values, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if values.SiteName != Defaults().SiteName {
		t.Errorf("SiteName = %q, want the refused update to leave it alone", values.SiteName)
	}
}

// TestApplyStoresSecretsEncryptedAndReadsThemBack is the round trip the
// encrypted settings depend on: only the ciphertext reaches the database, and
// only the matching master key can read it again.
func TestApplyStoresSecretsEncryptedAndReadsThemBack(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 7)
	secret := "gho_client-secret-value"

	if err := Apply(ctx, db, box, Update{
		Values:  map[string]any{KeySiteName: "加密站点"},
		Secrets: map[string]string{SecretKeyGitHubClientSecret: secret, SecretKeyAIAPIKey: "sk-ai"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	nonce, ciphertext, ok := secretRow(t, db, SecretKeyGitHubClientSecret)
	if !ok {
		t.Fatal("the secret row is missing")
	}
	if len(nonce) != secretbox.NonceBytes {
		t.Errorf("nonce = %d bytes, want %d", len(nonce), secretbox.NonceBytes)
	}
	if string(ciphertext) == secret || strings.Contains(string(ciphertext), secret) {
		t.Error("the stored ciphertext contains the plaintext")
	}

	stored, err := ReadSecret(ctx, db, box, SecretKeyGitHubClientSecret)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if stored != secret {
		t.Errorf("ReadSecret = %q, want %q", stored, secret)
	}
	// The same transaction also wrote the plain value.
	values, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if values.SiteName != "加密站点" {
		t.Errorf("SiteName = %q", values.SiteName)
	}

	// A second write replaces the value and the nonce.
	if err := Apply(ctx, db, box, Update{Secrets: map[string]string{SecretKeyGitHubClientSecret: "rotated"}}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if after, _, _ := secretRow(t, db, SecretKeyGitHubClientSecret); string(after) == string(nonce) {
		t.Error("the nonce was reused for a new value")
	}
	rotated, err := ReadSecret(ctx, db, box, SecretKeyGitHubClientSecret)
	if err != nil || rotated != "rotated" {
		t.Fatalf("ReadSecret after rotation = %q, %v", rotated, err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM secret_settings WHERE key = ?`, SecretKeyGitHubClientSecret).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows = %d, want 1 (upsert)", rows)
	}
}

func TestApplyRejectsBadSecretsAndWritesNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 3)

	tests := []struct {
		name   string
		update Update
	}{
		{"empty value", Update{Secrets: map[string]string{SecretKeyAIAPIKey: ""}}},
		{"oversized value", Update{Secrets: map[string]string{SecretKeyAIAPIKey: strings.Repeat("k", maxSecretBytes+1)}}},
		{"unknown secret", Update{Secrets: map[string]string{"nope.secret": "x"}}},
		{"unknown clear target", Update{Clear: []string{"nope.secret"}}},
		{"invalid value beside a valid one", Update{
			Values:  map[string]any{KeySiteName: "合法", KeyContentPageSize: 99},
			Secrets: map[string]string{SecretKeyAIAPIKey: "sk-x"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Apply(ctx, db, box, tt.update); !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("error = %v, want ErrInvalidValue", err)
			}
			var settingsRows, secretRows int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&settingsRows); err != nil {
				t.Fatalf("count settings: %v", err)
			}
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM secret_settings`).Scan(&secretRows); err != nil {
				t.Fatalf("count secrets: %v", err)
			}
			if settingsRows != 0 || secretRows != 0 {
				t.Errorf("rows written: settings=%d secrets=%d, want none", settingsRows, secretRows)
			}
		})
	}
}

func TestApplyClearsOnlyTheNamedSecret(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 5)

	if err := Apply(ctx, db, box, Update{Secrets: map[string]string{
		SecretKeyGitHubClientSecret: "secret-value",
		SecretKeyAIAPIKey:           "sk-ai",
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := Apply(ctx, db, box, Update{Clear: []string{SecretKeyGitHubClientSecret}}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, _, ok := secretRow(t, db, SecretKeyGitHubClientSecret); ok {
		t.Error("the cleared secret is still stored")
	}
	if _, err := ReadSecret(ctx, db, box, SecretKeyGitHubClientSecret); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("ReadSecret after clear: error = %v, want ErrSecretNotFound", err)
	}
	if _, err := ReadSecret(ctx, db, box, SecretKeyAIAPIKey); err != nil {
		t.Errorf("clearing one secret removed another: %v", err)
	}
	// Clearing an absent secret is harmless.
	if err := Apply(ctx, db, box, Update{Clear: []string{SecretKeyGitHubClientSecret}}); err != nil {
		t.Errorf("second clear: %v", err)
	}
}

func TestReadSecretFailures(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 1)
	other := testBox(t, 2)

	if _, err := ReadSecret(ctx, nil, box, SecretKeyAIAPIKey); err == nil {
		t.Error("ReadSecret accepted a nil database")
	}
	if _, err := ReadSecret(ctx, db, box, "nope.secret"); !errors.Is(err, ErrUnknownSecretKey) {
		t.Errorf("unknown key: error = %v, want ErrUnknownSecretKey", err)
	}
	if _, err := ReadSecret(ctx, db, nil, SecretKeyAIAPIKey); !errors.Is(err, ErrMasterKeyRequired) {
		t.Errorf("nil box: error = %v, want ErrMasterKeyRequired", err)
	}
	if _, err := ReadSecret(ctx, db, box, SecretKeyAIAPIKey); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("missing row: error = %v, want ErrSecretNotFound", err)
	}

	if err := Apply(ctx, db, box, Update{Secrets: map[string]string{SecretKeyAIAPIKey: "sk-ai"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := ReadSecret(ctx, db, other, SecretKeyAIAPIKey); err == nil {
		t.Error("another master key decrypted the secret")
	} else if strings.Contains(err.Error(), "sk-ai") {
		t.Errorf("the failure message leaks the plaintext: %v", err)
	}

	// A tampered row fails the same way instead of returning garbage.
	if _, err := db.ExecContext(ctx, `UPDATE secret_settings SET ciphertext = x'00' WHERE key = ?`, SecretKeyAIAPIKey); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := ReadSecret(ctx, db, box, SecretKeyAIAPIKey); err == nil {
		t.Error("a tampered row was accepted")
	}
}

// TestReadSecretRejectsASwappedCiphertext is the AAD invariant: every stored
// secret is bound to its own setting key, so copying the nonce and ciphertext of
// one key onto another key can never decrypt — neither side returns plaintext.
func TestReadSecretRejectsASwappedCiphertext(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 8)

	if err := Apply(ctx, db, box, Update{Secrets: map[string]string{
		SecretKeyGitHubClientSecret: "github-secret-value",
		SecretKeyAIAPIKey:           "sk-ai-api-key-value",
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Both rows are valid before the swap: this is the state a working site has.
	for key, want := range map[string]string{
		SecretKeyGitHubClientSecret: "github-secret-value",
		SecretKeyAIAPIKey:           "sk-ai-api-key-value",
	} {
		if got, err := ReadSecret(ctx, db, box, key); err != nil || got != want {
			t.Fatalf("ReadSecret(%s) = %q, %v before the swap", key, got, err)
		}
	}

	// Swap the two rows, exactly as copying the columns between keys would.
	firstNonce, firstCipher, ok := secretRow(t, db, SecretKeyGitHubClientSecret)
	if !ok {
		t.Fatal("the github row is missing")
	}
	secondNonce, secondCipher, ok := secretRow(t, db, SecretKeyAIAPIKey)
	if !ok {
		t.Fatal("the ai row is missing")
	}
	storeRow := func(key string, nonce, ciphertext []byte) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`UPDATE secret_settings SET nonce = ?, ciphertext = ? WHERE key = ?`, nonce, ciphertext, key); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
	}
	storeRow(SecretKeyGitHubClientSecret, secondNonce, secondCipher)
	storeRow(SecretKeyAIAPIKey, firstNonce, firstCipher)

	for _, key := range []string{SecretKeyGitHubClientSecret, SecretKeyAIAPIKey} {
		value, err := ReadSecret(ctx, db, box, key)
		if err == nil {
			t.Fatalf("ReadSecret(%s) = %q, want a decryption failure", key, value)
		}
		if !errors.Is(err, secretbox.ErrCiphertext) {
			t.Errorf("ReadSecret(%s): error = %v, want a cipher failure", key, err)
		}
		for _, plaintext := range []string{"github-secret-value", "sk-ai-api-key-value"} {
			if strings.Contains(err.Error(), plaintext) {
				t.Errorf("ReadSecret(%s) leaks the plaintext: %v", key, err)
			}
		}
	}
}

func TestConfiguredSecretsReportsEveryDocumentedKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 9)

	configured, err := ConfiguredSecrets(ctx, db)
	if err != nil {
		t.Fatalf("ConfiguredSecrets: %v", err)
	}
	for _, key := range []string{SecretKeyGitHubClientID, SecretKeyGitHubClientSecret, SecretKeyAIAPIKey} {
		if value, ok := configured[key]; !ok || value {
			t.Errorf("%s = %v, %v, want a reported false", key, value, ok)
		}
	}

	if err := Apply(ctx, db, box, Update{Secrets: map[string]string{SecretKeyGitHubClientID: "client-id"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// A row for a key the schema does not document is ignored rather than fatal.
	if _, err := db.ExecContext(ctx, `INSERT INTO secret_settings(key, nonce, ciphertext, updated_at) VALUES('legacy.key', x'00', x'00', '2026-09-28T00:00:00Z')`); err != nil {
		t.Fatalf("insert unknown secret: %v", err)
	}

	configured, err = ConfiguredSecrets(ctx, db)
	if err != nil {
		t.Fatalf("second ConfiguredSecrets: %v", err)
	}
	if !configured[SecretKeyGitHubClientID] {
		t.Error("the stored client id is not reported as configured")
	}
	if configured[SecretKeyAIAPIKey] {
		t.Error("an unstored secret is reported as configured")
	}
	if _, ok := configured["legacy.key"]; ok {
		t.Error("an undocumented key appeared in the report")
	}
	// The report never needs the master key: it only checks for rows.
	if _, err := ConfiguredSecrets(ctx, nil); err == nil {
		t.Error("ConfiguredSecrets accepted a nil database")
	}
}
