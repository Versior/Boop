package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"boop/internal/settings"
)

// fakeGitHub is a local stand-in for github.com: the flow is exercised over real
// HTTP, so no interface or mock layer is needed.
type fakeGitHub struct {
	server *httptest.Server
	// Token is the value the token endpoint hands out.
	Token string
	// ExchangeStatus, ExchangeBody and ErrorCode steer the token endpoint.
	ExchangeStatus int
	ErrorCode      string
	// ProfileStatus steers /user, EmailsStatus steers /user/emails.
	ProfileStatus int
	EmailsStatus  int
	// Profile and Emails are the answers of a healthy GitHub.
	Profile map[string]any
	Emails  []map[string]any
	// SeenCode records the authorization code GitHub received.
	SeenCode string
	// ExchangeBodySize, when > 0, pads the token answer to that many bytes.
	ExchangeBodySize int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	fake := &fakeGitHub{
		Token:   "gho_test_token_value",
		Profile: map[string]any{"id": 4242, "login": "octocat", "name": "Monalisa Octocat"},
		Emails: []map[string]any{
			{"email": "octocat@example.com", "primary": false, "verified": false},
			{"email": "octocat@example.com", "primary": true, "verified": true},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if fake.ExchangeStatus != 0 && fake.ExchangeStatus != http.StatusOK {
			w.WriteHeader(fake.ExchangeStatus)
			return
		}
		var body struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Code         string `json:"code"`
		}
		if err := decodeGitHub(r.Body, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fake.SeenCode = body.Code
		w.Header().Set("Content-Type", "application/json")
		answer := map[string]any{"access_token": fake.Token, "token_type": "bearer", "scope": githubScopes}
		if fake.ErrorCode != "" {
			answer = map[string]any{"error": fake.ErrorCode, "error_description": "the code passed is incorrect"}
		}
		if fake.ExchangeBodySize > 0 {
			answer["padding"] = strings.Repeat("x", fake.ExchangeBodySize)
		}
		fmt.Fprint(w, mustJSON(t, answer))
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if fake.ProfileStatus != 0 && fake.ProfileStatus != http.StatusOK {
			w.WriteHeader(fake.ProfileStatus)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+fake.Token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, mustJSON(t, fake.Profile))
	})
	mux.HandleFunc("GET /user/emails", func(w http.ResponseWriter, r *http.Request) {
		if fake.EmailsStatus != 0 && fake.EmailsStatus != http.StatusOK {
			w.WriteHeader(fake.EmailsStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, mustJSON(t, fake.Emails))
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeGitHub) api() GitHubAPI {
	return GitHubAPI{
		HTTP:      f.server.Client(),
		OAuthBase: f.server.URL,
		APIBase:   f.server.URL,
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

func TestAuthorizationURL(t *testing.T) {
	api := NewGitHubAPI(nil)
	raw := api.AuthorizationURL("client-123", "https://blog.example.com/auth/github/callback", "state-value")

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.Path != "/login/oauth/authorize" {
		t.Errorf("authorization URL = %q", raw)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"client_id":    "client-123",
		"redirect_uri": "https://blog.example.com/auth/github/callback",
		"state":        "state-value",
		"scope":        githubScopes,
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestExchangeReturnsTheAccessToken(t *testing.T) {
	fake := newFakeGitHub(t)
	token, err := fake.api().Exchange(context.Background(), "id", "secret", "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if token != fake.Token {
		t.Errorf("token = %q, want %q", token, fake.Token)
	}
	if fake.SeenCode != "the-code" {
		t.Errorf("GitHub saw code %q", fake.SeenCode)
	}
}

func TestExchangeFailuresAreRedacted(t *testing.T) {
	const secret = "client-secret-value"
	tests := []struct {
		name   string
		mutate func(*fakeGitHub)
		want   error
	}{
		{"github rejects the code", func(f *fakeGitHub) { f.ErrorCode = "bad_verification_code" }, ErrGitHubRejected},
		{"token endpoint fails", func(f *fakeGitHub) { f.ExchangeStatus = http.StatusInternalServerError }, ErrGitHubUnavailable},
		{"unreadable answer", func(f *fakeGitHub) { f.ExchangeBodySize = 2 << 20 }, ErrGitHubUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			tt.mutate(fake)

			_, err := fake.api().Exchange(context.Background(), "client-id", secret, "the-code")
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			message := err.Error()
			for _, leak := range []string{secret, "the-code", fake.Token} {
				if strings.Contains(message, leak) {
					t.Errorf("error message %q leaks %q", message, leak)
				}
			}
		})
	}
}

func TestExchangeRequiresEveryInput(t *testing.T) {
	fake := newFakeGitHub(t)
	for _, tt := range []struct{ id, secret, code string }{
		{"", "secret", "code"},
		{"id", "", "code"},
		{"id", "secret", ""},
	} {
		if _, err := fake.api().Exchange(context.Background(), tt.id, tt.secret, tt.code); err == nil {
			t.Errorf("Exchange(%q, %q, %q) succeeded", tt.id, tt.secret, tt.code)
		}
	}
}

func TestFetchIdentityPrefersTheVerifiedPrimaryEmail(t *testing.T) {
	fake := newFakeGitHub(t)
	identity, err := fake.api().FetchIdentity(context.Background(), fake.Token)
	if err != nil {
		t.Fatalf("FetchIdentity: %v", err)
	}
	if identity.ProviderUserID != "4242" {
		t.Errorf("ProviderUserID = %q, want 4242", identity.ProviderUserID)
	}
	if identity.Login != "octocat" || identity.Name != "Monalisa Octocat" {
		t.Errorf("identity = %+v", identity)
	}
	if identity.Email != "octocat@example.com" || !identity.EmailVerified {
		t.Errorf("email = %q verified = %v, want the verified primary", identity.Email, identity.EmailVerified)
	}
}

func TestFetchIdentityFallsBackToAVerifiedSecondary(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.Emails = []map[string]any{
		{"email": "secondary@example.com", "primary": false, "verified": true},
	}
	identity, err := fake.api().FetchIdentity(context.Background(), fake.Token)
	if err != nil {
		t.Fatalf("FetchIdentity: %v", err)
	}
	if identity.Email != "secondary@example.com" || !identity.EmailVerified {
		t.Errorf("email = %q verified = %v", identity.Email, identity.EmailVerified)
	}
}

func TestFetchIdentityNeverTrustsAnUnverifiedAddress(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.Emails = []map[string]any{
		{"email": "unverified@example.com", "primary": true, "verified": false},
	}
	fake.Profile["email"] = "public@example.com"

	identity, err := fake.api().FetchIdentity(context.Background(), fake.Token)
	if err != nil {
		t.Fatalf("FetchIdentity: %v", err)
	}
	if identity.EmailVerified {
		t.Errorf("identity = %+v, want no verified address", identity)
	}
	if identity.Email != "public@example.com" {
		t.Errorf("email = %q, want the public profile address for diagnostics", identity.Email)
	}
}

func TestFetchIdentityFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeGitHub)
	}{
		{"profile fails", func(f *fakeGitHub) { f.ProfileStatus = http.StatusForbidden }},
		{"emails fail", func(f *fakeGitHub) { f.EmailsStatus = http.StatusNotFound }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			tt.mutate(fake)
			if _, err := fake.api().FetchIdentity(context.Background(), fake.Token); !errors.Is(err, ErrGitHubUnavailable) {
				t.Fatalf("error = %v, want ErrGitHubUnavailable", err)
			}
		})
	}
	if _, err := newFakeGitHub(t).api().FetchIdentity(context.Background(), ""); err == nil {
		t.Error("FetchIdentity accepted an empty token")
	}
}

// ---------- account resolution ----------

func githubIdentity(t *testing.T, userID, email string, verified bool) GitHubIdentity {
	t.Helper()
	return GitHubIdentity{
		ProviderUserID: userID,
		Login:          "octocat" + userID,
		Name:           "Mona " + userID,
		Email:          email,
		EmailVerified:  verified,
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func TestGitHubSignInLinksAnExistingAccount(t *testing.T) {
	db := testDB(t)
	reader := newReader(t, db, "reader@example.com", "读者甲")
	if err := settings.Seed(context.Background(), db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}

	identity := githubIdentity(t, "9001", "Reader@Example.com", true)
	user, err := SignInWithGitHub(context.Background(), db, identity, time.Now())
	if err != nil {
		t.Fatalf("SignInWithGitHub: %v", err)
	}
	if user.ID != reader.ID {
		t.Errorf("user = %d, want the existing account %d", user.ID, reader.ID)
	}
	if user.DisplayName != "读者甲" {
		t.Errorf("display name = %q, want the existing one untouched", user.DisplayName)
	}
	if links := countRows(t, db, "oauth_accounts"); links != 1 {
		t.Errorf("oauth_accounts = %d, want 1", links)
	}

	// Signing in again through the same GitHub account resolves the same user and
	// adds no second link.
	again, err := SignInWithGitHub(context.Background(), db, identity, time.Now())
	if err != nil {
		t.Fatalf("second SignInWithGitHub: %v", err)
	}
	if again.ID != reader.ID || countRows(t, db, "oauth_accounts") != 1 {
		t.Errorf("second sign-in = %+v, links = %d", again, countRows(t, db, "oauth_accounts"))
	}
}

// TestLinkGitHubAccountReusesOnlyItsOwnUser covers the unique-violation path: a
// concurrent request may link the same GitHub account first, and only the
// account this sign-in resolved to may reuse that link. A link that points at
// another account is a conflict, so the caller rolls back instead of signing the
// wrong user in.
func TestLinkGitHubAccountReusesOnlyItsOwnUser(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	first := newReader(t, db, "first@example.com", "甲")
	second := newReader(t, db, "second@example.com", "乙")
	identity := githubIdentity(t, "4242", "first@example.com", true)
	stamp := timestamp(time.Now())

	// The link the concurrent request would have committed first.
	withTx := func(run func(tx *sql.Tx) error) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		defer tx.Rollback()
		if err := run(tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return nil
	}
	if err := withTx(func(tx *sql.Tx) error {
		return linkGitHubAccount(ctx, tx, first.ID, identity, stamp)
	}); err != nil {
		t.Fatalf("first link: %v", err)
	}

	// The same account links idempotently: the row is reused, not duplicated.
	if err := withTx(func(tx *sql.Tx) error {
		return linkGitHubAccount(ctx, tx, first.ID, identity, stamp)
	}); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
	if links := countRows(t, db, "oauth_accounts"); links != 1 {
		t.Fatalf("oauth_accounts = %d, want 1", links)
	}

	// A different account must never take over the identity.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	linkErr := linkGitHubAccount(ctx, tx, second.ID, identity, stamp)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !errors.Is(linkErr, ErrGitHubLinkConflict) {
		t.Fatalf("link for another user: error = %v, want ErrGitHubLinkConflict", linkErr)
	}

	// Nothing changed: the identity still belongs to the first account and the
	// second account has no link at all.
	var owner int64
	if err := db.QueryRow(`SELECT user_id FROM oauth_accounts WHERE provider = ? AND provider_user_id = ?`,
		githubProvider, identity.ProviderUserID).Scan(&owner); err != nil {
		t.Fatalf("read the link: %v", err)
	}
	if owner != first.ID {
		t.Errorf("link owner = %d, want %d", owner, first.ID)
	}
	if links := countRows(t, db, "oauth_accounts"); links != 1 {
		t.Errorf("oauth_accounts = %d, want the conflict to add none", links)
	}
}

// TestGitHubSignInWithRegistrationDisabled covers the documented rule that
// closing public registration keeps existing accounts usable.
func TestGitHubSignInWithRegistrationDisabled(t *testing.T) {
	db := testDB(t)
	reader := newReader(t, db, "reader@example.com", "读者甲")
	ctx := context.Background()
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	if err := settings.Set(ctx, db, settings.KeyAuthRegistrationEnabled, false); err != nil {
		t.Fatalf("disable registration: %v", err)
	}

	// Binding a verified address to an account that already exists stays allowed.
	user, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9002", "reader@example.com", true), time.Now())
	if err != nil {
		t.Fatalf("bind with registration closed: %v", err)
	}
	if user.ID != reader.ID {
		t.Errorf("user = %d, want %d", user.ID, reader.ID)
	}

	// A brand new account is refused and leaves nothing behind.
	before := countRows(t, db, "users")
	_, err = SignInWithGitHub(ctx, db, githubIdentity(t, "9003", "newcomer@example.com", true), time.Now())
	if !errors.Is(err, ErrRegistrationDisabled) {
		t.Fatalf("error = %v, want ErrRegistrationDisabled", err)
	}
	if after := countRows(t, db, "users"); after != before {
		t.Errorf("users = %d, want %d: a refused sign-in must not create an account", after, before)
	}
	if links := countRows(t, db, "oauth_accounts"); links != 1 {
		t.Errorf("oauth_accounts = %d, want only the earlier link", links)
	}
}

func TestGitHubSignInCreatesAReaderWhileRegistrationIsOpen(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}

	user, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9004", "fresh@example.com", true), time.Now())
	if err != nil {
		t.Fatalf("SignInWithGitHub: %v", err)
	}
	if user.Role != RoleReader {
		t.Errorf("role = %q, want %q: GitHub never grants the owner role", user.Role, RoleReader)
	}
	if user.DisplayName != "Mona 9004" {
		t.Errorf("display name = %q", user.DisplayName)
	}
	// A GitHub-only account has no password, so it can never sign in with one.
	var hash *string
	if err := db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, user.ID).Scan(&hash); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	if hash != nil {
		t.Errorf("password_hash = %q, want NULL", *hash)
	}
	if VerifyPassword(user.PasswordHash, "correct horse battery") {
		t.Error("an OAuth-only account verified a password")
	}
}

// TestGitHubSignInRefusesAnUnverifiedCollision is the documented boundary: an
// address that matches an existing account may only be bound when GitHub marks
// it verified.
func TestGitHubSignInRefusesAnUnverifiedCollision(t *testing.T) {
	db := testDB(t)
	newReader(t, db, "reader@example.com", "读者甲")
	ctx := context.Background()
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}

	_, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9005", "reader@example.com", false), time.Now())
	if !errors.Is(err, ErrEmailUnverified) {
		t.Fatalf("error = %v, want ErrEmailUnverified", err)
	}
	if links := countRows(t, db, "oauth_accounts"); links != 0 {
		t.Errorf("oauth_accounts = %d, want none", links)
	}

	// The same holds for a brand new address: Boop does not create an account
	// from an address GitHub will not vouch for.
	if _, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9006", "unverified@example.com", false), time.Now()); !errors.Is(err, ErrEmailUnverified) {
		t.Fatalf("error = %v, want ErrEmailUnverified", err)
	}
	if users := countRows(t, db, "users"); users != 1 {
		t.Errorf("users = %d, want only the fixture's account", users)
	}
}

func TestGitHubSignInRefusesAnUnusableAddress(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	if _, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9007", "not-an-address", true), time.Now()); !errors.Is(err, ErrEmailUnverified) {
		t.Fatalf("error = %v, want ErrEmailUnverified", err)
	}
	if _, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9008", "", true), time.Now()); !errors.Is(err, ErrEmailUnverified) {
		t.Fatalf("empty address: error = %v, want ErrEmailUnverified", err)
	}
}

func TestGitHubSignInRefusesADisabledAccount(t *testing.T) {
	db := testDB(t)
	reader := newReader(t, db, "reader@example.com", "读者甲")
	ctx := context.Background()
	if _, err := db.Exec(`UPDATE users SET status = 'disabled' WHERE id = ?`, reader.ID); err != nil {
		t.Fatalf("disable account: %v", err)
	}

	_, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9009", "reader@example.com", true), time.Now())
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("error = %v, want ErrAccountDisabled", err)
	}
	if links := countRows(t, db, "oauth_accounts"); links != 0 {
		t.Errorf("oauth_accounts = %d, want none for a refused sign-in", links)
	}
}

func TestGitHubSignInFallsBackToAUsableDisplayName(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}

	long := strings.Repeat("名", 200)
	identity := githubIdentity(t, "9010", "long@example.com", true)
	identity.Name = long
	user, err := SignInWithGitHub(ctx, db, identity, time.Now())
	if err != nil {
		t.Fatalf("SignInWithGitHub: %v", err)
	}
	if user.DisplayName != identity.Login {
		t.Errorf("display name = %q, want the handle %q", user.DisplayName, identity.Login)
	}

	identity = githubIdentity(t, "9011", "blank@example.com", true)
	identity.Name = "   "
	identity.Login = ""
	user, err = SignInWithGitHub(ctx, db, identity, time.Now())
	if err != nil {
		t.Fatalf("SignInWithGitHub: %v", err)
	}
	if user.DisplayName != "GitHub 用户" {
		t.Errorf("display name = %q, want the neutral default", user.DisplayName)
	}
}

func TestGitHubSignInRequiresIdentityAndDatabase(t *testing.T) {
	db := testDB(t)
	if _, err := SignInWithGitHub(context.Background(), nil, GitHubIdentity{ProviderUserID: "1"}, time.Now()); err == nil {
		t.Error("SignInWithGitHub accepted a nil database")
	}
	if _, err := SignInWithGitHub(context.Background(), db, GitHubIdentity{}, time.Now()); err == nil {
		t.Error("SignInWithGitHub accepted an identity without a provider user id")
	}
}

// TestGitHubSignInFollowsTheStoredRegistrationSwitch keeps the same rule comment
// creation follows: the switch is read inside the transaction, so a caller
// cannot impose a stale decision.
func TestGitHubSignInFollowsTheStoredRegistrationSwitch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := settings.Seed(ctx, db); err != nil {
		t.Fatalf("settings.Seed: %v", err)
	}
	stale, err := settings.Load(ctx, db)
	if err != nil {
		t.Fatalf("settings.Load: %v", err)
	}
	if !stale.RegistrationEnabled {
		t.Fatal("the fixture is expected to start with registration open")
	}
	if err := settings.Set(ctx, db, settings.KeyAuthRegistrationEnabled, false); err != nil {
		t.Fatalf("disable registration: %v", err)
	}

	if _, err := SignInWithGitHub(ctx, db, githubIdentity(t, "9012", "stale@example.com", true), time.Now()); !errors.Is(err, ErrRegistrationDisabled) {
		t.Fatalf("error = %v, want the stored switch to decide", err)
	}
}
