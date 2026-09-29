package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boop/internal/secretbox"
	"boop/internal/settings"
)

const (
	settingsClientID     = "github-client-id-value"
	settingsClientSecret = "github-client-secret-value"
	settingsAPIKey       = "sk-test-api-key-value"
)

// newSettingsFixture adds a configured master key, so the secret paths are
// reachable.
func newSettingsFixture(t *testing.T) *authFixture {
	t.Helper()
	cfg := testConfig()
	cfg.MasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	f := newAuthFixtureWithConfig(t, cfg)
	if err := settings.Seed(t.Context(), f.db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	return f
}

// settingsBox rebuilds the encryption box the server holds.
func settingsBox(t *testing.T, f *authFixture) *secretbox.Box {
	t.Helper()
	box, err := secretbox.NewFromBase64(f.cfg.MasterKey)
	if err != nil {
		t.Fatalf("secretbox.NewFromBase64: %v", err)
	}
	return box
}

func (f *authFixture) patchSettings(t *testing.T, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{"X-CSRF-Token": csrf}
	return f.do(t, http.MethodPatch, "/api/v1/admin/settings", body, headers, cookie)
}

func (f *authFixture) getSettings(t *testing.T, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodGet, "/api/v1/admin/settings", "", nil, cookie)
}

func (f *authFixture) settingsData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return decodeData(t, rec)
}

func TestAdminSettingsAPIRequiresTheOwner(t *testing.T) {
	f := newSettingsFixture(t)
	f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	readerCookie := sessionCookie(t, f.register(t, "reader@example.com", "读者甲"))

	for _, tt := range []struct {
		name   string
		method string
		body   string
		cookie *http.Cookie
		csrf   string
		status int
		code   string
	}{
		{"guest read", http.MethodGet, "", nil, "", http.StatusUnauthorized, "unauthorized"},
		{"guest write", http.MethodPatch, `{"site_name":"x"}`, nil, "", http.StatusUnauthorized, "unauthorized"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, tt.method, "/api/v1/admin/settings", tt.body, map[string]string{"Origin": testOrigin}, tt.cookie)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			if code, _, _ := decodeAPIError(t, rec); code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
		})
	}

	rec := f.do(t, http.MethodGet, "/api/v1/admin/settings", "", nil, readerCookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader read: status = %d, want 403", rec.Code)
	}
	if code, _, _ := decodeAPIError(t, rec); code != "forbidden" {
		t.Errorf("code = %q, want forbidden", code)
	}
}

func TestAdminSettingsAPIReportsAvailabilityWithoutValues(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie := sessionCookie(t, login)

	if err := settings.Apply(t.Context(), f.db, settingsBox(t, f), settings.Update{
		Secrets: map[string]string{
			settings.SecretKeyGitHubClientID:     settingsClientID,
			settings.SecretKeyGitHubClientSecret: settingsClientSecret,
			settings.SecretKeyAIAPIKey:           settingsAPIKey,
		},
	}); err != nil {
		t.Fatalf("store secrets: %v", err)
	}

	rec := f.getSettings(t, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := f.settingsData(t, rec)
	for key, want := range map[string]any{
		"github_client_id_set":     true,
		"github_client_secret_set": true,
		"ai_api_key_set":           true,
		"master_key":               true,
		"site_name":                "Boop",
		"page_size":                float64(20),
	} {
		if got := data[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	// No secret value may ever appear in a response.
	body := rec.Body.String()
	for _, secret := range []string{settingsClientID, settingsClientSecret, settingsAPIKey} {
		if strings.Contains(body, secret) {
			t.Errorf("the settings response leaks %q", secret)
		}
	}
}

func TestPatchSettingsUpdatesValues(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	rec := f.patchSettings(t, `{"site_name":"少爷的博客","site_description":"海边","site_avatar_url":"https://cdn.example.com/avatar.png","site_icon_url":"https://cdn.example.com/favicon.png","site_timezone":"UTC",
		"page_size":42,"registration_enabled":false,"comments_enabled":false,"comments_moderation_enabled":true,
		"ai_enabled":true,"ai_base_url":"https://api.example.com/v1","ai_chat_model":"gpt-4o-mini",
		"ai_embedding_model":"text-embedding-3-small","ai_author_status_ttl_hours":24}`, cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := f.settingsData(t, rec)
	for key, want := range map[string]any{
		"site_name":                   "少爷的博客",
		"site_description":            "海边",
		"site_avatar_url":             "https://cdn.example.com/avatar.png",
		"site_icon_url":               "https://cdn.example.com/favicon.png",
		"site_timezone":               "UTC",
		"page_size":                   float64(42),
		"registration_enabled":        false,
		"comments_enabled":            false,
		"comments_moderation_enabled": true,
		"ai_enabled":                  true,
		"ai_base_url":                 "https://api.example.com/v1",
		"ai_chat_model":               "gpt-4o-mini",
		"ai_embedding_model":          "text-embedding-3-small",
		"ai_author_status_ttl_hours":  float64(24),
		"github_client_id_set":        false,
		"github_client_secret_set":    false,
		"ai_api_key_set":              false,
		"master_key":                  true,
	} {
		if got := data[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}

	// The stored values are the ones the API reports, not only the response echo.
	values, err := settings.Load(t.Context(), f.db)
	if err != nil {
		t.Fatalf("settings.Load: %v", err)
	}
	if values.SiteName != "少爷的博客" || values.PageSize != 42 || values.RegistrationEnabled || !values.CommentsModerationEnabled {
		t.Errorf("stored values = %+v", values)
	}
}

// TestPatchSettingsRejectsAHalfAppliedUpdate pins the atomicity rule: one bad
// field must leave every other field untouched.
func TestPatchSettingsRejectsAHalfAppliedUpdate(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	rec := f.patchSettings(t, `{"site_name":"新名字","page_size":99}`, cookie, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	code, message, _ := decodeAPIError(t, rec)
	if code != "invalid_settings" {
		t.Errorf("code = %q, want invalid_settings", code)
	}
	if !strings.Contains(message, "page_size") {
		t.Errorf("message = %q, want it to name the refused key", message)
	}
	values, err := settings.Load(t.Context(), f.db)
	if err != nil {
		t.Fatalf("settings.Load: %v", err)
	}
	if values.SiteName != "Boop" {
		t.Errorf("site_name = %q, want the stored value unchanged", values.SiteName)
	}
}

func TestPatchSettingsRejectsUnknownFieldsAndValues(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	for _, tt := range []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"unknown field", `{"upload_dir":"/etc"}`, http.StatusBadRequest, "invalid_body"},
		{"wrong type", `{"page_size":"20"}`, http.StatusBadRequest, "invalid_body"},
		{"blank site name", `{"site_name":"   "}`, http.StatusBadRequest, "invalid_settings"},
		{"unknown timezone", `{"site_timezone":"Mars/Olympus"}`, http.StatusBadRequest, "invalid_settings"},
		{"relative avatar", `{"site_avatar_url":"/avatar.png"}`, http.StatusBadRequest, "invalid_settings"},
		{"avatar with a wrong scheme", `{"site_avatar_url":"javascript:alert(1)"}`, http.StatusBadRequest, "invalid_settings"},
		{"avatar with credentials", `{"site_avatar_url":"https://user:pass@example.com/a.png"}`, http.StatusBadRequest, "invalid_settings"},
		{"relative icon", `{"site_icon_url":"/favicon.png"}`, http.StatusBadRequest, "invalid_settings"},
		{"icon with a wrong scheme", `{"site_icon_url":"javascript:alert(1)"}`, http.StatusBadRequest, "invalid_settings"},
		{"icon with credentials", `{"site_icon_url":"https://user:pass@example.com/favicon.png"}`, http.StatusBadRequest, "invalid_settings"},
		{"unknown secret to clear", `{"clear_secret":["github.nope"]}`, http.StatusBadRequest, "invalid_settings"},
		{"not an object", `[]`, http.StatusBadRequest, "invalid_body"},
		{"trailing content", `{"site_name":"a"}{"site_name":"b"}`, http.StatusBadRequest, "invalid_body"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.patchSettings(t, tt.body, cookie, csrf)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			if code, _, _ := decodeAPIError(t, rec); code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
		})
	}
	if secrets := f.countRows(t, "secret_settings"); secrets != 0 {
		t.Errorf("secret_settings = %d, want none", secrets)
	}
}

func TestPatchSettingsStoresSecretsEncrypted(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	rec := f.patchSettings(t, `{"github_client_id":"`+settingsClientID+`","github_client_secret":"`+settingsClientSecret+`","ai_api_key":"`+settingsAPIKey+`"}`, cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{settingsClientID, settingsClientSecret, settingsAPIKey} {
		if strings.Contains(body, secret) {
			t.Errorf("the write response leaks %q", secret)
		}
	}
	if data := f.settingsData(t, rec); data["github_client_id_set"] != true || data["ai_api_key_set"] != true {
		t.Errorf("availability flags = %#v", data)
	}

	// The stored row holds a nonce and ciphertext, never the plaintext.
	var nonce, ciphertext []byte
	err := f.db.QueryRow(`SELECT nonce, ciphertext FROM secret_settings WHERE key = ?`, settings.SecretKeyGitHubClientSecret).
		Scan(&nonce, &ciphertext)
	if err != nil {
		t.Fatalf("read secret row: %v", err)
	}
	if len(nonce) != secretbox.NonceBytes {
		t.Errorf("nonce = %d bytes, want %d", len(nonce), secretbox.NonceBytes)
	}
	if bytes.Contains(ciphertext, []byte(settingsClientSecret)) {
		t.Error("the stored ciphertext contains the plaintext")
	}
	// The server can decrypt it again with BOOP_MASTER_KEY.
	stored, err := settings.ReadSecret(t.Context(), f.db, settingsBox(t, f), settings.SecretKeyGitHubClientSecret)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if stored != settingsClientSecret {
		t.Errorf("stored secret = %q, want the submitted value", stored)
	}

	// A later read still refuses to echo it.
	read := f.getSettings(t, cookie)
	for _, secret := range []string{settingsClientID, settingsClientSecret, settingsAPIKey} {
		if strings.Contains(read.Body.String(), secret) {
			t.Errorf("the settings response leaks %q", secret)
		}
	}
}

func TestPatchSettingsKeepsAnEmptySecretAndClearsOnRequest(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	if rec := f.patchSettings(t, `{"github_client_secret":"`+settingsClientSecret+`","ai_api_key":"`+settingsAPIKey+`"}`, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("first write: %d %s", rec.Code, rec.Body.String())
	}
	// An empty secret keeps the stored value (docs/API.md): it is not a delete.
	if rec := f.patchSettings(t, `{"github_client_secret":"","ai_api_key":""}`, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("empty secret: %d %s", rec.Code, rec.Body.String())
	}
	stored, err := settings.ReadSecret(t.Context(), f.db, settingsBox(t, f), settings.SecretKeyGitHubClientSecret)
	if err != nil {
		t.Fatalf("ReadSecret after an empty field: %v", err)
	}
	if stored != settingsClientSecret {
		t.Errorf("stored secret = %q, want it unchanged", stored)
	}

	// Only an explicit clear_secret deletes.
	rec := f.patchSettings(t, `{"clear_secret":["github.client_secret"]}`, cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	data := f.settingsData(t, rec)
	if data["github_client_secret_set"] != false {
		t.Errorf("github_client_secret_set = %#v, want false", data["github_client_secret_set"])
	}
	if data["ai_api_key_set"] != true {
		t.Errorf("clearing one secret must not touch another: %#v", data["ai_api_key_set"])
	}
	if _, err := settings.ReadSecret(t.Context(), f.db, settingsBox(t, f), settings.SecretKeyGitHubClientSecret); err == nil {
		t.Error("the cleared secret is still readable")
	}
	// Clearing again is harmless.
	if rec := f.patchSettings(t, `{"clear_secret":["github.client_secret"]}`, cookie, csrf); rec.Code != http.StatusOK {
		t.Errorf("second clear: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPatchSettingsWithoutMasterKeyFailsClosed covers the documented rules: a
// missing BOOP_MASTER_KEY never results in a plaintext secret, and clearing a
// stored secret needs no key because it only deletes a row.
func TestPatchSettingsWithoutMasterKeyFailsClosed(t *testing.T) {
	f := newAuthFixture(t)
	if err := settings.Seed(t.Context(), f.db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	for _, body := range []string{
		`{"github_client_secret":"` + settingsClientSecret + `"}`,
		`{"clear_secret":["github.client_secret"],"ai_api_key":"` + settingsAPIKey + `"}`,
	} {
		rec := f.patchSettings(t, body, cookie, csrf)
		if rec.Code != http.StatusConflict {
			t.Fatalf("body %s: status = %d, want 409: %s", body, rec.Code, rec.Body.String())
		}
		if code, _, _ := decodeAPIError(t, rec); code != "master_key_required" {
			t.Errorf("code = %q, want master_key_required", code)
		}
	}
	if secrets := f.countRows(t, "secret_settings"); secrets != 0 {
		t.Errorf("secret_settings = %d, want none", secrets)
	}
	if data := f.settingsData(t, f.getSettings(t, cookie)); data["master_key"] != false {
		t.Errorf("master_key = %#v, want false", data["master_key"])
	}

	// Clearing is a delete: without a master key it still succeeds, and repeating
	// it is harmless.
	for i := 0; i < 2; i++ {
		rec := f.patchSettings(t, `{"clear_secret":["github.client_secret","ai.api_key"]}`, cookie, csrf)
		if rec.Code != http.StatusOK {
			t.Fatalf("clear attempt %d: status = %d, want 200: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	// Non-secret settings stay editable without a master key.
	if rec := f.patchSettings(t, `{"site_name":"没有主密钥也能改"}`, cookie, csrf); rec.Code != http.StatusOK {
		t.Errorf("non-secret write: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPublishedBrandComesFromSettings is the end-to-end brand check: after a
// PATCH the rendered home page carries the new name, description and avatar, and
// no user-visible surface still shows the built-in name.
func TestPublishedBrandComesFromSettings(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	const avatarURL = "https://cdn.example.com/site-avatar.png"
	rec := f.patchSettings(t, `{"site_name":"少爷的博客","site_description":"海边的个人博客","site_avatar_url":"`+avatarURL+`"}`, cookie, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	home := f.do(t, http.MethodGet, "/", "", nil, cookie)
	if home.Code != http.StatusOK {
		t.Fatalf("home: status = %d, want 200", home.Code)
	}
	body := home.Body.String()
	for _, want := range []string{
		"<title>少爷的博客</title>",
		`<meta name="description" content="海边的个人博客">`,
		`<link rel="icon" href="` + avatarURL + `">`,
		`<span class="brand-name">少爷的博客</span>`,
		`aria-label="少爷的博客 首页"`,
		"少爷的博客 首页",
		"少爷的博客 上只有站长会发布内容",
		`<img class="avatar-img" src="` + avatarURL + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the home page is missing %s", want)
		}
	}
	// The right rail carries no search form any more, so the search page owns the
	// only search label and placeholder. The brand still has to reach them.
	search := f.do(t, http.MethodGet, "/search", "", nil, cookie)
	if search.Code != http.StatusOK {
		t.Fatalf("search: status = %d, want 200", search.Code)
	}
	for _, want := range []string{
		`<label class="sr-only" for="search-input">搜索 少爷的博客</label>`,
		`placeholder="搜索 少爷的博客"`,
	} {
		if !strings.Contains(search.Body.String(), want) {
			t.Errorf("the search page is missing %s", want)
		}
	}
	// The configured name replaced the built-in one everywhere on this page.
	if strings.Contains(body, "Boop") {
		t.Error("the home page still renders the built-in brand name")
	}
	// The other HTML pages take the same brand.
	for _, target := range []string{"/login", "/register"} {
		page := f.do(t, http.MethodGet, target, "", nil, nil)
		if page.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", target, page.Code)
		}
		if !strings.Contains(page.Body.String(), "<title>少爷的博客</title>") {
			t.Errorf("%s does not use the configured site name in its title", target)
		}
	}
}

// TestAuthorAvatarFallback pins the documented order: the configured site avatar
// wins, an empty setting falls back to the owner account, and with neither the
// built-in SVG is rendered.
func TestAuthorAvatarFallback(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	const (
		siteAvatar  = "https://cdn.example.com/site-avatar.png"
		ownerAvatar = "https://cdn.example.com/owner-avatar.png"
	)
	setAvatar := func(value string) {
		t.Helper()
		rec := f.patchSettings(t, `{"site_avatar_url":"`+value+`"}`, cookie, csrf)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch site_avatar_url: status = %d: %s", rec.Code, rec.Body.String())
		}
	}
	home := func() string {
		t.Helper()
		rec := f.do(t, http.MethodGet, "/", "", nil, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("home: status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	setAvatar(siteAvatar)
	if body := home(); !strings.Contains(body, siteAvatar) {
		t.Error("the configured site avatar is not rendered")
	}

	// An empty setting falls back to the owner account's avatar.
	if _, err := f.db.Exec(`UPDATE users SET avatar_url = ? WHERE id = ?`, ownerAvatar, owner.ID); err != nil {
		t.Fatalf("set the owner avatar: %v", err)
	}
	setAvatar("")
	body := home()
	if !strings.Contains(body, ownerAvatar) {
		t.Error("the owner account avatar is not used as the fallback")
	}
	if strings.Contains(body, siteAvatar) {
		t.Error("the cleared site avatar is still rendered")
	}

	// With neither avatar the built-in SVG keeps the layout intact.
	if _, err := f.db.Exec(`UPDATE users SET avatar_url = '' WHERE id = ?`, owner.ID); err != nil {
		t.Fatalf("clear the owner avatar: %v", err)
	}
	body = home()
	if !strings.Contains(body, `href="#i-avatar"`) {
		t.Error("the built-in avatar placeholder is missing")
	}
	if strings.Contains(body, `class="avatar-img"`) {
		t.Error("an image avatar is rendered without any configured URL")
	}
}

// TestSiteIconFaviconFallback pins the tab icon chain and, just as important, its
// separation from the avatar: the icon is what the browser tab shows, the avatar
// is the face next to every post, and configuring one must not move the other.
func TestSiteIconFaviconFallback(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	login := f.login(t, owner.Email, authPassword, nil)
	cookie, csrf := sessionCookie(t, login), decodeData(t, login)["csrf_token"].(string)

	const (
		siteAvatar  = "https://cdn.example.com/site-avatar.png"
		siteIcon    = "https://cdn.example.com/site-icon.png"
		ownerAvatar = "https://cdn.example.com/owner-avatar.png"
	)
	builtin := `<link rel="icon" type="image/svg+xml" href="/static/brand/boop-mark.svg?v=` + staticAssets().version + `">`

	patch := func(body string) {
		t.Helper()
		rec := f.patchSettings(t, body, cookie, csrf)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %s: status = %d: %s", body, rec.Code, rec.Body.String())
		}
	}
	home := func() string {
		t.Helper()
		rec := f.do(t, http.MethodGet, "/", "", nil, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("home: status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}
	favicon := func(body string) string {
		t.Helper()
		start := strings.Index(body, `<link rel="icon"`)
		if start < 0 {
			t.Fatal("the page has no favicon link")
		}
		rest := body[start:]
		end := strings.Index(rest, ">")
		return rest[:end+1]
	}

	if _, err := f.db.Exec(`UPDATE users SET avatar_url = ? WHERE id = ?`, ownerAvatar, owner.ID); err != nil {
		t.Fatalf("set the owner avatar: %v", err)
	}

	// Only the icon is configured: the tab uses it, the avatar keeps the account
	// fallback.
	patch(`{"site_icon_url":"` + siteIcon + `"}`)
	body := home()
	if got := favicon(body); got != `<link rel="icon" href="`+siteIcon+`">` {
		t.Errorf("favicon = %s, want the configured site icon", got)
	}
	if !strings.Contains(body, ownerAvatar) {
		t.Error("the site icon replaced the visible avatar")
	}

	// Both configured: the icon still wins the tab, the avatar still shows the
	// site avatar in the page.
	patch(`{"site_avatar_url":"` + siteAvatar + `"}`)
	body = home()
	if got := favicon(body); got != `<link rel="icon" href="`+siteIcon+`">` {
		t.Errorf("favicon = %s, want the site icon to outrank the avatar", got)
	}
	if !strings.Contains(body, siteAvatar) {
		t.Error("the configured site avatar is not rendered in the page")
	}

	// Clearing the icon falls back to the avatar, then to the built-in SVG.
	patch(`{"site_icon_url":""}`)
	body = home()
	if got := favicon(body); got != `<link rel="icon" href="`+siteAvatar+`">` {
		t.Errorf("favicon = %s, want the site avatar", got)
	}
	patch(`{"site_avatar_url":""}`)
	body = home()
	if got := favicon(body); got != builtin {
		t.Errorf("favicon = %s, want the built-in icon", got)
	}
}

// TestConfiguredAvatarRendersOnEveryContentSurface walks the four documented
// places: the quick publisher, a feed card, a bookmark card and the detail page.
func TestConfiguredAvatarRendersOnEveryContentSurface(t *testing.T) {
	f := newContentFixture(t)
	const avatarURL = "https://cdn.example.com/site-avatar.png"

	post := f.createOK(t, map[string]any{"type": "moment", "status": "published", "body": "海边的下午"})
	slug := post["slug"].(string)
	postID := int64(post["id"].(float64))

	patch := f.do(t, http.MethodPatch, "/api/v1/admin/settings",
		`{"site_avatar_url":"`+avatarURL+`"}`,
		map[string]string{"Origin": testOrigin, "X-CSRF-Token": f.csrf}, f.cookie)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch site_avatar_url: status = %d: %s", patch.Code, patch.Body.String())
	}
	marker := `<img class="avatar-img" src="` + avatarURL + `"`

	home := f.do(t, http.MethodGet, "/", "", nil, f.cookie)
	if home.Code != http.StatusOK {
		t.Fatalf("home: status = %d, want 200", home.Code)
	}
	// The owner's session renders both the quick publisher and the feed card.
	if got := strings.Count(home.Body.String(), marker); got != 2 {
		t.Errorf("the home page renders %d configured avatars, want 2 (composer and card)", got)
	}

	detail := f.do(t, http.MethodGet, "/p/"+slug, "", nil, f.cookie)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: status = %d, want 200", detail.Code)
	}
	if !strings.Contains(detail.Body.String(), marker) {
		t.Error("the detail page does not render the configured avatar")
	}

	bookmark := f.do(t, http.MethodPut, fmt.Sprintf("/api/v1/posts/%d/bookmark", postID), "",
		map[string]string{"Origin": testOrigin, "X-CSRF-Token": f.csrf}, f.cookie)
	if bookmark.Code != http.StatusOK {
		t.Fatalf("bookmark: status = %d, want 200: %s", bookmark.Code, bookmark.Body.String())
	}
	list := f.do(t, http.MethodGet, "/bookmarks", "", nil, f.cookie)
	if list.Code != http.StatusOK {
		t.Fatalf("bookmarks: status = %d, want 200", list.Code)
	}
	if !strings.Contains(list.Body.String(), marker) {
		t.Error("the bookmark card does not render the configured avatar")
	}
}

// TestLeftRailAdminEntryIsOwnerOnly pins the permanent way into the admin pages:
// the rail owns a gear at its foot, so the owner does not have to remember a URL.
// A guest or a reader must not be handed a link that could only answer 403.
//
// The assertion is about the link, not about the rail foot: the foot is also
// where every signed-in visitor gets the sign-out control, so it exists for a
// reader too. What must stay owner-only is the way into /admin.
func TestLeftRailAdminEntryIsOwnerOnly(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	ownerCookie := sessionCookie(t, f.login(t, owner.Email, authPassword, nil))
	readerCookie := sessionCookie(t, f.register(t, "reader@example.com", "读者甲"))

	const marker = `class="rail-foot"`

	rec := f.do(t, http.MethodGet, "/", "", nil, ownerCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner home: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, marker) {
		t.Fatal("the owner's page has no rail foot")
	}
	foot := body[strings.Index(body, marker):]
	// Only the rail foot matters here: the topbar carries its own owner-only
	// admin buttons, and that is a different surface with its own test.
	if end := strings.Index(foot, "</aside>"); end >= 0 {
		foot = foot[:end]
	}
	if !strings.Contains(foot, `href="/admin"`) {
		t.Errorf("the rail foot does not link into the admin pages: %s", foot)
	}

	for _, tt := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"guest", nil},
		{"reader", readerCookie},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, "/", "", nil, tt.cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if strings.Contains(rec.Body.String(), `href="/admin"`) {
				t.Errorf("%s is offered the rail's admin link", tt.name)
			}
		})
	}
}

func TestAdminSettingsPageAndIndex(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	ownerLogin := f.login(t, owner.Email, authPassword, nil)
	ownerCookie := sessionCookie(t, ownerLogin)

	readerCookie := sessionCookie(t, f.register(t, "reader@example.com", "读者甲"))

	t.Run("guest is sent to the sign-in page", func(t *testing.T) {
		for _, target := range []string{"/admin", "/admin/settings"} {
			rec := f.do(t, http.MethodGet, target, "", nil, nil)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("%s: status = %d, want 303", target, rec.Code)
			}
			if location := rec.Header().Get("Location"); location != loginPath {
				t.Errorf("%s: Location = %q, want %q", target, location, loginPath)
			}
		}
	})

	t.Run("a reader is refused", func(t *testing.T) {
		for _, target := range []string{"/admin", "/admin/settings"} {
			rec := f.do(t, http.MethodGet, target, "", nil, readerCookie)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: status = %d, want 403", target, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "只有站长") {
				t.Errorf("%s: body = %q", target, rec.Body.String())
			}
		}
	})

	t.Run("the owner gets the form", func(t *testing.T) {
		if err := settings.Apply(t.Context(), f.db, settingsBox(t, f), settings.Update{
			Secrets: map[string]string{settings.SecretKeyGitHubClientSecret: settingsClientSecret},
		}); err != nil {
			t.Fatalf("store a secret: %v", err)
		}
		rec := f.do(t, http.MethodGet, "/admin/settings", "", nil, ownerCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, marker := range []string{
			`data-settings-form`,
			`data-settings-action="/api/v1/admin/settings"`,
			`name="site_name"`,
			`name="site_description"`,
			`name="site_avatar_url"`,
			`name="site_icon_url"`,
			`name="page_size"`,
			`name="registration_enabled"`,
			`name="ai_api_key"`,
			`data-secret-clear="github.client_secret"`,
			`data-settings-submit`,
		} {
			if !strings.Contains(body, marker) {
				t.Errorf("the settings page is missing %s", marker)
			}
		}
		if !strings.Contains(body, `value="Boop"`) {
			t.Error("the page does not render the stored site name")
		}
		if !strings.Contains(body, testOrigin+oauthCallbackPath) {
			t.Error("the page does not show the OAuth callback address")
		}
		// A configured secret says so and never repeats its value; an unconfigured
		// one says so as well.
		if !strings.Contains(body, "留空则保持不变") {
			t.Error("a configured secret does not explain that an empty field keeps it")
		}
		if !strings.Contains(body, "未配置") {
			t.Error("an unconfigured secret is not marked as such")
		}
		if strings.Contains(body, settingsClientSecret) {
			t.Error("the page repeats a stored secret")
		}
	})

	t.Run("the index redirects to the settings page", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/admin", "", nil, ownerCookie)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/settings" {
			t.Fatalf("status = %d, Location = %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("without a master key the page warns", func(t *testing.T) {
		plain := newAuthFixture(t)
		if err := settings.Seed(t.Context(), plain.db); err != nil {
			t.Fatalf("settings.Seed: %v", err)
		}
		plainOwner := plain.bootstrapOwner(t, "owner@example.com", "遇事开心")
		plainCookie := sessionCookie(t, plain.login(t, plainOwner.Email, authPassword, nil))

		rec := plain.do(t, http.MethodGet, "/admin/settings", "", nil, plainCookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "未配置 BOOP_MASTER_KEY") {
			t.Error("the page does not warn about the missing master key")
		}
	})

	t.Run("the API subtree stays JSON", func(t *testing.T) {
		// A guest probe reaches routing: an authenticated unsafe request without a
		// CSRF token is refused earlier, which is its own documented behaviour.
		rec := f.do(t, http.MethodPut, "/api/v1/admin/settings", "{}", map[string]string{"Origin": testOrigin}, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PATCH" {
			t.Errorf("Allow = %q, want %q", allow, "GET, PATCH")
		}
		if code, _, _ := decodeAPIError(t, rec); code != "method_not_allowed" {
			t.Errorf("code = %q", code)
		}
		rec = f.do(t, http.MethodGet, "/api/v1/admin/settings/extra", "", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("unknown path: status = %d, want 404", rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != jsonContentType {
			t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
		}
	})
}

// TestAdminSettingsPageFailsClosed keeps the page from rendering defaults that
// the owner could then save over the real values.
func TestAdminSettingsPageFailsClosed(t *testing.T) {
	f := newSettingsFixture(t)
	owner := f.bootstrapOwner(t, "owner@example.com", "遇事开心")
	cookie := sessionCookie(t, f.login(t, owner.Email, authPassword, nil))

	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE settings SET value_json = '"not a number"' WHERE key = ?`, settings.KeyContentPageSize); err != nil {
		t.Fatalf("corrupt setting: %v", err)
	}
	rec := f.do(t, http.MethodGet, "/admin/settings", "", nil, cookie)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	api := f.getSettings(t, cookie)
	if api.Code != http.StatusInternalServerError {
		t.Fatalf("API status = %d, want 500", api.Code)
	}
	if code, _, _ := decodeAPIError(t, api); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
}
