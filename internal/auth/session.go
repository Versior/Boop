package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Documented session rules: docs/PRODUCT.md §5.2 (HttpOnly, SameSite=Lax,
// Secure in production) and docs/API.md (X-CSRF-Token on unsafe methods).
const (
	// SessionTokenBytes is 32 bytes of entropy; only its SHA-256 hash is stored.
	SessionTokenBytes = 32
	// CSRFTokenBytes is the CSRF token size: HMAC-SHA256 encoded as base64url.
	CSRFTokenBytes = 32
	// DefaultSessionTTL bounds how long a stolen session stays usable.
	DefaultSessionTTL = 30 * 24 * time.Hour
)

var (
	// ErrSessionInvalid covers a missing, unknown or already rotated token.
	ErrSessionInvalid = errors.New("auth: session is not valid")
	// ErrSessionExpired means the row existed but is past expires_at.
	ErrSessionExpired = errors.New("auth: session has expired")
	// ErrAccountDisabled means the account was disabled after signing in.
	ErrAccountDisabled = errors.New("auth: account is disabled")
)

// Session is a session row plus, only for the caller that just created it, the
// plaintext tokens. LookupSession never returns plaintext tokens.
type Session struct {
	ID            int64
	UserID        int64
	Token         string
	CSRFToken     string
	TokenHash     []byte
	CSRFTokenHash []byte
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

// NewSession describes a session to create. Now and TTL are explicit so tests
// and callers share one clock.
type NewSession struct {
	UserID    int64
	Now       time.Time
	TTL       time.Duration
	UserAgent string
	IPPrefix  string
}

// NewToken returns a random URL-safe token and its SHA-256 hash. The plaintext
// value is returned to the caller once and never stored.
func NewToken(size int) (string, []byte, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken hashes a token for storage and lookup.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// csrfToken derives the session's CSRF token from its token hash with HMAC, so
// the server can recompute it at any time (GET /api/v1/auth/me must return it)
// without ever storing the plaintext. A cross-site attacker cannot read the
// session cookie, so it cannot produce the value.
func csrfToken(tokenHash []byte) string {
	mac := hmac.New(sha256.New, tokenHash)
	mac.Write([]byte(csrfDerivationLabel))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// csrfDerivationLabel versions the derivation, so changing it invalidates every
// stored csrf_token_hash instead of silently accepting the old scheme.
const csrfDerivationLabel = "boop-csrf-v1"

// CreateSession stores a new session for userID and returns its plaintext
// tokens to the caller.
func CreateSession(ctx context.Context, db *sql.DB, in NewSession) (Session, error) {
	if db == nil {
		return Session{}, errors.New("auth: create session: nil database")
	}
	return insertSession(ctx, db, in)
}

// RotateSession deletes the session identified by oldToken, then creates a new
// one. Every sign-in rotates: a stolen or fixated cookie cannot survive it.
func RotateSession(ctx context.Context, db *sql.DB, oldToken string, in NewSession) (Session, error) {
	if db == nil {
		return Session{}, errors.New("auth: rotate session: nil database")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("auth: rotate session: begin: %w", err)
	}
	defer tx.Rollback()

	if strings.TrimSpace(oldToken) != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(oldToken)); err != nil {
			return Session{}, fmt.Errorf("auth: rotate session: delete previous: %w", err)
		}
	}
	session, err := insertSession(ctx, tx, in)
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("auth: rotate session: commit: %w", err)
	}
	return session, nil
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertSession(ctx context.Context, exec execer, in NewSession) (Session, error) {
	token, tokenHash, err := NewToken(SessionTokenBytes)
	if err != nil {
		return Session{}, err
	}
	csrf := csrfToken(tokenHash)
	csrfHash := HashToken(csrf)
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	expiresAt := now.Add(ttl)

	result, err := exec.ExecContext(ctx, `INSERT INTO sessions(user_id, token_hash, csrf_token_hash, expires_at, created_at, last_seen_at, user_agent, ip_prefix)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, tokenHash, csrfHash, timestamp(expiresAt), timestamp(now), timestamp(now),
		truncate(in.UserAgent, 255), truncate(in.IPPrefix, 64))
	if err != nil {
		return Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Session{}, fmt.Errorf("auth: create session: id: %w", err)
	}

	return Session{
		ID:            id,
		UserID:        in.UserID,
		Token:         token,
		CSRFToken:     csrf,
		TokenHash:     tokenHash,
		CSRFTokenHash: csrfHash,
		ExpiresAt:     expiresAt,
		CreatedAt:     now,
	}, nil
}

// LookupSession resolves a token to its session and account. Expired sessions
// and sessions of disabled accounts are deleted and rejected.
func LookupSession(ctx context.Context, db *sql.DB, token string, now time.Time) (Session, User, error) {
	if db == nil {
		return Session{}, User{}, errors.New("auth: lookup session: nil database")
	}
	if strings.TrimSpace(token) == "" {
		return Session{}, User{}, ErrSessionInvalid
	}

	var session Session
	var expiresAt, createdAt string
	var user User
	var userCreatedAt string
	var lastLoginAt sql.NullString
	err := db.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.csrf_token_hash, s.expires_at, s.created_at,
			u.id, u.email, u.display_name, u.avatar_url, u.role, u.status, COALESCE(u.password_hash, ''), u.created_at, u.last_login_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?`, HashToken(token)).
		Scan(&session.ID, &session.UserID, &session.CSRFTokenHash, &expiresAt, &createdAt,
			&user.ID, &user.Email, &user.DisplayName, &user.AvatarURL, &user.Role, &user.Status,
			&user.PasswordHash, &userCreatedAt, &lastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, User{}, ErrSessionInvalid
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("auth: lookup session: %w", err)
	}
	session.TokenHash = HashToken(token)
	session.CSRFToken = csrfToken(session.TokenHash)
	if subtle.ConstantTimeCompare(HashToken(session.CSRFToken), session.CSRFTokenHash) != 1 {
		// The stored hash does not match the derivation: the row was altered or
		// written by an older scheme. Treat it as unusable.
		if err := DeleteSession(ctx, db, token); err != nil {
			return Session{}, User{}, err
		}
		return Session{}, User{}, ErrSessionInvalid
	}
	if session.ExpiresAt, err = parseTimestamp(expiresAt); err != nil {
		return Session{}, User{}, fmt.Errorf("auth: session %d expires_at: %w", session.ID, err)
	}
	if session.CreatedAt, err = parseTimestamp(createdAt); err != nil {
		return Session{}, User{}, fmt.Errorf("auth: session %d created_at: %w", session.ID, err)
	}
	if user.CreatedAt, err = parseTimestamp(userCreatedAt); err != nil {
		return Session{}, User{}, fmt.Errorf("auth: user %d created_at: %w", user.ID, err)
	}
	if lastLoginAt.Valid && lastLoginAt.String != "" {
		loginAt, err := parseTimestamp(lastLoginAt.String)
		if err != nil {
			return Session{}, User{}, fmt.Errorf("auth: user %d last_login_at: %w", user.ID, err)
		}
		user.LastLoginAt = &loginAt
	}

	if !now.Before(session.ExpiresAt) {
		if err := DeleteSession(ctx, db, token); err != nil {
			return Session{}, User{}, err
		}
		return Session{}, User{}, ErrSessionExpired
	}
	if user.Status != StatusActive {
		if err := DeleteSession(ctx, db, token); err != nil {
			return Session{}, User{}, err
		}
		return Session{}, User{}, ErrAccountDisabled
	}
	return session, user, nil
}

// DeleteSession removes a session by plaintext token. An unknown or empty token
// is not an error: signing out twice stays safe.
func DeleteSession(ctx context.Context, db *sql.DB, token string) error {
	if db == nil {
		return errors.New("auth: delete session: nil database")
	}
	if strings.TrimSpace(token) == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(token)); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// SweepExpiredSessions deletes every session past its expires_at and reports how
// many rows went away.
//
// Only LookupSession used to remove expired rows, and only for the cookie being
// presented, so a browser that never came back left its row behind forever and
// the table grew with every sign-in. The caller amortises this sweep over
// sign-ins (see server.newSession) instead of scheduling it: sessions are only
// ever added by a sign-in, so an instance nobody signs into has nothing to
// clean, and a busy one pays for at most one indexed DELETE per interval.
//
// The comparison is on the stored text. timestamp() writes RFC3339 in UTC with
// no fractional seconds, so every row is a fixed-width "2006-01-02T15:04:05Z"
// string and lexicographic order is chronological order; idx_sessions_expires
// serves the range. A row whose expires_at an older build wrote in another form
// simply falls outside the range and is left for LookupSession to reject.
func SweepExpiredSessions(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, errors.New("auth: sweep sessions: nil database")
	}
	result, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, timestamp(now))
	if err != nil {
		return 0, fmt.Errorf("auth: sweep sessions: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		// The rows are gone; only the count is unknown, which is not worth
		// failing a sign-in over.
		return 0, nil
	}
	return removed, nil
}

// VerifyCSRF compares a presented token with the session's derived CSRF token
// in constant time.
func VerifyCSRF(session Session, candidate string) bool {
	if candidate == "" || session.CSRFToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(session.CSRFToken)) == 1
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
