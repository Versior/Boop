// Package settings stores the documented non-secret site settings as typed
// values backed by the settings table.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"boop/internal/secretbox"
)

// Bounds enforced by Set so no caller can store a value that renders the site
// useless or breaks Load later.
const (
	minPageSize         = 1
	maxPageSize         = 50
	minAuthorStatusTTL  = 1
	maxAuthorStatusTTL  = 2160
	maxSiteNameRunes    = 80
	maxDescriptionRunes = 280
	maxModelNameRunes   = 200
)

// Setting keys as stored in the settings table.
const (
	KeySiteName                  = "site.name"
	KeySiteDescription           = "site.description"
	KeySiteTimezone              = "site.timezone"
	KeyContentPageSize           = "content.page_size"
	KeyAuthRegistrationEnabled   = "auth.registration_enabled"
	KeyCommentsEnabled           = "comments.enabled"
	KeyCommentsModerationEnabled = "comments.moderation_enabled"
	KeyAIEnabled                 = "ai.enabled"
	KeyAIBaseURL                 = "ai.base_url"
	KeyAIChatModel               = "ai.chat_model"
	KeyAIEmbeddingModel          = "ai.embedding_model"
	KeyAIAuthorStatusTTLHours    = "ai.author_status_ttl_hours"
)

// Secret keys live in the secret_settings table and are encrypted with
// BOOP_MASTER_KEY (docs/PRODUCT.md §5.4, docs/DATABASE.md). They are never
// returned by an API and never appear in a log line.
const (
	SecretKeyGitHubClientID     = "github.client_id"
	SecretKeyGitHubClientSecret = "github.client_secret"
	SecretKeyAIAPIKey           = "ai.api_key"
)

// maxSecretBytes bounds one stored secret. An OAuth client secret or an API key
// is far shorter, and the bound keeps a single request from writing a huge row.
const maxSecretBytes = 512

var (
	// ErrInvalidValue marks a rejected settings value or key: the HTTP layer maps
	// it to 400 without having to parse the reason.
	ErrInvalidValue = errors.New("settings: value is not acceptable")
	// ErrMasterKeyRequired is returned when a secret needs BOOP_MASTER_KEY and it
	// is not configured, so the write fails closed instead of storing plaintext.
	ErrMasterKeyRequired = errors.New("settings: BOOP_MASTER_KEY is not configured")
	// ErrSecretNotFound means a documented secret has no stored row.
	ErrSecretNotFound = errors.New("settings: secret is not configured")
	// ErrUnknownSecretKey rejects a secret key the schema does not document.
	ErrUnknownSecretKey = errors.New("settings: unknown secret key")
)

// secretKeys lists every documented secret setting, so callers can report
// which ones are configured without knowing the list themselves.
var secretKeys = []string{SecretKeyGitHubClientID, SecretKeyGitHubClientSecret, SecretKeyAIAPIKey}

func knownSecretKey(key string) bool {
	for _, known := range secretKeys {
		if known == key {
			return true
		}
	}
	return false
}

// Values holds every non-secret setting with the documented defaults applied.
type Values struct {
	SiteName                  string
	SiteDescription           string
	SiteTimezone              string
	PageSize                  int
	RegistrationEnabled       bool
	CommentsEnabled           bool
	CommentsModerationEnabled bool
	AIEnabled                 bool
	AIBaseURL                 string
	AIChatModel               string
	AIEmbeddingModel          string
	AIAuthorStatusTTLHours    int
}

// Defaults returns the documented default settings.
func Defaults() Values {
	return Values{
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
}

// Seed writes the default value of every known key that has no row yet, in one
// transaction. Existing values are never overwritten.
func Seed(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("settings: seed: nil database")
	}
	defaults := Defaults()
	fields := fieldsOf(&defaults)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: seed: begin: %w", err)
	}
	defer tx.Rollback()

	statement, err := tx.PrepareContext(ctx, `INSERT INTO settings(key, value_json, updated_at)
		VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))
		ON CONFLICT(key) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("settings: seed: prepare: %w", err)
	}
	defer statement.Close()

	for key, target := range fields {
		raw, err := json.Marshal(target)
		if err != nil {
			return fmt.Errorf("settings: seed: encode %s: %w", key, err)
		}
		if _, err := statement.ExecContext(ctx, key, string(raw)); err != nil {
			return fmt.Errorf("settings: seed: store %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: seed: commit: %w", err)
	}
	return nil
}

// Load returns the defaults overlaid with every stored known key. Unknown keys
// are ignored so a newer schema can be downgraded without breaking startup.
func Load(ctx context.Context, db *sql.DB) (Values, error) {
	if db == nil {
		return Values{}, errors.New("settings: load: nil database")
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value_json FROM settings`)
	if err != nil {
		return Values{}, fmt.Errorf("settings: load: %w", err)
	}
	return decode(rows)
}

// LoadTx reads the same values inside a caller's transaction, so a write path can
// decide on settings that cannot change between that read and its own writes. A
// missing key still means the documented default; a stored value that does not
// decode is an error, and the caller's transaction rolls back.
func LoadTx(ctx context.Context, tx *sql.Tx) (Values, error) {
	if tx == nil {
		return Values{}, errors.New("settings: load: nil transaction")
	}
	rows, err := tx.QueryContext(ctx, `SELECT key, value_json FROM settings`)
	if err != nil {
		return Values{}, fmt.Errorf("settings: load: %w", err)
	}
	return decode(rows)
}

func decode(rows *sql.Rows) (Values, error) {
	defer rows.Close()
	values := Defaults()
	fields := fieldsOf(&values)

	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return Values{}, fmt.Errorf("settings: load: scan: %w", err)
		}
		target, known := fields[key]
		if !known {
			continue
		}
		if err := json.Unmarshal([]byte(raw), target); err != nil {
			return Values{}, fmt.Errorf("settings: load: %s holds an invalid value: %w", key, err)
		}
	}
	if err := rows.Err(); err != nil {
		return Values{}, fmt.Errorf("settings: load: %w", err)
	}
	return values, nil
}

// Set validates and stores one known non-secret key as JSON, inserting or
// replacing the row. An invalid value is rejected before the database is touched.
func Set(ctx context.Context, db *sql.DB, key string, value any) error {
	return Apply(ctx, db, nil, Update{Values: map[string]any{key: value}})
}

// Update is one settings change. Values are validated, secrets are encrypted,
// and every row lands in a single transaction, so a rejected field never leaves
// a half-applied update behind.
type Update struct {
	// Values are non-secret settings keyed by their documented key.
	Values map[string]any
	// Secrets are secret settings to store; the plaintext is encrypted with the
	// master key before the transaction opens.
	Secrets map[string]string
	// Clear names secret settings to delete.
	Clear []string
}

// Apply validates and writes one settings change. Without BOOP_MASTER_KEY a
// secret change fails with ErrMasterKeyRequired rather than storing plaintext.
func Apply(ctx context.Context, db *sql.DB, box *secretbox.Box, update Update) error {
	if db == nil {
		return errors.New("settings: apply: nil database")
	}

	// Validate everything first: the database is only touched when the whole
	// update is acceptable.
	var probe Values
	fields := fieldsOf(&probe)
	for key, value := range update.Values {
		if _, known := fields[key]; !known {
			return fmt.Errorf("%w: unknown key %q", ErrInvalidValue, key)
		}
		if err := validate(key, value); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidValue, err)
		}
	}
	if len(update.Secrets) > 0 || len(update.Clear) > 0 {
		if box == nil {
			return ErrMasterKeyRequired
		}
	}
	for key, value := range update.Secrets {
		if !knownSecretKey(key) {
			return fmt.Errorf("%w: unknown secret key %q", ErrInvalidValue, key)
		}
		switch {
		case value == "":
			return fmt.Errorf("%w: secret %s must not be empty", ErrInvalidValue, key)
		case len(value) > maxSecretBytes:
			return fmt.Errorf("%w: secret %s must be at most %d bytes", ErrInvalidValue, key, maxSecretBytes)
		}
	}
	for _, key := range update.Clear {
		if !knownSecretKey(key) {
			return fmt.Errorf("%w: unknown secret key %q", ErrInvalidValue, key)
		}
	}

	// Encrypt before the transaction: sealing is quick, but the write lock must
	// not be held for work that can fail (the same rule password hashing follows).
	type sealed struct{ nonce, ciphertext []byte }
	sealedSecrets := make(map[string]sealed, len(update.Secrets))
	for key, value := range update.Secrets {
		nonce, ciphertext, err := box.Seal(value)
		if err != nil {
			return fmt.Errorf("settings: apply: encrypt %s: %w", key, err)
		}
		sealedSecrets[key] = sealed{nonce: nonce, ciphertext: ciphertext}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: apply: begin: %w", err)
	}
	defer tx.Rollback()

	for key, value := range update.Values {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("settings: apply: encode %s: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx, upsertValueSQL, key, string(raw)); err != nil {
			return fmt.Errorf("settings: apply: %s: %w", key, err)
		}
	}
	for key, value := range sealedSecrets {
		if _, err := tx.ExecContext(ctx, upsertSecretSQL, key, value.nonce, value.ciphertext); err != nil {
			return fmt.Errorf("settings: apply: %s: %w", key, err)
		}
	}
	for _, key := range update.Clear {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secret_settings WHERE key = ?`, key); err != nil {
			return fmt.Errorf("settings: apply: clear %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: apply: commit: %w", err)
	}
	return nil
}

const upsertValueSQL = `INSERT INTO settings(key, value_json, updated_at)
	VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))
	ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`

const upsertSecretSQL = `INSERT INTO secret_settings(key, nonce, ciphertext, updated_at)
	VALUES(?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))
	ON CONFLICT(key) DO UPDATE SET nonce = excluded.nonce, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`

// ReadSecret decrypts one stored secret. A missing master key, an unknown key
// and a missing row are separate answers, so a caller can tell an unconfigured
// site from a broken one.
func ReadSecret(ctx context.Context, db *sql.DB, box *secretbox.Box, key string) (string, error) {
	if db == nil {
		return "", errors.New("settings: read secret: nil database")
	}
	if !knownSecretKey(key) {
		return "", fmt.Errorf("%w: %q", ErrUnknownSecretKey, key)
	}
	if box == nil {
		return "", ErrMasterKeyRequired
	}
	var nonce, ciphertext []byte
	err := db.QueryRowContext(ctx, `SELECT nonce, ciphertext FROM secret_settings WHERE key = ?`, key).
		Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, key)
	}
	if err != nil {
		return "", fmt.Errorf("settings: read secret %s: %w", key, err)
	}
	value, err := box.Open(nonce, ciphertext)
	if err != nil {
		return "", fmt.Errorf("settings: read secret %s: %w", key, err)
	}
	return value, nil
}

// ConfiguredSecrets reports which documented secret keys have a stored value.
// It never decrypts, so a page can show whether a secret exists without holding
// its plaintext.
func ConfiguredSecrets(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	configured := make(map[string]bool, len(secretKeys))
	for _, key := range secretKeys {
		configured[key] = false
	}
	if db == nil {
		return nil, errors.New("settings: configured secrets: nil database")
	}
	rows, err := db.QueryContext(ctx, `SELECT key FROM secret_settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: configured secrets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("settings: configured secrets: scan: %w", err)
		}
		if knownSecretKey(key) {
			configured[key] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings: configured secrets: %w", err)
	}
	return configured, nil
}

// validate enforces the type and range of every known key. Values are checked
// strictly: no string is parsed into a number and no truthy value is accepted
// for a boolean, so a caller cannot store JSON that Load later rejects.
func validate(key string, value any) error {
	switch key {
	case KeySiteName:
		return validateText(key, value, 1, maxSiteNameRunes)
	case KeySiteDescription:
		return validateText(key, value, 0, maxDescriptionRunes)
	case KeyAIChatModel, KeyAIEmbeddingModel:
		return validateText(key, value, 0, maxModelNameRunes)
	case KeySiteTimezone:
		zone, err := requireString(key, value)
		if err != nil {
			return err
		}
		if zone == "" {
			return fmt.Errorf("settings: set: %s must not be empty", key)
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return fmt.Errorf("settings: set: %s %q is not a known time zone", key, zone)
		}
		return nil
	case KeyAIBaseURL:
		base, err := requireString(key, value)
		if err != nil {
			return err
		}
		return validateBaseURL(key, base)
	case KeyContentPageSize:
		return validateInt(key, value, minPageSize, maxPageSize)
	case KeyAIAuthorStatusTTLHours:
		return validateInt(key, value, minAuthorStatusTTL, maxAuthorStatusTTL)
	case KeyAuthRegistrationEnabled, KeyCommentsEnabled, KeyCommentsModerationEnabled, KeyAIEnabled:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("settings: set: %s requires a bool, got %T", key, value)
		}
		return nil
	default:
		return fmt.Errorf("settings: set: unknown key %q", key)
	}
}

func requireString(key string, value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("settings: set: %s requires a string, got %T", key, value)
	}
	return s, nil
}

// validateText checks the type and rune length of a free-text setting;
// minRunes > 0 additionally requires a non-blank value.
func validateText(key string, value any, minRunes, maxRunes int) error {
	s, err := requireString(key, value)
	if err != nil {
		return err
	}
	length := utf8.RuneCountInString(s)
	if minRunes > 0 && strings.TrimSpace(s) == "" {
		return fmt.Errorf("settings: set: %s must not be empty", key)
	}
	if length < minRunes || length > maxRunes {
		return fmt.Errorf("settings: set: %s must be %d..%d characters, got %d", key, minRunes, maxRunes, length)
	}
	return nil
}

func validateInt(key string, value any, min, max int) error {
	number, ok := value.(int)
	if !ok {
		return fmt.Errorf("settings: set: %s requires an int, got %T", key, value)
	}
	if number < min || number > max {
		return fmt.Errorf("settings: set: %s must be between %d and %d, got %d", key, min, max, number)
	}
	return nil
}

// validateBaseURL accepts an empty value (AI disabled) or an absolute http(s)
// endpoint with no credentials, query or fragment.
func validateBaseURL(key, raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("settings: set: %s %q is not a URL", key, raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("settings: set: %s %q must use http or https", key, raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("settings: set: %s %q has no host", key, raw)
	}
	if parsed.User != nil {
		return fmt.Errorf("settings: set: %s must not contain credentials", key)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("settings: set: %s must not contain a query or fragment", key)
	}
	return nil
}

// fieldsOf maps each known key to the matching field of values.
func fieldsOf(values *Values) map[string]any {
	return map[string]any{
		KeySiteName:                  &values.SiteName,
		KeySiteDescription:           &values.SiteDescription,
		KeySiteTimezone:              &values.SiteTimezone,
		KeyContentPageSize:           &values.PageSize,
		KeyAuthRegistrationEnabled:   &values.RegistrationEnabled,
		KeyCommentsEnabled:           &values.CommentsEnabled,
		KeyCommentsModerationEnabled: &values.CommentsModerationEnabled,
		KeyAIEnabled:                 &values.AIEnabled,
		KeyAIBaseURL:                 &values.AIBaseURL,
		KeyAIChatModel:               &values.AIChatModel,
		KeyAIEmbeddingModel:          &values.AIEmbeddingModel,
		KeyAIAuthorStatusTTLHours:    &values.AIAuthorStatusTTLHours,
	}
}
