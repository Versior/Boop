package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boop/internal/store"
)

func base64URLDecode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}

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

const goodPassword = "correct horse battery"

func newReader(t *testing.T, db *sql.DB, email, name string) User {
	t.Helper()
	user, err := CreateReader(context.Background(), db, email, name, goodPassword)
	if err != nil {
		t.Fatalf("CreateReader(%s): %v", email, err)
	}
	return user
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"trims and lowercases", "  Alice@Example.COM  ", "alice@example.com", false},
		{"plus addressing", "a+tag@example.com", "a+tag@example.com", false},
		{"dotted local part", "first.last@sub.example.co", "first.last@sub.example.co", false},
		{"empty", "", "", true},
		{"blank", "   ", "", true},
		{"no at sign", "alice.example.com", "", true},
		{"two at signs", "a@@example.com", "", true},
		{"empty local part", "@example.com", "", true},
		{"empty domain", "alice@", "", true},
		{"domain without dot", "alice@localhost", "", true},
		{"single letter tld", "alice@example.c", "", true},
		{"numeric tld", "alice@example.12", "", true},
		{"domain with space", "alice@exa mple.com", "", true},
		{"local part with space", "al ice@example.com", "", true},
		{"local part trailing dot", "alice.@example.com", "", true},
		{"domain label starts with hyphen", "alice@-example.com", "", true},
		{"domain label ends with hyphen", "alice@example-.com", "", true},
		{"domain empty label", "alice@example..com", "", true},
		{"non ascii local part", "阿力@example.com", "", true},
		{"credentials injection", "alice@example.com@evil.com", "", true},
		{"too long", strings.Repeat("a", 250) + "@example.com", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEmail(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeEmail(%q) = %q, want error", tt.raw, got)
				}
				if !errors.Is(err, ErrInvalidEmail) {
					t.Errorf("error = %v, want ErrInvalidEmail", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeEmail(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestValidatePasswordBytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"nine bytes", strings.Repeat("a", 9), true},
		{"ten bytes", strings.Repeat("a", 10), false},
		{"seventy two bytes", strings.Repeat("a", 72), false},
		{"seventy three bytes", strings.Repeat("a", 73), true},
		{"empty", "", true},
		// 24 CJK characters are exactly 72 bytes, so the limit counts bytes.
		{"multibyte at byte limit", strings.Repeat("遇", 24), false},
		{"multibyte over byte limit", strings.Repeat("遇", 25), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.input)
			if tt.wantErr && err == nil {
				t.Fatal("ValidatePassword accepted an invalid password")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidatePassword: %v", err)
			}
			if err != nil && !errors.Is(err, ErrInvalidPassword) {
				t.Errorf("error = %v, want ErrInvalidPassword", err)
			}
		})
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, goodPassword) {
		t.Fatal("hash contains the plaintext password")
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("hash = %q, want a bcrypt hash", hash)
	}
	if !VerifyPassword(hash, goodPassword) {
		t.Error("VerifyPassword rejected the correct password")
	}
	if VerifyPassword(hash, "correct horse batterz") {
		t.Error("VerifyPassword accepted a wrong password")
	}
	if VerifyPassword("not-a-hash", goodPassword) {
		t.Error("VerifyPassword accepted a malformed hash")
	}
	if VerifyPassword("", goodPassword) {
		t.Error("VerifyPassword accepted an empty hash")
	}
}

func TestDummyHashIsUsableAndMatchesNothing(t *testing.T) {
	if err := ValidatePassword("placeholder-for-timing"); err != nil {
		t.Fatalf("test password invalid: %v", err)
	}
	if len(dummyPasswordHash) < 55 || !strings.HasPrefix(dummyPasswordHash, "$2") {
		t.Fatalf("dummyPasswordHash = %q, want a bcrypt hash", dummyPasswordHash)
	}
	for _, candidate := range []string{"", goodPassword, "placeholder-for-timing", "password"} {
		if VerifyPassword(dummyPasswordHash, candidate) {
			t.Errorf("dummy hash matched %q", candidate)
		}
	}
}

func TestHashPasswordRejectsInvalidLength(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("HashPassword(short) error = %v, want ErrInvalidPassword", err)
	}
}

func TestCreateReaderStoresNormalizedAccount(t *testing.T) {
	db := testDB(t)
	user := newReader(t, db, "  Alice@Example.COM ", "  阿丽丝  ")

	if user.Email != "alice@example.com" {
		t.Errorf("Email = %q, want the normalized address", user.Email)
	}
	if user.DisplayName != "阿丽丝" {
		t.Errorf("DisplayName = %q, want the trimmed name", user.DisplayName)
	}
	if user.Role != RoleReader {
		t.Errorf("Role = %q, want %q", user.Role, RoleReader)
	}
	if user.Status != StatusActive {
		t.Errorf("Status = %q, want %q", user.Status, StatusActive)
	}
	if !VerifyPassword(user.PasswordHash, goodPassword) {
		t.Error("stored hash does not verify the password")
	}

	found, err := FindByEmail(context.Background(), db, "ALICE@example.com")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if found.ID != user.ID {
		t.Errorf("ID = %d, want %d", found.ID, user.ID)
	}
}

func TestCreateReaderRejectsDuplicateEmail(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	newReader(t, db, "alice@example.com", "阿丽丝")

	for _, candidate := range []string{"alice@example.com", "ALICE@example.com", " alice@example.com "} {
		_, err := CreateReader(ctx, db, candidate, "另一个名字", "another password")
		if !errors.Is(err, ErrEmailTaken) {
			t.Errorf("CreateReader(%q) error = %v, want ErrEmailTaken", candidate, err)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Errorf("users = %d, want 1", count)
	}
}

func TestCreateReaderRejectsInvalidInput(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := CreateReader(ctx, db, "not-an-email", "名字", goodPassword); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("invalid email error = %v, want ErrInvalidEmail", err)
	}
	if _, err := CreateReader(ctx, db, "alice@example.com", "   ", goodPassword); !errors.Is(err, ErrInvalidDisplayName) {
		t.Errorf("invalid name error = %v, want ErrInvalidDisplayName", err)
	}
	if _, err := CreateReader(ctx, db, "alice@example.com", "名字", "short"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("invalid password error = %v, want ErrInvalidPassword", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Errorf("users = %d, want none created", count)
	}
}

func TestBootstrapOwnerCreatesExactlyOneOwner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	exists, err := OwnerExists(ctx, db)
	if err != nil {
		t.Fatalf("OwnerExists: %v", err)
	}
	if exists {
		t.Fatal("OwnerExists = true on an empty database")
	}

	owner, err := BootstrapOwner(ctx, db, "Owner@Example.com", "站长", goodPassword)
	if err != nil {
		t.Fatalf("BootstrapOwner: %v", err)
	}
	if owner.Role != RoleOwner {
		t.Errorf("Role = %q, want %q", owner.Role, RoleOwner)
	}

	if _, err := BootstrapOwner(ctx, db, "second@example.com", "第二位", goodPassword); !errors.Is(err, ErrOwnerExists) {
		t.Errorf("second bootstrap error = %v, want ErrOwnerExists", err)
	}

	// A reader account must not block the owner bootstrap; only an owner does.
	other := testDB(t)
	newReader(t, other, "reader@example.com", "读者")
	if _, err := BootstrapOwner(ctx, other, "owner@example.com", "站长", goodPassword); err != nil {
		t.Fatalf("BootstrapOwner with an existing reader: %v", err)
	}
}

func TestBootstrapOwnerRejectsInvalidInput(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := BootstrapOwner(ctx, db, "owner@example.com", "站长", "short"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("error = %v, want ErrInvalidPassword", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Errorf("users = %d, want none created", count)
	}
}

func TestFindByEmailUnknownAccount(t *testing.T) {
	db := testDB(t)
	if _, err := FindByEmail(context.Background(), db, "nobody@example.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("error = %v, want ErrUserNotFound", err)
	}
}

func TestMarkLoginUpdatesTimestamp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	if err := MarkLogin(ctx, db, user.ID, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkLogin: %v", err)
	}
	reloaded, err := FindByID(ctx, db, user.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if reloaded.LastLoginAt == nil {
		t.Fatal("LastLoginAt is nil after MarkLogin")
	}
	if want := "2026-09-28T12:00:00Z"; reloaded.LastLoginAt.UTC().Format(time.RFC3339) != want {
		t.Errorf("LastLoginAt = %s, want %s", reloaded.LastLoginAt.UTC().Format(time.RFC3339), want)
	}
}

func TestCreateSessionStoresOnlyHashes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	session, err := CreateSession(ctx, db, NewSession{
		UserID: user.ID, Now: now, TTL: time.Hour,
		UserAgent: "test-agent", IPPrefix: "192.0.2.0/24",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if raw, err := base64URLDecode(session.Token); err != nil {
		t.Fatalf("session token is not base64url: %v", err)
	} else if len(raw) != SessionTokenBytes {
		t.Errorf("session token = %d bytes, want %d", len(raw), SessionTokenBytes)
	}
	if raw, err := base64URLDecode(session.CSRFToken); err != nil {
		t.Fatalf("csrf token is not base64url: %v", err)
	} else if len(raw) != CSRFTokenBytes {
		t.Errorf("csrf token = %d bytes, want %d", len(raw), CSRFTokenBytes)
	}

	var storedToken, storedCSRF []byte
	var storedUA, storedIP, expiresAt string
	if err := db.QueryRowContext(ctx,
		`SELECT token_hash, csrf_token_hash, user_agent, ip_prefix, expires_at FROM sessions WHERE user_id = ?`,
		user.ID).Scan(&storedToken, &storedCSRF, &storedUA, &storedIP, &expiresAt); err != nil {
		t.Fatalf("read session row: %v", err)
	}
	if string(storedToken) == session.Token {
		t.Error("the plaintext session token was stored in the database")
	}
	if string(storedToken) != string(HashToken(session.Token)) {
		t.Error("token_hash is not the SHA-256 hash of the token")
	}
	if string(storedCSRF) != string(HashToken(session.CSRFToken)) {
		t.Error("csrf_token_hash is not the SHA-256 hash of the CSRF token")
	}
	if len(storedToken) != 32 || len(storedCSRF) != 32 {
		t.Errorf("hash lengths = %d and %d, want 32", len(storedToken), len(storedCSRF))
	}
	if storedUA != "test-agent" || storedIP != "192.0.2.0/24" {
		t.Errorf("user_agent/ip_prefix = %q/%q", storedUA, storedIP)
	}
	if want := now.Add(time.Hour).Format(time.RFC3339); expiresAt != want {
		t.Errorf("expires_at = %q, want %q", expiresAt, want)
	}
}

func TestSessionTokensAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		token, hash, err := NewToken(SessionTokenBytes)
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[token] {
			t.Fatal("NewToken repeated a token")
		}
		if len(hash) != 32 {
			t.Fatalf("hash length = %d, want 32", len(hash))
		}
		seen[token] = true
	}
}

func TestLookupSessionValidatesState(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	fresh, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	session, found, err := LookupSession(ctx, db, fresh.Token, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if found.ID != user.ID || found.Email != user.Email {
		t.Errorf("looked up user = %+v, want %+v", found, user)
	}
	if session.ID != fresh.ID {
		t.Errorf("session ID = %d, want %d", session.ID, fresh.ID)
	}

	if _, _, err := LookupSession(ctx, db, "", now); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("empty token error = %v, want ErrSessionInvalid", err)
	}
	if _, _, err := LookupSession(ctx, db, "unknown-token-value", now); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("unknown token error = %v, want ErrSessionInvalid", err)
	}
}

func TestLookupSessionRejectsAndRemovesExpiredSession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	expired, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now.Add(-2 * time.Hour), TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, _, err := LookupSession(ctx, db, expired.Token, now); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("error = %v, want ErrSessionExpired", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("sessions = %d, want the expired row removed", count)
	}
}

func TestLookupSessionRejectsDisabledUser(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	session, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = ?`, user.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	if _, _, err := LookupSession(ctx, db, session.Token, now); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("error = %v, want ErrAccountDisabled", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("sessions = %d, want the disabled user's session removed", count)
	}
}

func TestRotateSessionInvalidatesTheOldToken(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	first, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rotated, err := RotateSession(ctx, db, first.Token, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("RotateSession: %v", err)
	}
	if rotated.Token == first.Token {
		t.Fatal("rotation reused the previous token")
	}
	if rotated.CSRFToken == first.CSRFToken {
		t.Error("rotation reused the previous CSRF token")
	}
	if _, _, err := LookupSession(ctx, db, first.Token, now); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("old token error = %v, want ErrSessionInvalid", err)
	}
	if _, _, err := LookupSession(ctx, db, rotated.Token, now); err != nil {
		t.Errorf("new token rejected: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE user_id = ?`, user.ID).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("sessions = %d, want exactly one after rotation", count)
	}

	// Rotating without a previous token (a fresh sign-in) creates a session and
	// must leave unrelated sessions alone: signing in on a second device does
	// not sign the first one out.
	fresh, err := RotateSession(ctx, db, "", NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("RotateSession without a previous token: %v", err)
	}
	if _, _, err := LookupSession(ctx, db, fresh.Token, now); err != nil {
		t.Errorf("new session rejected: %v", err)
	}
	if _, _, err := LookupSession(ctx, db, rotated.Token, now); err != nil {
		t.Errorf("unrelated session was invalidated: %v", err)
	}
}

func TestDeleteSessionIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	session, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := DeleteSession(ctx, db, session.Token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, _, err := LookupSession(ctx, db, session.Token, now); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("error = %v, want ErrSessionInvalid", err)
	}
	if err := DeleteSession(ctx, db, session.Token); err != nil {
		t.Errorf("second DeleteSession: %v", err)
	}
	if err := DeleteSession(ctx, db, ""); err != nil {
		t.Errorf("DeleteSession with an empty token: %v", err)
	}
}

// TestSweepExpiredSessionsRemovesOnlyStaleRows covers the cleanup sign-in
// amortises. Three sessions are created around the sweep instant: the live one
// must survive, the long-expired one and the one expiring exactly now must go
// (LookupSession rejects a row once !now.Before(expiresAt), so the boundary row
// is already unusable and keeping it would leak a row per sign-in).
func TestSweepExpiredSessionsRemovesOnlyStaleRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	live, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("CreateSession(live): %v", err)
	}
	expired, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now.Add(-48 * time.Hour), TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession(expired): %v", err)
	}
	boundary, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now.Add(-time.Hour), TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession(boundary): %v", err)
	}

	removed, err := SweepExpiredSessions(ctx, db, now)
	if err != nil {
		t.Fatalf("SweepExpiredSessions: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	if _, _, err := LookupSession(ctx, db, live.Token, now); err != nil {
		t.Errorf("the live session did not survive the sweep: %v", err)
	}
	for name, token := range map[string]string{"expired": expired.Token, "boundary": boundary.Token} {
		// The row is gone, so the token is unknown rather than expired.
		if _, _, err := LookupSession(ctx, db, token, now); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("%s session: error = %v, want ErrSessionInvalid", name, err)
		}
	}

	// Running it again removes nothing: the sweep converges.
	removed, err = SweepExpiredSessions(ctx, db, now)
	if err != nil {
		t.Fatalf("second SweepExpiredSessions: %v", err)
	}
	if removed != 0 {
		t.Errorf("second sweep removed = %d, want 0", removed)
	}

	if _, err := SweepExpiredSessions(ctx, nil, now); err == nil {
		t.Error("SweepExpiredSessions(nil) succeeded, want an error")
	}
}

// TestCSRFTokenIsDerivedFromTheSession documents that the CSRF token is
// recomputable from the session, so GET /api/v1/auth/me can return it without
// storing plaintext.
func TestCSRFTokenIsDerivedFromTheSession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	created, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if want := csrfToken(HashToken(created.Token)); created.CSRFToken != want {
		t.Error("CSRFToken is not derived from the session token")
	}

	loaded, _, err := LookupSession(ctx, db, created.Token, now)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if loaded.CSRFToken != created.CSRFToken {
		t.Errorf("reloaded CSRF token = %q, want %q", loaded.CSRFToken, created.CSRFToken)
	}
	if !VerifyCSRF(loaded, created.CSRFToken) {
		t.Error("the reloaded session rejected its own CSRF token")
	}

	// A row whose csrf_token_hash does not match the derivation is unusable.
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET csrf_token_hash = x'00'`); err != nil {
		t.Fatalf("tamper csrf hash: %v", err)
	}
	if _, _, err := LookupSession(ctx, db, created.Token, now); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("tampered row error = %v, want ErrSessionInvalid", err)
	}
}

func TestCSRFTokenIsPerSession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	user := newReader(t, db, "alice@example.com", "阿丽丝")

	first, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	second, err := CreateSession(ctx, db, NewSession{UserID: user.ID, Now: now, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if first.CSRFToken == second.CSRFToken {
		t.Fatal("two sessions share a CSRF token")
	}

	if !VerifyCSRF(first, first.CSRFToken) {
		t.Error("VerifyCSRF rejected the matching token")
	}
	for _, candidate := range []string{"", " ", second.CSRFToken, first.CSRFToken + "x", strings.ToUpper(first.CSRFToken)} {
		if VerifyCSRF(first, candidate) {
			t.Errorf("VerifyCSRF accepted %q", candidate)
		}
	}
}

func TestHashTokenIsStableAndDistinct(t *testing.T) {
	if string(HashToken("abc")) != string(HashToken("abc")) {
		t.Error("HashToken is not deterministic")
	}
	if string(HashToken("abc")) == string(HashToken("abd")) {
		t.Error("HashToken collided")
	}
}
