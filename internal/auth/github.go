package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"boop/internal/settings"
	"boop/internal/store"
)

// GitHub login (docs/PRODUCT.md §5.2, docs/API.md 认证). Boop talks to GitHub's
// documented OAuth endpoints with the standard library only: no SDK and no
// provider plugin surface.
const (
	// githubProvider is the value stored in oauth_accounts.provider.
	githubProvider = "github"
	// githubScopes needs user:email to read the verified addresses.
	githubScopes = "read:user user:email"
	// githubAPIVersion pins the REST API version GitHub documents.
	githubAPIVersion = "2022-11-28"
	// maxGitHubResponseBytes bounds every GitHub response body, so a hostile or
	// broken upstream cannot make the process buffer without limit.
	maxGitHubResponseBytes = 1 << 20
	// githubTimeout bounds one outbound round trip.
	githubTimeout = 10 * time.Second
	// githubLoginMaxBytes bounds the stored provider_login column value.
	githubLoginMaxBytes = 100
)

var (
	// ErrGitHubUnavailable covers a transport failure, a timeout, a non-2xx
	// answer and an unreadable body.
	ErrGitHubUnavailable = errors.New("auth: github is unavailable")
	// ErrGitHubRejected means GitHub refused the authorization request itself.
	ErrGitHubRejected = errors.New("auth: github rejected the request")
	// ErrRegistrationDisabled means a new account would be needed while public
	// registration is closed.
	ErrRegistrationDisabled = errors.New("auth: public registration is disabled")
	// ErrEmailUnverified means GitHub did not vouch for an address, so no account
	// may be created or bound with it.
	ErrEmailUnverified = errors.New("auth: github email is not verified")
)

// GitHubAPI is the GitHub client. The base URLs and the HTTP client are fields
// so tests can point the flow at a local server; production uses the documented
// defaults and a client with an explicit timeout.
type GitHubAPI struct {
	HTTP      *http.Client
	OAuthBase string
	APIBase   string
}

// NewGitHubAPI builds the production client. A nil client gets the documented
// timeout, so no request can hang a handler.
func NewGitHubAPI(client *http.Client) GitHubAPI {
	if client == nil {
		client = &http.Client{Timeout: githubTimeout}
	}
	return GitHubAPI{HTTP: client, OAuthBase: "https://github.com", APIBase: "https://api.github.com"}
}

func (api GitHubAPI) client() *http.Client {
	if api.HTTP != nil {
		return api.HTTP
	}
	return &http.Client{Timeout: githubTimeout}
}

func (api GitHubAPI) oauthBase() string {
	if api.OAuthBase != "" {
		return strings.TrimSuffix(api.OAuthBase, "/")
	}
	return "https://github.com"
}

func (api GitHubAPI) apiBase() string {
	if api.APIBase != "" {
		return strings.TrimSuffix(api.APIBase, "/")
	}
	return "https://api.github.com"
}

// AuthorizationURL is the redirect that starts the flow. The state value is
// generated and stored by the HTTP layer.
func (api GitHubAPI) AuthorizationURL(clientID, redirectURL, state string) string {
	values := url.Values{}
	values.Set("client_id", clientID)
	values.Set("redirect_uri", redirectURL)
	values.Set("scope", githubScopes)
	values.Set("state", state)
	return api.oauthBase() + "/login/oauth/authorize?" + values.Encode()
}

// GitHubIdentity is the part of a GitHub account Boop stores and trusts.
type GitHubIdentity struct {
	ProviderUserID string
	Login          string
	Name           string
	Email          string
	// EmailVerified is true only when GitHub listed the address as verified.
	EmailVerified bool
}

// Exchange trades the authorization code for an access token. Errors never
// carry the code, the client secret or the token.
func (api GitHubAPI) Exchange(ctx context.Context, clientID, clientSecret, code string) (string, error) {
	if clientID == "" || clientSecret == "" || code == "" {
		return "", errors.New("auth: github exchange needs a client id, a client secret and a code")
	}
	payload, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"code":          code,
	})
	if err != nil {
		return "", fmt.Errorf("auth: github exchange: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		api.oauthBase()+"/login/oauth/access_token", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("auth: github exchange: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := api.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", fmt.Errorf("%w: token endpoint answered %d", ErrGitHubUnavailable, response.StatusCode)
	}

	var result struct {
		AccessToken      string `json:"access_token"`
		TokenType        string `json:"token_type"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := decodeGitHub(response.Body, &result); err != nil {
		return "", err
	}
	if result.Error != "" {
		// The machine readable reason is safe to report; the human readable text
		// can echo request input, so it stays out of the error.
		return "", fmt.Errorf("%w: %s", ErrGitHubRejected, result.Error)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("%w: no access token in the answer", ErrGitHubRejected)
	}
	return result.AccessToken, nil
}

// FetchIdentity reads the profile and the verified addresses. The public profile
// email is not proof of ownership, so the answer comes from /user/emails.
func (api GitHubAPI) FetchIdentity(ctx context.Context, accessToken string) (GitHubIdentity, error) {
	if strings.TrimSpace(accessToken) == "" {
		return GitHubIdentity{}, errors.New("auth: github identity needs an access token")
	}

	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := api.getJSON(ctx, api.apiBase()+"/user", accessToken, &profile); err != nil {
		return GitHubIdentity{}, err
	}
	if profile.ID == 0 {
		return GitHubIdentity{}, fmt.Errorf("%w: profile has no id", ErrGitHubUnavailable)
	}
	identity := GitHubIdentity{
		ProviderUserID: strconv.FormatInt(profile.ID, 10),
		Login:          profile.Login,
		Name:           profile.Name,
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := api.getJSON(ctx, api.apiBase()+"/user/emails", accessToken, &emails); err != nil {
		return GitHubIdentity{}, err
	}

	primary, fallback := "", ""
	for _, entry := range emails {
		if !entry.Verified || entry.Email == "" {
			continue
		}
		if fallback == "" {
			fallback = entry.Email
		}
		if entry.Primary {
			primary = entry.Email
			break
		}
	}
	switch {
	case primary != "":
		identity.Email, identity.EmailVerified = primary, true
	case fallback != "":
		identity.Email, identity.EmailVerified = fallback, true
	case profile.Email != "":
		// Kept for diagnostics only: EmailVerified stays false, so the caller
		// refuses to create or bind an account with it.
		identity.Email = profile.Email
	}
	return identity, nil
}

func (api GitHubAPI) getJSON(ctx context.Context, endpoint, accessToken string, dst any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("auth: github request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	response, err := api.client().Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("%w: %s answered %d", ErrGitHubUnavailable, request.URL.Path, response.StatusCode)
	}
	return decodeGitHub(response.Body, dst)
}

// decodeGitHub reads one bounded JSON document. Unknown fields are accepted on
// purpose: GitHub adds them, and a strict decoder would break on an upstream
// release.
func decodeGitHub(body io.Reader, dst any) error {
	if err := json.NewDecoder(io.LimitReader(body, maxGitHubResponseBytes)).Decode(dst); err != nil {
		return fmt.Errorf("%w: unreadable answer: %v", ErrGitHubUnavailable, err)
	}
	return nil
}

// SignInWithGitHub resolves a GitHub identity to a Boop account: a known GitHub
// account signs in, a verified address matching an existing account is bound,
// and otherwise a reader is created while public registration is open. The
// registration switch is read inside this transaction, so it cannot change
// between the decision and the insert.
func SignInWithGitHub(ctx context.Context, db *sql.DB, identity GitHubIdentity, now time.Time) (User, error) {
	if db == nil {
		return User{}, errors.New("auth: github sign-in: nil database")
	}
	if strings.TrimSpace(identity.ProviderUserID) == "" {
		return User{}, errors.New("auth: github sign-in: provider user id is required")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("auth: github sign-in: begin: %w", err)
	}
	defer tx.Rollback()

	stamp := timestamp(now)
	userID, linked, err := linkedUserID(ctx, tx, identity.ProviderUserID)
	if err != nil {
		return User{}, err
	}
	if !linked {
		if userID, err = resolveGitHubEmail(ctx, tx, identity, stamp); err != nil {
			return User{}, err
		}
	}

	user, err := loadUserTx(ctx, tx, userID)
	if err != nil {
		return User{}, err
	}
	if !user.IsActive() {
		return User{}, ErrAccountDisabled
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("auth: github sign-in: commit: %w", err)
	}
	return user, nil
}

func linkedUserID(ctx context.Context, tx *sql.Tx, providerUserID string) (int64, bool, error) {
	var userID int64
	err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM oauth_accounts WHERE provider = ? AND provider_user_id = ?`,
		githubProvider, providerUserID).Scan(&userID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("auth: github sign-in: find link: %w", err)
	}
	return userID, true, nil
}

// resolveGitHubEmail binds the identity to the account that owns the verified
// address, or creates a reader account while public registration is open.
func resolveGitHubEmail(ctx context.Context, tx *sql.Tx, identity GitHubIdentity, stamp string) (int64, error) {
	if !identity.EmailVerified || strings.TrimSpace(identity.Email) == "" {
		return 0, ErrEmailUnverified
	}
	email, err := NormalizeEmail(identity.Email)
	if err != nil {
		// An address Boop cannot store must never become an account.
		return 0, ErrEmailUnverified
	}

	var userID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email = ?`, email).Scan(&userID)
	switch {
	case err == nil:
		// The address already belongs to an account, so this is a sign-in with a
		// second provider: it stays available after registration is closed.
		if err := linkGitHubAccount(ctx, tx, userID, identity, stamp); err != nil {
			return 0, err
		}
		return userID, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("auth: github sign-in: find account: %w", err)
	}

	values, err := settings.LoadTx(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("auth: github sign-in: settings: %w", err)
	}
	if !values.RegistrationEnabled {
		return 0, ErrRegistrationDisabled
	}

	name, err := NormalizeDisplayName(displayNameFor(identity))
	if err != nil {
		return 0, err
	}
	// password_hash stays NULL: the account signs in through GitHub only, and a
	// NULL hash can never verify a password.
	result, err := tx.ExecContext(ctx, `INSERT INTO users(email, password_hash, display_name, role, status, created_at, updated_at)
		VALUES(?, NULL, ?, ?, ?, ?, ?)`, email, name, RoleReader, StatusActive, stamp, stamp)
	if err != nil {
		if !store.IsUniqueViolation(err) {
			return 0, fmt.Errorf("auth: github sign-in: create account: %w", err)
		}
		// Another request created the account a moment ago: bind to it.
		if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email = ?`, email).Scan(&userID); err != nil {
			return 0, fmt.Errorf("auth: github sign-in: re-read account: %w", err)
		}
	} else if userID, err = result.LastInsertId(); err != nil {
		return 0, fmt.Errorf("auth: github sign-in: account id: %w", err)
	}

	if err := linkGitHubAccount(ctx, tx, userID, identity, stamp); err != nil {
		return 0, err
	}
	return userID, nil
}

// linkGitHubAccount records the identity. A concurrent request may have linked
// the same GitHub account first, which leaves the existing row in place.
func linkGitHubAccount(ctx context.Context, tx *sql.Tx, userID int64, identity GitHubIdentity, stamp string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO oauth_accounts(user_id, provider, provider_user_id, provider_login, created_at)
		VALUES(?, ?, ?, ?, ?)`,
		userID, githubProvider, identity.ProviderUserID, truncate(identity.Login, githubLoginMaxBytes), stamp)
	if err == nil || store.IsUniqueViolation(err) {
		return nil
	}
	return fmt.Errorf("auth: github sign-in: link account: %w", err)
}

// displayNameFor picks the stored display name: the GitHub profile name, else
// the handle, else a neutral default. A cosmetic field must never fail a
// sign-in, and an existing account is never renamed.
func displayNameFor(identity GitHubIdentity) string {
	for _, candidate := range []string{identity.Name, identity.Login} {
		if name, err := NormalizeDisplayName(candidate); err == nil {
			return name
		}
	}
	return "GitHub 用户"
}

func loadUserTx(ctx context.Context, tx *sql.Tx, id int64) (User, error) {
	return scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}
