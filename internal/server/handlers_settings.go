package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"boop/internal/settings"
)

// settingsJSONBytes bounds the admin settings body: every field is a short
// string, a number or a boolean, and one secret is capped at 512 bytes.
const settingsJSONBytes = 64 << 10

// settingsPayload is the documented admin settings shape (docs/API.md §设置与 AI):
// the non-secret values plus whether each secret is configured. A secret value
// is never part of a response.
type settingsPayload struct {
	SiteName                  string `json:"site_name"`
	SiteDescription           string `json:"site_description"`
	SiteTimezone              string `json:"site_timezone"`
	PageSize                  int    `json:"page_size"`
	RegistrationEnabled       bool   `json:"registration_enabled"`
	CommentsEnabled           bool   `json:"comments_enabled"`
	CommentsModerationEnabled bool   `json:"comments_moderation_enabled"`
	AIEnabled                 bool   `json:"ai_enabled"`
	AIBaseURL                 string `json:"ai_base_url"`
	AIChatModel               string `json:"ai_chat_model"`
	AIEmbeddingModel          string `json:"ai_embedding_model"`
	AIAuthorStatusTTLHours    int    `json:"ai_author_status_ttl_hours"`
	GitHubClientIDSet         bool   `json:"github_client_id_set"`
	GitHubClientSecretSet     bool   `json:"github_client_secret_set"`
	AIAPIKeySet               bool   `json:"ai_api_key_set"`
	// MasterKey reports whether BOOP_MASTER_KEY is configured, so the form can
	// explain why the secret fields are unusable.
	MasterKey bool `json:"master_key"`
}

// settingsRequest is the whitelist PATCH body. Every field is optional: an
// absent field keeps its stored value, an empty secret keeps the stored secret,
// and clear_secret names secrets to delete. Fields the page must not change
// (upload directory, file size, allowed MIME types) are absent on purpose.
type settingsRequest struct {
	SiteName                  *string `json:"site_name"`
	SiteDescription           *string `json:"site_description"`
	SiteTimezone              *string `json:"site_timezone"`
	PageSize                  *int    `json:"page_size"`
	RegistrationEnabled       *bool   `json:"registration_enabled"`
	CommentsEnabled           *bool   `json:"comments_enabled"`
	CommentsModerationEnabled *bool   `json:"comments_moderation_enabled"`
	AIEnabled                 *bool   `json:"ai_enabled"`
	AIBaseURL                 *string `json:"ai_base_url"`
	AIChatModel               *string `json:"ai_chat_model"`
	AIEmbeddingModel          *string `json:"ai_embedding_model"`
	AIAuthorStatusTTLHours    *int    `json:"ai_author_status_ttl_hours"`
	GitHubClientID            *string `json:"github_client_id"`
	GitHubClientSecret        *string `json:"github_client_secret"`
	AIAPIKey                  *string `json:"ai_api_key"`
	// ClearSecret lists secret keys to delete, for example
	// ["github.client_secret"].
	ClearSecret []string `json:"clear_secret"`
}

// settingsPayloadOf reads the stored settings and secret availability. It fails
// instead of falling back to defaults: a form filled with defaults could
// overwrite the real settings with wrong values.
func (s *server) settingsPayloadOf(ctx context.Context) (settingsPayload, error) {
	values, err := settings.Load(ctx, s.db)
	if err != nil {
		return settingsPayload{}, err
	}
	configured, err := settings.ConfiguredSecrets(ctx, s.db)
	if err != nil {
		return settingsPayload{}, err
	}
	return settingsPayload{
		SiteName:                  values.SiteName,
		SiteDescription:           values.SiteDescription,
		SiteTimezone:              values.SiteTimezone,
		PageSize:                  values.PageSize,
		RegistrationEnabled:       values.RegistrationEnabled,
		CommentsEnabled:           values.CommentsEnabled,
		CommentsModerationEnabled: values.CommentsModerationEnabled,
		AIEnabled:                 values.AIEnabled,
		AIBaseURL:                 values.AIBaseURL,
		AIChatModel:               values.AIChatModel,
		AIEmbeddingModel:          values.AIEmbeddingModel,
		AIAuthorStatusTTLHours:    values.AIAuthorStatusTTLHours,
		GitHubClientIDSet:         configured[settings.SecretKeyGitHubClientID],
		GitHubClientSecretSet:     configured[settings.SecretKeyGitHubClientSecret],
		AIAPIKeySet:               configured[settings.SecretKeyAIAPIKey],
		MasterKey:                 s.secrets != nil,
	}, nil
}

// handleAdminSettingsAPI returns the current settings. Guests get 401 and
// readers 403, exactly like the other admin endpoints.
func (s *server) handleAdminSettingsAPI(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOwner(w, r); !ok {
		return
	}
	payload, err := s.settingsPayloadOf(r.Context())
	if err != nil {
		s.writeSettingsFailure(w, r, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload})
}

// handlePatchSettingsAPI applies a whitelisted settings change in one
// transaction. Secrets are encrypted with BOOP_MASTER_KEY before they are
// written, and the answer repeats the availability flags rather than any value.
func (s *server) handlePatchSettingsAPI(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOwner(w, r); !ok {
		return
	}
	var body settingsRequest
	if !s.readJSON(w, r, &body, settingsJSONBytes) {
		return
	}

	values := map[string]any{}
	if body.SiteName != nil {
		values[settings.KeySiteName] = *body.SiteName
	}
	if body.SiteDescription != nil {
		values[settings.KeySiteDescription] = *body.SiteDescription
	}
	if body.SiteTimezone != nil {
		values[settings.KeySiteTimezone] = *body.SiteTimezone
	}
	if body.PageSize != nil {
		values[settings.KeyContentPageSize] = *body.PageSize
	}
	if body.RegistrationEnabled != nil {
		values[settings.KeyAuthRegistrationEnabled] = *body.RegistrationEnabled
	}
	if body.CommentsEnabled != nil {
		values[settings.KeyCommentsEnabled] = *body.CommentsEnabled
	}
	if body.CommentsModerationEnabled != nil {
		values[settings.KeyCommentsModerationEnabled] = *body.CommentsModerationEnabled
	}
	if body.AIEnabled != nil {
		values[settings.KeyAIEnabled] = *body.AIEnabled
	}
	if body.AIBaseURL != nil {
		values[settings.KeyAIBaseURL] = *body.AIBaseURL
	}
	if body.AIChatModel != nil {
		values[settings.KeyAIChatModel] = *body.AIChatModel
	}
	if body.AIEmbeddingModel != nil {
		values[settings.KeyAIEmbeddingModel] = *body.AIEmbeddingModel
	}
	if body.AIAuthorStatusTTLHours != nil {
		values[settings.KeyAIAuthorStatusTTLHours] = *body.AIAuthorStatusTTLHours
	}

	// An empty secret means "keep the stored value" (docs/API.md), so only a
	// non-empty field is written.
	secrets := map[string]string{}
	for _, candidate := range []struct {
		key   string
		value *string
	}{
		{settings.SecretKeyGitHubClientID, body.GitHubClientID},
		{settings.SecretKeyGitHubClientSecret, body.GitHubClientSecret},
		{settings.SecretKeyAIAPIKey, body.AIAPIKey},
	} {
		if candidate.value != nil && *candidate.value != "" {
			secrets[candidate.key] = *candidate.value
		}
	}

	err := settings.Apply(r.Context(), s.db, s.secrets, settings.Update{
		Values:  values,
		Secrets: secrets,
		Clear:   body.ClearSecret,
	})
	if err != nil {
		s.writeSettingsFailure(w, r, "update settings", err)
		return
	}

	payload, err := s.settingsPayloadOf(r.Context())
	if err != nil {
		s.writeSettingsFailure(w, r, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload})
}

// handleAdminIndex sends the owner to the settings page: /admin is the admin
// entry point (docs/API.md HTML 页面) and settings is where it leads.
func (s *server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	if !state.user.IsOwner() {
		writeFailure(w, r, http.StatusForbidden, "forbidden", "只有站长可以进入管理页面")
		return
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

// handleAdminSettingsPage renders the settings form. Guests are sent to the
// sign-in page and signed-in readers get an explicit 403, like the moderation
// page.
func (s *server) handleAdminSettingsPage(w http.ResponseWriter, r *http.Request) {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	if !state.user.IsOwner() {
		writeFailure(w, r, http.StatusForbidden, "forbidden", "只有站长可以修改设置")
		return
	}

	payload, err := s.settingsPayloadOf(r.Context())
	if err != nil {
		// Fail closed: a form filled from defaults could overwrite the real
		// settings with wrong values.
		s.logger.LogAttrs(r.Context(), slog.LevelError, "settings unavailable",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
		return
	}
	s.render(w, r, http.StatusOK, "admin_settings", adminSettingsView{
		pageView: s.shellView(r, navNeutralFilter),
		Settings: payload,
		BaseURL:  s.cfg.BaseURL,
	})
}

// adminSettingsView drives the settings form. Secret values are absent on
// purpose: the page only learns whether each one is configured.
type adminSettingsView struct {
	pageView
	Settings settingsPayload
	// BaseURL is the configured origin, shown so the owner can copy the exact
	// OAuth callback address.
	BaseURL string
}

// writeSettingsFailure maps a settings error onto the documented envelope.
func (s *server) writeSettingsFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, settings.ErrMasterKeyRequired):
		writeFailure(w, r, http.StatusConflict, "master_key_required", "未配置 BOOP_MASTER_KEY，无法保存或清除密钥")
	case errors.Is(err, settings.ErrInvalidValue):
		writeFailure(w, r, http.StatusBadRequest, "invalid_settings", "设置值不合法："+settingsReason(err))
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, operation+" failed",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		writeFailure(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	}
}

// settingsReason strips the package prefixes from a validation failure, so the
// owner sees which key was refused and why. A secret value never appears in one:
// the settings package validates keys and lengths, not stored values.
func settingsReason(err error) string {
	message := err.Error()
	for _, prefix := range []string{
		settings.ErrInvalidValue.Error() + ": ",
		"settings: set: ",
		"settings: ",
	} {
		message = strings.TrimPrefix(message, prefix)
	}
	return message
}
