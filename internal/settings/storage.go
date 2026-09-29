package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"boop/internal/media"
	"boop/internal/secretbox"
)

// Storage is where the process keeps uploads: the mode, the fields the object
// backend needs, and the credentials that sign a write. It is assembled either
// from the settings table or, on a site that has never saved the category, from
// the environment.
//
// The credentials are plaintext here because this is the value that ends up in
// the running configuration; they are never rendered back into a page and never
// written to a log line.
type Storage struct {
	Mode            string
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	PublicURL       string
	AccessKeyID     string
	SecretAccessKey string
}

// Credentials reports which object-storage credentials have a stored row.
// Validation needs only their existence: a form leaves an untouched secret out
// of the body, so whether the object store is complete is a question about the
// stored rows rather than about one request.
type Credentials struct {
	AccessKeyID     bool
	SecretAccessKey bool
}

// Object reports whether uploads go to a bucket.
func (s Storage) Object() bool { return s.Mode == StorageModeObject }

// ObjectOptions maps the selection onto the media layer's own description of a
// bucket, so the shape of the configuration is validated — and exactly once —
// by the code that has to use it. The zero value means the local directory.
func (s Storage) ObjectOptions() media.ObjectOptions {
	if !s.Object() {
		return media.ObjectOptions{}
	}
	return media.ObjectOptions{
		Endpoint: strings.TrimSpace(s.Endpoint),
		Region:   strings.TrimSpace(s.Region),
		Bucket:   strings.TrimSpace(s.Bucket),
		// The prefix is written as a path in the form and stored with its
		// slashes, the way documentation writes it; the signing code addresses
		// keys with the bare form.
		Prefix:    strings.Trim(strings.TrimSpace(s.Prefix), "/"),
		PublicURL: strings.TrimSpace(s.PublicURL),
		AccessKey: s.AccessKeyID,
		SecretKey: s.SecretAccessKey,
	}
}

// Validate refuses a selection that cannot work: an unknown mode, or an object
// backend with a field missing. The wording is the one config.validateStorage
// already uses for the same mistake made through the environment.
func (s Storage) Validate(credentials Credentials) error {
	if s.Mode != StorageModeLocal && s.Mode != StorageModeObject {
		return fmt.Errorf("storage mode %q is neither %s nor %s", s.Mode, StorageModeLocal, StorageModeObject)
	}
	if !s.Object() {
		return nil
	}
	var missing []string
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"endpoint", strings.TrimSpace(s.Endpoint) != ""},
		{"bucket", strings.TrimSpace(s.Bucket) != ""},
		{"public URL", strings.TrimSpace(s.PublicURL) != ""},
		{"access key id", credentials.AccessKeyID},
		{"secret access key", credentials.SecretAccessKey},
	} {
		if !field.set {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("object storage is enabled but incomplete, missing %s", strings.Join(missing, ", "))
	}
	return s.ObjectOptions().Validate()
}

// Storage assembles the non-secret half of the selection. The credentials live
// encrypted in another table, so they are filled in by whoever read them.
func (v Values) Storage() Storage {
	return Storage{
		Mode:      v.StorageMode,
		Endpoint:  v.StorageEndpoint,
		Region:    v.StorageRegion,
		Bucket:    v.StorageBucket,
		Prefix:    v.StoragePrefix,
		PublicURL: v.StoragePublicURL,
	}
}

// ReadStorage returns the storage configuration the process must use.
//
// A site that has saved the storage category owns its configuration, and the
// page is the only thing that changes it. A site that never has follows the
// environment: that is what keeps a deployment configured with nothing but
// BOOP_R2_* working, because its credentials cannot be imported without
// BOOP_MASTER_KEY and quietly falling back to the local directory would send
// new uploads somewhere the site's published images are not.
func ReadStorage(ctx context.Context, db *sql.DB, box *secretbox.Box, environment Storage) (Storage, error) {
	if db == nil {
		return Storage{}, errors.New("settings: read storage: nil database")
	}
	configured, err := storageConfigured(ctx, db)
	if err != nil {
		return Storage{}, err
	}
	if !configured {
		return environment, nil
	}

	values, err := Load(ctx, db)
	if err != nil {
		return Storage{}, err
	}
	storage := values.Storage()
	if storage.AccessKeyID, err = readStoredSecret(ctx, db, box, SecretKeyStorageAccessKeyID); err != nil {
		return Storage{}, err
	}
	if storage.SecretAccessKey, err = readStoredSecret(ctx, db, box, SecretKeyStorageSecretAccessKey); err != nil {
		return Storage{}, err
	}
	return storage, nil
}

// ImportStorage writes an object-storage configuration into the settings table
// for a site that has never saved one, so a deployment configured through
// BOOP_R2_* keeps working after this category exists and its settings page can
// show what it is using. It reports whether it wrote anything, and writes
// nothing at all once the page has saved the category.
func ImportStorage(ctx context.Context, db *sql.DB, box *secretbox.Box, storage Storage) (bool, error) {
	if db == nil {
		return false, errors.New("settings: import storage: nil database")
	}
	configured, err := storageConfigured(ctx, db)
	if err != nil {
		return false, err
	}
	if configured {
		return false, nil
	}

	update := Update{Values: map[string]any{
		KeyStorageMode:      storage.Mode,
		KeyStorageEndpoint:  storage.Endpoint,
		KeyStorageRegion:    storage.Region,
		KeyStorageBucket:    storage.Bucket,
		KeyStoragePrefix:    storage.Prefix,
		KeyStoragePublicURL: storage.PublicURL,
	}}
	secrets := map[string]string{}
	if storage.AccessKeyID != "" {
		secrets[SecretKeyStorageAccessKeyID] = storage.AccessKeyID
	}
	if storage.SecretAccessKey != "" {
		secrets[SecretKeyStorageSecretAccessKey] = storage.SecretAccessKey
	}
	if len(secrets) > 0 {
		update.Secrets = secrets
	}

	if err := Apply(ctx, db, box, update); err != nil {
		return false, err
	}
	return true, nil
}

// readStoredSecret reads a credential that may legitimately be absent: a site
// on the local directory has none, and a site whose master key was removed
// still reads its uploaded images (reading a bucket needs the public address,
// not the signing key). Anything else — a corrupt row, a failed decrypt — is an
// error rather than an empty credential.
func readStoredSecret(ctx context.Context, db *sql.DB, box *secretbox.Box, key string) (string, error) {
	value, err := ReadSecret(ctx, db, box, key)
	switch {
	case err == nil:
		return value, nil
	case errors.Is(err, ErrSecretNotFound), errors.Is(err, ErrMasterKeyRequired):
		return "", nil
	default:
		return "", err
	}
}

// queryer is the one method the read-only helpers need, so they work both on
// the pool and inside a caller's transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// StorageConfigured reports whether the site has ever saved the storage
// category. The mode row is the marker: it is written by the first save and by
// the import, and an absent row is what "follow the environment" means. A page
// says which of the two is in charge, so this is asked separately from
// ReadStorage, which answers what the value is.
func StorageConfigured(ctx context.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, errors.New("settings: read storage mode: nil database")
	}
	return storageConfigured(ctx, db)
}

// storageConfigured is the query behind StorageConfigured and the import, so
// both ask the same question of the same row.
func storageConfigured(ctx context.Context, q queryer) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT 1 FROM settings WHERE key = ?`, KeyStorageMode)
	if err != nil {
		return false, fmt.Errorf("settings: read storage mode: %w", err)
	}
	defer rows.Close()
	configured := rows.Next()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("settings: read storage mode: %w", err)
	}
	return configured, nil
}

// storedSecretKeys lists the secret rows that exist, without decrypting them.
func storedSecretKeys(ctx context.Context, q queryer) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT key FROM secret_settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: list secrets: %w", err)
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("settings: list secrets: %w", err)
		}
		keys[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings: list secrets: %w", err)
	}
	return keys, nil
}

// storagePrefix names every key the storage category owns. The category is
// identified by its prefix rather than by a list, so a field added to it cannot
// be forgotten in the two places that care: Seed must not write them, and Apply
// must re-check the merged state when one of them changes.
const storagePrefix = "storage."

// storageKey reports whether a key belongs to the storage category.
func storageKey(key string) bool { return strings.HasPrefix(key, storagePrefix) }

// touchesStorage reports whether an update changes anything the storage
// category owns. Only such an update can turn a working configuration into a
// half-configured one, so only such an update reads the merge.
func touchesStorage(update Update) bool {
	for key := range update.Values {
		if storageKey(key) {
			return true
		}
	}
	for key := range update.Secrets {
		if storageKey(key) {
			return true
		}
	}
	for _, key := range update.Clear {
		if storageKey(key) {
			return true
		}
	}
	return false
}

// checkStorageAfterUpdate validates the storage category as it will be once the
// update lands. It runs inside the caller's transaction and only reads, so a
// refusal still leaves every row where it was.
func checkStorageAfterUpdate(ctx context.Context, tx *sql.Tx, update Update) error {
	effective, err := LoadTx(ctx, tx)
	if err != nil {
		return err
	}
	fields := fieldsOf(&effective)
	for key, value := range update.Values {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("settings: apply: encode %s: %w", key, err)
		}
		// The type was checked before the transaction opened, so this only
		// fails for a key that has no field, which fieldsOf already ruled out.
		if err := json.Unmarshal(raw, fields[key]); err != nil {
			return fmt.Errorf("settings: apply: %s: %w", key, err)
		}
	}

	configured, err := storedSecretKeys(ctx, tx)
	if err != nil {
		return err
	}
	for key := range update.Secrets {
		configured[key] = true
	}
	for _, key := range update.Clear {
		delete(configured, key)
	}

	return effective.Storage().Validate(Credentials{
		AccessKeyID:     configured[SecretKeyStorageAccessKeyID],
		SecretAccessKey: configured[SecretKeyStorageSecretAccessKey],
	})
}
