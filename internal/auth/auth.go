// Package auth owns Boop accounts: credential validation, bcrypt password
// hashing, owner bootstrap and cookie-backed sessions with per-session CSRF
// tokens.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"boop/internal/store"
)

// Roles and account states as stored in the users table.
const (
	RoleOwner  = "owner"
	RoleReader = "reader"

	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Documented credential limits: docs/PRODUCT.md (邮箱无需验证) and docs/API.md
// (密码 10–72 字节；邮箱最大 254 字符).
const (
	MinPasswordBytes    = 10
	MaxPasswordBytes    = 72
	MaxEmailChars       = 254
	MaxDisplayNameRunes = 60
)

var (
	// ErrInvalidEmail, ErrInvalidPassword and ErrInvalidDisplayName are input
	// validation failures and may be reported to the caller.
	ErrInvalidEmail       = errors.New("auth: email is not a valid address")
	ErrInvalidPassword    = errors.New("auth: password must be 10 to 72 bytes")
	ErrInvalidDisplayName = errors.New("auth: display name must be 1 to 60 characters")

	// ErrEmailTaken is reported as a conflict; it does reveal that an email is
	// registered, which registration must do to be usable.
	ErrEmailTaken = errors.New("auth: email is already registered")

	// ErrOwnerExists guards the one-time owner bootstrap.
	ErrOwnerExists = errors.New("auth: an owner account already exists")

	ErrUserNotFound = errors.New("auth: user not found")
)

// User is an account row. PasswordHash never leaves the process: HTTP payloads
// are built from explicit fields, never by serialising this struct.
type User struct {
	ID           int64
	Email        string
	DisplayName  string
	AvatarURL    string
	Role         string
	Status       string
	PasswordHash string
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

// IsOwner reports whether the account may publish and administer the site.
func (u User) IsOwner() bool { return u.Role == RoleOwner }

// IsActive reports whether the account may authenticate.
func (u User) IsActive() bool { return u.Status == StatusActive }

// NormalizeEmail trims, lowercases and validates an address. Only ASCII
// addresses are accepted, which keeps lookups, uniqueness and rendering free of
// Unicode homograph ambiguity; the local part keeps the RFC 5322 atoms that
// real-world signups use, including "+" tagging.
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" || len(email) > MaxEmailChars {
		return "", ErrInvalidEmail
	}
	local, domain, found := strings.Cut(email, "@")
	if !found || strings.Contains(domain, "@") {
		return "", ErrInvalidEmail
	}
	if !validLocalPart(local) || !validDomain(domain) {
		return "", ErrInvalidEmail
	}
	return email, nil
}

func validLocalPart(local string) bool {
	if local == "" || len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") {
		return false
	}
	if strings.Contains(local, "..") {
		return false
	}
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-/=?^_`{|}~.", r):
		default:
			return false
		}
	}
	return true
}

func validDomain(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 ||
			strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			default:
				return false
			}
		}
	}
	// A top level domain is alphabetic and at least two characters long, which
	// rejects "user@example.1" and "user@example.c".
	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	for _, r := range tld {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// NormalizeDisplayName trims and validates a display name.
func NormalizeDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	length := utf8.RuneCountInString(name)
	if length < 1 || length > MaxDisplayNameRunes {
		return "", ErrInvalidDisplayName
	}
	return name, nil
}

// CreateReader registers a public account. Public registration can only ever
// create a reader; the owner role comes from BootstrapOwner alone.
func CreateReader(ctx context.Context, db *sql.DB, email, displayName, password string) (User, error) {
	return createUser(ctx, db, RoleReader, email, displayName, password, false)
}

// BootstrapOwner creates the single owner account. It refuses once an owner
// exists, so a second run cannot take over the site.
func BootstrapOwner(ctx context.Context, db *sql.DB, email, displayName, password string) (User, error) {
	return createUser(ctx, db, RoleOwner, email, displayName, password, true)
}

func createUser(ctx context.Context, db *sql.DB, role, email, displayName, password string, requireNoOwner bool) (User, error) {
	if db == nil {
		return User{}, errors.New("auth: create user: nil database")
	}
	normalizedEmail, err := NormalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	name, err := NormalizeDisplayName(displayName)
	if err != nil {
		return User{}, err
	}
	// Hash before opening the transaction: bcrypt is intentionally slow and
	// must not hold SQLite's single write lock.
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}

	now := timestamp(time.Now())
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("auth: create user: begin: %w", err)
	}
	defer tx.Rollback()

	if requireNoOwner {
		var owners int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = ?`, RoleOwner).Scan(&owners); err != nil {
			return User{}, fmt.Errorf("auth: create user: count owners: %w", err)
		}
		if owners > 0 {
			return User{}, ErrOwnerExists
		}
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO users(email, password_hash, display_name, role, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		normalizedEmail, hash, name, role, StatusActive, now, now)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("auth: create user: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("auth: create user: id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("auth: create user: commit: %w", err)
	}

	created, err := FindByID(ctx, db, id)
	if err != nil {
		return User{}, err
	}
	return created, nil
}

const userColumns = `id, email, display_name, avatar_url, role, status, COALESCE(password_hash, ''), created_at, last_login_at`

// FindByEmail loads an account by normalized address.
func FindByEmail(ctx context.Context, db *sql.DB, email string) (User, error) {
	if db == nil {
		return User{}, errors.New("auth: find user: nil database")
	}
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return User{}, ErrUserNotFound
	}
	return scanUser(db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, normalized))
}

// FindByID loads an account by primary key.
func FindByID(ctx context.Context, db *sql.DB, id int64) (User, error) {
	if db == nil {
		return User{}, errors.New("auth: find user: nil database")
	}
	return scanUser(db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// OwnerExists reports whether the one-time owner bootstrap already ran.
func OwnerExists(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, errors.New("auth: owner exists: nil database")
	}
	var owners int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = ?`, RoleOwner).Scan(&owners); err != nil {
		return false, fmt.Errorf("auth: count owners: %w", err)
	}
	return owners > 0, nil
}

// MarkLogin records the last successful sign-in time.
func MarkLogin(ctx context.Context, db *sql.DB, userID int64, at time.Time) error {
	if db == nil {
		return errors.New("auth: mark login: nil database")
	}
	if _, err := db.ExecContext(ctx, `UPDATE users SET last_login_at = ?, updated_at = ? WHERE id = ?`,
		timestamp(at), timestamp(at), userID); err != nil {
		return fmt.Errorf("auth: mark login: %w", err)
	}
	return nil
}

func scanUser(row *sql.Row) (User, error) {
	var user User
	var createdAt string
	var lastLoginAt sql.NullString
	err := row.Scan(&user.ID, &user.Email, &user.DisplayName, &user.AvatarURL, &user.Role, &user.Status,
		&user.PasswordHash, &createdAt, &lastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: scan user: %w", err)
	}
	created, err := parseTimestamp(createdAt)
	if err != nil {
		return User{}, fmt.Errorf("auth: user %d created_at: %w", user.ID, err)
	}
	user.CreatedAt = created
	if lastLoginAt.Valid && lastLoginAt.String != "" {
		loginAt, err := parseTimestamp(lastLoginAt.String)
		if err != nil {
			return User{}, fmt.Errorf("auth: user %d last_login_at: %w", user.ID, err)
		}
		user.LastLoginAt = &loginAt
	}
	return user, nil
}

// timestamp renders UTC RFC3339 as required by docs/DATABASE.md.
func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func parseTimestamp(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339, raw)
}
