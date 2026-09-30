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
	maxAssetURLRunes    = 2048
	// maxStorageRunes bounds every object-storage field. A bucket name is at
	// most 63 characters and an endpoint host far less than this; the bound is
	// here so one field cannot be used to store a document.
	maxStorageRunes = 255
)

// Setting keys as stored in the settings table.
const (
	KeySiteName                  = "site.name"
	KeySiteDescription           = "site.description"
	KeySiteAvatarURL             = "site.avatar_url"
	KeySiteCoverURL              = "site.cover_url"
	KeySiteIconURL               = "site.icon_url"
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

	// Where uploads are kept. The mode is stored explicitly rather than
	// inferred from whether the other fields are filled in: a site that has
	// switched back to the local directory keeps its bucket values so switching
	// forward again does not mean typing them a second time.
	KeyStorageMode      = "storage.mode"
	KeyStorageEndpoint  = "storage.endpoint"
	KeyStorageRegion    = "storage.region"
	KeyStorageBucket    = "storage.bucket"
	KeyStoragePrefix    = "storage.prefix"
	KeyStoragePublicURL = "storage.public_url"
)

// Storage modes. A site that has never saved the storage category follows the
// environment, which is the documented default of a local directory.
const (
	StorageModeLocal  = "local"
	StorageModeObject = "object"
)

// Secret keys live in the secret_settings table and are encrypted with
// BOOP_MASTER_KEY (docs/PRODUCT.md §5.4, docs/DATABASE.md). They are never
// returned by an API and never appear in a log line. Every ciphertext is also
// bound to the key it belongs to as GCM additional data, so a row copied onto
// another key cannot be decrypted.
const (
	SecretKeyGitHubClientID     = "github.client_id"
	SecretKeyGitHubClientSecret = "github.client_secret"
	SecretKeyAIAPIKey           = "ai.api_key"
	// The object-storage credentials are secrets for the same reason an API key
	// is: an R2 token writes to the bucket and has no business being rendered
	// back into a page.
	SecretKeyStorageAccessKeyID     = "storage.access_key_id"
	SecretKeyStorageSecretAccessKey = "storage.secret_access_key"
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
var secretKeys = []string{
	SecretKeyGitHubClientID, SecretKeyGitHubClientSecret, SecretKeyAIAPIKey,
	SecretKeyStorageAccessKeyID, SecretKeyStorageSecretAccessKey,
}

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
	SiteAvatarURL             string
	SiteCoverURL              string
	SiteIconURL               string
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
	StorageMode               string
	StorageEndpoint           string
	StorageRegion             string
	StorageBucket             string
	StoragePrefix             string
	StoragePublicURL          string
}

// Defaults returns the documented default settings.
func Defaults() Values {
	return Values{
		SiteName:                  "Boop",
		SiteDescription:           "遇事开心的个人博客",
		SiteAvatarURL:             "",
		SiteCoverURL:              "",
		SiteIconURL:               "",
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
		StorageMode:               StorageModeLocal,
	}
}

// Seed writes the default value of every known key that has no row yet, in one
// transaction. Existing values are never overwritten.
//
// The storage category is deliberately not seeded. An absent storage row is how
// the site knows it has never saved that category and must keep following
// BOOP_R2_*; writing the default would erase the fact and silently move a
// deployment's uploads to the local directory. Those rows are written by
// ImportStorage when the environment has a bucket to import, and by the first
// save from the settings page.
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
		if storageKey(key) {
			continue
		}
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
// secret write fails with ErrMasterKeyRequired rather than storing plaintext;
// clearing a secret only deletes a row, so it never needs the master key and
// stays idempotent. A request that both writes and clears is still a secret
// write and fails as a whole when the key is missing.
//
// A change to the storage category is also checked as the state it produces
// rather than as it arrives, because some settings only mean something together
// (a bucket without credentials is not a configuration). That check reads the
// stored rows inside the transaction, so a rejected update still writes nothing.
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
	if len(update.Secrets) > 0 && box == nil {
		return ErrMasterKeyRequired
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
		nonce, ciphertext, err := box.Seal(key, value)
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

	if touchesStorage(update) {
		if err := checkStorageAfterUpdate(ctx, tx, update); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidValue, err)
		}
	}

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
// site from a broken one. The ciphertext is bound to its setting key, so a row
// that was copied from another key fails to decrypt.
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
	value, err := box.Open(key, nonce, ciphertext)
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
	case KeySiteAvatarURL, KeySiteCoverURL, KeySiteIconURL:
		asset, err := requireString(key, value)
		if err != nil {
			return err
		}
		return validateAssetURL(key, asset)
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
	case KeyStorageMode:
		mode, err := requireString(key, value)
		if err != nil {
			return err
		}
		if mode != StorageModeLocal && mode != StorageModeObject {
			return fmt.Errorf("settings: set: %s %q is not %s or %s", key, mode, StorageModeLocal, StorageModeObject)
		}
		return nil
	case KeyStorageEndpoint, KeyStorageRegion, KeyStorageBucket, KeyStoragePrefix:
		if key == KeyStorageEndpoint {
			return validateStorageAddress(key, value, maxStorageRunes)
		}
		return validateStorageText(key, value, maxStorageRunes)
	case KeyStoragePublicURL:
		return validateStorageAddress(key, value, maxAssetURLRunes)
	case KeyAuthRegistrationEnabled, KeyCommentsEnabled, KeyCommentsModerationEnabled, KeyAIEnabled:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("settings: set: %s requires a bool, got %T", key, value)
		}
		return nil
	default:
		return fmt.Errorf("settings: set: unknown key %q", key)
	}
}

// validateStorageAddress checks one of the two addresses a bucket needs: the
// signing endpoint and the public read address. An empty value is acceptable —
// the mode decides whether it is required — and a non-empty one has to be an
// absolute http(s) URL without credentials, a query or a fragment.
//
// The shape is checked here, not only when the site switches to the bucket, so
// a value typed into a URL field says so immediately instead of on the day it
// is first used.
func validateStorageAddress(key string, value any, maxRunes int) error {
	address, err := requireString(key, value)
	if err != nil {
		return err
	}
	if err := validateStorageText(key, address, maxRunes); err != nil {
		return err
	}
	return validateBaseURL(key, address)
}

// validateStorageText checks one object-storage field: an empty value is always
// acceptable (the mode decides whether it is required), and a non-empty one is
// a single short line rather than a document.
func validateStorageText(key string, value any, maxRunes int) error {
	s, err := requireString(key, value)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return fmt.Errorf("settings: set: %s must be at most %d characters", key, maxRunes)
	}
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("settings: set: %s must be a single line", key)
	}
	return nil
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

// validateAssetURL backs the three brand image addresses (site avatar, home
// cover and tab icon) with one rule. It accepts an empty value - nothing is
// configured, so the caller falls back - or one of the two shapes such an
// address can take: an absolute http(s) URL with a host and no credentials, or
// a path inside this site starting with a single slash. Both are bounded to a
// sane length, so one setting can never render an off-scheme or unbounded
// resource reference.
//
// The site-relative shape is here for two reasons. The upload endpoint answers
// with one (/uploads/2026/09/<hash>.png), so requiring an absolute URL forced
// every owner to paste their own origin in front of what the API had just
// handed them; and the shipped cover image is a path into the embedded static
// bundle, which has no absolute form until a request says which host it is on.
//
// What the rule guards against decides where the line sits. These values are
// rendered into src and href attributes, so the shapes that must never get
// through are the ones a browser would read as a different origin or as
// something that is not an image: a protocol-relative //host/path (which
// resolves cross-origin while looking local), a backslash, which browsers
// normalise to a slash and which would smuggle the same thing past the check as
// /\host/path, and any scheme at all - data:, javascript: and blob: all lose on
// the scheme test, because only http and https pass it.
func validateAssetURL(key, raw string) error {
	if raw == "" {
		return nil
	}
	if utf8.RuneCountInString(raw) > maxAssetURLRunes {
		return fmt.Errorf("settings: set: %s must be at most %d characters", key, maxAssetURLRunes)
	}
	if strings.HasPrefix(raw, "/") {
		if strings.HasPrefix(raw, "//") {
			return fmt.Errorf("settings: set: %s %q must not be a protocol-relative address", key, raw)
		}
		if strings.ContainsAny(raw, `\ `) {
			return fmt.Errorf("settings: set: %s %q must not contain a backslash or a space", key, raw)
		}
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("settings: set: %s %q is not a URL", key, raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("settings: set: %s %q must use http or https, or start with / for a path on this site", key, raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("settings: set: %s %q has no host", key, raw)
	}
	if parsed.User != nil {
		return fmt.Errorf("settings: set: %s must not contain credentials", key)
	}
	return nil
}

// fieldsOf maps each known key to the matching field of values.
func fieldsOf(values *Values) map[string]any {
	return map[string]any{
		KeySiteName:                  &values.SiteName,
		KeySiteDescription:           &values.SiteDescription,
		KeySiteAvatarURL:             &values.SiteAvatarURL,
		KeySiteCoverURL:              &values.SiteCoverURL,
		KeySiteIconURL:               &values.SiteIconURL,
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
		KeyStorageMode:               &values.StorageMode,
		KeyStorageEndpoint:           &values.StorageEndpoint,
		KeyStorageRegion:             &values.StorageRegion,
		KeyStorageBucket:             &values.StorageBucket,
		KeyStoragePrefix:             &values.StoragePrefix,
		KeyStoragePublicURL:          &values.StoragePublicURL,
	}
}
