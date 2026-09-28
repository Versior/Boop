// Package settings stores the documented non-secret site settings as typed
// values backed by the settings table.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	values := Defaults()
	fields := fieldsOf(&values)

	rows, err := db.QueryContext(ctx, `SELECT key, value_json FROM settings`)
	if err != nil {
		return Values{}, fmt.Errorf("settings: load: %w", err)
	}
	defer rows.Close()

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

// Set stores one known key as JSON, inserting or replacing the row.
func Set(ctx context.Context, db *sql.DB, key string, value any) error {
	if db == nil {
		return errors.New("settings: set: nil database")
	}
	var probe Values
	if _, known := fieldsOf(&probe)[key]; !known {
		return fmt.Errorf("settings: set: unknown key %q", key)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("settings: set: encode %s: %w", key, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO settings(key, value_json, updated_at)
		VALUES(?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		key, string(raw)); err != nil {
		return fmt.Errorf("settings: set: %s: %w", key, err)
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
