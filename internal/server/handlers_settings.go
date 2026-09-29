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
	SiteAvatarURL             string `json:"site_avatar_url"`
	SiteIconURL               string `json:"site_icon_url"`
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
	StorageMode               string `json:"storage_mode"`
	StorageEndpoint           string `json:"storage_endpoint"`
	StorageRegion             string `json:"storage_region"`
	StorageBucket             string `json:"storage_bucket"`
	StoragePrefix             string `json:"storage_prefix"`
	StoragePublicURL          string `json:"storage_public_url"`
	GitHubClientIDSet         bool   `json:"github_client_id_set"`
	GitHubClientSecretSet     bool   `json:"github_client_secret_set"`
	AIAPIKeySet               bool   `json:"ai_api_key_set"`
	StorageAccessKeyIDSet     bool   `json:"storage_access_key_id_set"`
	StorageSecretAccessKeySet bool   `json:"storage_secret_access_key_set"`
	// StorageEnvironment is true while the site has never saved the storage
	// category, so the page can say that BOOP_R2_* is what is in charge.
	StorageEnvironment bool `json:"storage_environment"`
	// The effective fields are where the running process actually keeps
	// uploads. They are read from the live configuration rather than the
	// stored rows, so the page shows a switch that has taken effect instead of
	// one that has merely been written down.
	StorageEffectiveMode      string `json:"storage_effective_mode"`
	StorageEffectiveBucket    string `json:"storage_effective_bucket"`
	StorageEffectivePublicURL string `json:"storage_effective_public_url"`
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
	SiteAvatarURL             *string `json:"site_avatar_url"`
	SiteIconURL               *string `json:"site_icon_url"`
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
	StorageMode               *string `json:"storage_mode"`
	StorageEndpoint           *string `json:"storage_endpoint"`
	StorageRegion             *string `json:"storage_region"`
	StorageBucket             *string `json:"storage_bucket"`
	StoragePrefix             *string `json:"storage_prefix"`
	StoragePublicURL          *string `json:"storage_public_url"`
	GitHubClientID            *string `json:"github_client_id"`
	GitHubClientSecret        *string `json:"github_client_secret"`
	AIAPIKey                  *string `json:"ai_api_key"`
	StorageAccessKeyID        *string `json:"storage_access_key_id"`
	StorageSecretAccessKey    *string `json:"storage_secret_access_key"`
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
	// Where uploads actually go, as opposed to where the rows say they should:
	// the two differ for a site whose configuration is still the environment's.
	effective := s.mediaOpts().Object
	saved, err := settings.StorageConfigured(ctx, s.db)
	if err != nil {
		return settingsPayload{}, err
	}
	mode := settings.StorageModeLocal
	if effective.PublicURL != "" {
		mode = settings.StorageModeObject
	}
	return settingsPayload{
		SiteName:                  values.SiteName,
		SiteDescription:           values.SiteDescription,
		SiteAvatarURL:             values.SiteAvatarURL,
		SiteIconURL:               values.SiteIconURL,
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
		StorageMode:               values.StorageMode,
		StorageEndpoint:           values.StorageEndpoint,
		StorageRegion:             values.StorageRegion,
		StorageBucket:             values.StorageBucket,
		StoragePrefix:             values.StoragePrefix,
		StoragePublicURL:          values.StoragePublicURL,
		GitHubClientIDSet:         configured[settings.SecretKeyGitHubClientID],
		GitHubClientSecretSet:     configured[settings.SecretKeyGitHubClientSecret],
		AIAPIKeySet:               configured[settings.SecretKeyAIAPIKey],
		StorageAccessKeyIDSet:     configured[settings.SecretKeyStorageAccessKeyID],
		StorageSecretAccessKeySet: configured[settings.SecretKeyStorageSecretAccessKey],
		StorageEnvironment:        !saved,
		StorageEffectiveMode:      mode,
		StorageEffectiveBucket:    effective.Bucket,
		StorageEffectivePublicURL: effective.PublicURL,
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
	if body.SiteAvatarURL != nil {
		values[settings.KeySiteAvatarURL] = *body.SiteAvatarURL
	}
	if body.SiteIconURL != nil {
		values[settings.KeySiteIconURL] = *body.SiteIconURL
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
	if body.StorageMode != nil {
		values[settings.KeyStorageMode] = *body.StorageMode
	}
	if body.StorageEndpoint != nil {
		values[settings.KeyStorageEndpoint] = *body.StorageEndpoint
	}
	if body.StorageRegion != nil {
		values[settings.KeyStorageRegion] = *body.StorageRegion
	}
	if body.StorageBucket != nil {
		values[settings.KeyStorageBucket] = *body.StorageBucket
	}
	if body.StoragePrefix != nil {
		values[settings.KeyStoragePrefix] = *body.StoragePrefix
	}
	if body.StoragePublicURL != nil {
		values[settings.KeyStoragePublicURL] = *body.StoragePublicURL
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
		{settings.SecretKeyStorageAccessKeyID, body.StorageAccessKeyID},
		{settings.SecretKeyStorageSecretAccessKey, body.StorageSecretAccessKey},
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

	// The storage configuration is a cached read of the rows just written, so a
	// save that changes it has to re-read them: that is what makes a new
	// backend take effect on the next request instead of the next restart. A
	// failed re-read is reported rather than hidden, because the site would
	// otherwise keep writing to the old place after accepting the change.
	if err := s.refreshStorage(r.Context()); err != nil {
		s.writeSettingsFailure(w, r, "reload storage", err)
		return
	}

	payload, err := s.settingsPayloadOf(r.Context())
	if err != nil {
		s.writeSettingsFailure(w, r, "read settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": payload})
}

// ownerOnlyPage resolves the two refusals every owner-only page shares: a guest
// is sent to the sign-in page and a signed-in reader gets an explicit 403. The
// message is per page because it names what the reader cannot do.
func (s *server) ownerOnlyPage(w http.ResponseWriter, r *http.Request, refusal string) bool {
	state, ok := authStateFrom(r.Context())
	if !ok || !state.authenticated {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return false
	}
	if !state.user.IsOwner() {
		writeFailure(w, r, http.StatusForbidden, "forbidden", refusal)
		return false
	}
	return true
}

// handleAdminIndex sends the owner into the back end: /admin is the entry point
// (docs/API.md HTML 页面) and the first settings category is where it leads.
func (s *server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	if !s.ownerOnlyPage(w, r, "只有站长可以进入管理页面") {
		return
	}
	http.Redirect(w, r, settingsSectionURL(defaultSettingsSection), http.StatusSeeOther)
}

// handleAdminSettingsIndex keeps working the address the single-page form used to
// live at. That page is now one category per page, so the bare address names the
// default one instead of the whole set.
func (s *server) handleAdminSettingsIndex(w http.ResponseWriter, r *http.Request) {
	if !s.ownerOnlyPage(w, r, "只有站长可以修改设置") {
		return
	}
	http.Redirect(w, r, settingsSectionURL(defaultSettingsSection), http.StatusSeeOther)
}

// adminSettingsSections is the settings taxonomy: every category is its own page,
// so the back end has visible structure instead of one long scroll. The order
// here is the order of the tab row.
type adminSettingsSection struct {
	Key   string
	Label string
	// Hint is the line under the page title: what this category decides.
	Hint string
	// Secrets is true for a category whose form carries credential fields, which
	// are the ones a missing BOOP_MASTER_KEY makes unusable.
	Secrets bool
}

const (
	adminSettingsPrefix    = "/admin/settings/"
	defaultSettingsSection = "site"
	// adminSectionSettings and adminSectionComments are the pageView.Admin
	// values, so the shell knows which back-end entry to mark as current.
	adminSectionSettings = "settings"
	adminSectionComments = "comments"
)

var adminSettingsSections = []adminSettingsSection{
	{
		Key:   "site",
		Label: "站点",
		Hint:  "站点名称、简介、头像与图标、时区与每页条数。保存后立刻生效；上传目录、单文件大小与允许的图片类型只能通过环境变量配置，不在这一页。",
	},
	{
		Key:   "registration",
		Label: "注册与评论",
		Hint:  "公开注册与评论的开关。关闭注册后已有账号仍可登录，包括 GitHub 绑定的账号。",
	},
	{
		Key:     "github",
		Label:   "GitHub 登录",
		Secrets: true,
		Hint:    "GitHub OAuth App 的凭据。回调地址必须与 GitHub 上填写的完全一致；密钥只会加密保存，任何页面都不会回显。",
	},
	{
		Key:     "ai",
		Label:   "AI",
		Secrets: true,
		Hint:    "OpenAI-compatible 服务；作者状态与写作助手在配置完成后生效。密钥只会加密保存，任何页面都不会回显。",
	},
	{
		Key:     "storage",
		Label:   "存储",
		Secrets: true,
		Hint:    "上传图片存放的位置。切换存放位置不会移动已经存在的对象，所以换到对象存储之前必须先把本地目录里的对象按原路径搬进桶里；保存后立刻生效，不需要重启。",
	},
}

// settingsSectionOf resolves a path segment to a category.
func settingsSectionOf(key string) (adminSettingsSection, bool) {
	for _, section := range adminSettingsSections {
		if section.Key == key {
			return section, true
		}
	}
	return adminSettingsSection{}, false
}

// settingsSectionURL is the address of one category.
func settingsSectionURL(key string) string { return adminSettingsPrefix + key }

// handleAdminSettingsPage renders one settings category. The page shows only the
// fields of that category, so the form it submits carries only those fields:
// PATCH /api/v1/admin/settings is a partial update, so the rest keep their stored
// values without the page having to round-trip them.
func (s *server) handleAdminSettingsPage(w http.ResponseWriter, r *http.Request) {
	if !s.ownerOnlyPage(w, r, "只有站长可以修改设置") {
		return
	}
	section, ok := settingsSectionOf(r.PathValue("section"))
	if !ok {
		writeFailure(w, r, http.StatusNotFound, "not_found", "没有这个设置分类")
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
		pageView: s.adminShellView(r, adminSectionSettings),
		Settings: payload,
		BaseURL:  s.cfg.BaseURL,
		DataDir:  s.cfg.DataDir,
		Section:  section,
		Tabs:     settingsTabsOf(section.Key),
	})
}

// adminSettingsView drives one settings category. Secret values are absent on
// purpose: the page only learns whether each one is configured.
type adminSettingsView struct {
	pageView
	Settings settingsPayload
	// BaseURL is the configured origin, shown so the owner can copy the exact
	// OAuth callback address.
	BaseURL string
	// DataDir is where the local upload directory lives, named on the storage
	// page so "本地目录" is an address rather than a phrase.
	DataDir string
	// Section is the category this page is, and Tabs is the row that switches
	// between them.
	Section adminSettingsSection
	Tabs    []settingsTab
}

// settingsTab is one entry of the category row.
type settingsTab struct {
	Label  string
	URL    string
	Active bool
}

func settingsTabsOf(active string) []settingsTab {
	tabs := make([]settingsTab, 0, len(adminSettingsSections))
	for _, section := range adminSettingsSections {
		tabs = append(tabs, settingsTab{
			Label:  section.Label,
			URL:    settingsSectionURL(section.Key),
			Active: section.Key == active,
		})
	}
	return tabs
}

// writeSettingsFailure maps a settings error onto the documented envelope.
func (s *server) writeSettingsFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, settings.ErrMasterKeyRequired):
		writeFailure(w, r, http.StatusConflict, "master_key_required", "未配置 BOOP_MASTER_KEY，无法保存密钥")
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
