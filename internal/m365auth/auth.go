// Package m365auth handles Microsoft Entra ID (Azure AD) sign-in for the
// Microsoft 365 Copilot provider.
//
// The Microsoft 365 Copilot Chat API only supports delegated (work or school
// account) permissions, so opencode signs the user in with either the OAuth 2.0
// authorization code flow with PKCE (a browser on this machine) or the device
// code flow (any browser, e.g. over SSH). Tokens are cached on disk and the
// access token is refreshed with the cached refresh token when it expires.
package m365auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAuthorityHost is the Microsoft identity platform host for the global cloud.
	DefaultAuthorityHost = "https://login.microsoftonline.com"

	// DefaultTenant only allows work or school accounts, which is all the Copilot APIs support.
	DefaultTenant = "organizations"

	// DefaultClientID is the public "Microsoft Graph Command Line Tools" application.
	// It supports the device code and loopback redirect flows and lets users
	// consent to the Copilot scopes dynamically. Organizations that do not allow
	// it can register their own public client app and set m365copilot.clientId.
	DefaultClientID = "14d82eec-204b-4c2f-b7e8-296a70dab67e"

	// CredentialsSentinel marks the provider as configured when credentials come
	// from the sign-in cache rather than from a static access token.
	CredentialsSentinel = "m365-signed-in"

	// AccessTokenEnv lets users supply a Microsoft Graph access token directly,
	// for example one copied from Graph Explorer.
	AccessTokenEnv = "M365_COPILOT_ACCESS_TOKEN"
)

// CopilotScopes are the delegated Microsoft Graph permissions the Microsoft 365
// Copilot Chat API requires. All of them are needed to call the API.
var CopilotScopes = []string{
	"Sites.Read.All",
	"Mail.Read",
	"People.Read.All",
	"OnlineMeetingTranscript.Read.All",
	"Chat.Read",
	"ChannelMessage.Read.All",
	"ExternalItem.Read.All",
}

// requestScopes adds the OpenID scopes needed for a refresh token and the account name.
func requestScopes() string {
	return strings.Join(append(append([]string{}, CopilotScopes...), "offline_access", "openid", "profile"), " ")
}

// Settings identifies the Entra ID application and tenant to sign in with.
type Settings struct {
	TenantID      string
	ClientID      string
	AuthorityHost string
}

// WithDefaults fills empty settings from the environment and the built-in defaults.
func (s Settings) WithDefaults() Settings {
	if s.TenantID == "" {
		s.TenantID = os.Getenv("M365_COPILOT_TENANT_ID")
	}
	if s.TenantID == "" {
		s.TenantID = DefaultTenant
	}
	if s.ClientID == "" {
		s.ClientID = os.Getenv("M365_COPILOT_CLIENT_ID")
	}
	if s.ClientID == "" {
		s.ClientID = DefaultClientID
	}
	if s.AuthorityHost == "" {
		s.AuthorityHost = os.Getenv("M365_COPILOT_AUTHORITY_HOST")
	}
	if s.AuthorityHost == "" {
		s.AuthorityHost = DefaultAuthorityHost
	}
	s.AuthorityHost = strings.TrimRight(s.AuthorityHost, "/")
	return s
}

func (s Settings) endpoint(name string) string {
	return fmt.Sprintf("%s/%s/oauth2/v2.0/%s", s.AuthorityHost, url.PathEscape(s.TenantID), name)
}

// CachedToken is what opencode stores on disk after a successful sign-in.
type CachedToken struct {
	TenantID      string    `json:"tenant_id"`
	ClientID      string    `json:"client_id"`
	AuthorityHost string    `json:"authority_host"`
	Scope         string    `json:"scope"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	ExpiresAt     time.Time `json:"expires_at"`
	Username      string    `json:"username,omitempty"`
}

func (t *CachedToken) settings() Settings {
	return Settings{TenantID: t.TenantID, ClientID: t.ClientID, AuthorityHost: t.AuthorityHost}.WithDefaults()
}

// TokenPath returns the file the sign-in cache is stored in.
func TokenPath() string {
	if p := os.Getenv("M365_COPILOT_TOKEN_CACHE"); p != "" {
		return p
	}
	var configDir string
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		configDir = xdgConfig
	} else if runtime.GOOS == "windows" {
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			configDir = localAppData
		} else {
			configDir = filepath.Join(os.Getenv("HOME"), "AppData", "Local")
		}
	} else {
		configDir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(configDir, "opencode", "m365copilot-auth.json")
}

// LoadCachedToken reads the sign-in cache. It returns os.ErrNotExist if the user never signed in.
func LoadCachedToken() (*CachedToken, error) {
	data, err := os.ReadFile(TokenPath())
	if err != nil {
		return nil, err
	}
	var tok CachedToken
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("invalid Microsoft 365 sign-in cache %s: %w", TokenPath(), err)
	}
	return &tok, nil
}

// SaveCachedToken writes the sign-in cache with permissions only the user can read.
func SaveCachedToken(tok *CachedToken) error {
	path := TokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	// A unique temporary file, so concurrent opencode processes refreshing the
	// token can't interleave their writes.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to write the sign-in cache: %w", err)
	}
	tmp := f.Name()
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Chmod(tmp, 0o600)
	}
	if writeErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to write %s: %w", tmp, writeErr)
	}
	return os.Rename(tmp, path)
}

// DeleteCachedToken signs the user out by removing the sign-in cache.
func DeleteCachedToken() error {
	err := os.Remove(TokenPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// HasCredentials reports whether a Microsoft Graph access token is available,
// either from the environment or from a previous sign-in.
func HasCredentials() bool {
	if os.Getenv(AccessTokenEnv) != "" {
		return true
	}
	tok, err := LoadCachedToken()
	return err == nil && (tok.RefreshToken != "" || tok.AccessToken != "")
}

// TokenSource returns Microsoft Graph access tokens for the Copilot APIs.
type TokenSource interface {
	// Token returns a valid access token, refreshing it if needed.
	Token(ctx context.Context) (string, error)
	// Invalidate forces the next Token call to get a new token, e.g. after a 401.
	Invalidate()
}

// StaticTokenSource always returns the same access token.
type StaticTokenSource string

func (s StaticTokenSource) Token(context.Context) (string, error) { return string(s), nil }
func (s StaticTokenSource) Invalidate()                           {}

// ErrNotSignedIn is returned when there is no cached sign-in.
var ErrNotSignedIn = errors.New("not signed in to Microsoft 365: run `opencode m365 login`, or set " + AccessTokenEnv)

// CacheTokenSource serves tokens from the on-disk sign-in cache and refreshes them.
type CacheTokenSource struct {
	HTTPClient *http.Client

	mu  sync.Mutex
	tok *CachedToken
}

// NewCacheTokenSource creates a token source backed by the sign-in cache.
func NewCacheTokenSource() *CacheTokenSource {
	return &CacheTokenSource{HTTPClient: &http.Client{Timeout: 60 * time.Second}}
}

// expirySkew refreshes tokens a little before they expire so in-flight requests don't fail.
const expirySkew = 2 * time.Minute

func (c *CacheTokenSource) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.tok == nil {
		tok, err := LoadCachedToken()
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotSignedIn
		}
		if err != nil {
			return "", err
		}
		c.tok = tok
	}
	if c.tok.AccessToken != "" && time.Until(c.tok.ExpiresAt) > expirySkew {
		return c.tok.AccessToken, nil
	}
	if c.tok.RefreshToken == "" {
		return "", fmt.Errorf("Microsoft 365 sign-in expired: run `opencode m365 login`")
	}

	settings := c.tok.settings()
	resp, err := requestToken(ctx, c.HTTPClient, settings, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {settings.ClientID},
		"refresh_token": {c.tok.RefreshToken},
		"scope":         {requestScopes()},
	})
	if err != nil {
		var oauthErr *OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Code == "invalid_grant" {
			return "", fmt.Errorf("Microsoft 365 sign-in expired or was revoked, run `opencode m365 login` again: %w", err)
		}
		return "", fmt.Errorf("failed to refresh Microsoft 365 access token: %w", err)
	}
	updated := resp.toCachedToken(settings)
	if updated.RefreshToken == "" {
		updated.RefreshToken = c.tok.RefreshToken
	}
	if updated.Username == "" {
		updated.Username = c.tok.Username
	}
	c.tok = updated
	if err := SaveCachedToken(updated); err != nil {
		// The new token still works for this process; only persistence failed.
		fmt.Fprintf(os.Stderr, "warning: failed to save Microsoft 365 sign-in cache: %v\n", err)
	}
	return c.tok.AccessToken, nil
}

func (c *CacheTokenSource) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != nil {
		c.tok.AccessToken = ""
	}
}

// OAuthError is an error response from the Microsoft identity platform.
type OAuthError struct {
	StatusCode  int
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *OAuthError) Error() string {
	desc := e.Description
	// Entra descriptions include trace and correlation IDs on later lines.
	if i := strings.IndexAny(desc, "\r\n"); i >= 0 {
		desc = desc[:i]
	}
	if desc == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, desc)
}

type tokenResponse struct {
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

func (r *tokenResponse) toCachedToken(s Settings) *CachedToken {
	expiresIn := r.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return &CachedToken{
		TenantID:      s.TenantID,
		ClientID:      s.ClientID,
		AuthorityHost: s.AuthorityHost,
		Scope:         r.Scope,
		AccessToken:   r.AccessToken,
		RefreshToken:  r.RefreshToken,
		ExpiresAt:     time.Now().Add(time.Duration(expiresIn) * time.Second),
		Username:      usernameFromIDToken(r.IDToken),
	}
}

func requestToken(ctx context.Context, client *http.Client, s Settings, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint("token"), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		oauthErr := &OAuthError{StatusCode: resp.StatusCode}
		if json.Unmarshal(body, oauthErr) != nil || oauthErr.Code == "" {
			oauthErr.Code = resp.Status
			oauthErr.Description = strings.TrimSpace(string(body))
		}
		return nil, oauthErr
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("invalid token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token response has no access_token")
	}
	return &tr, nil
}

// usernameFromIDToken reads the account name from an ID token for display only.
// The token comes straight from the token endpoint over TLS, so it isn't verified.
func usernameFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
		UPN               string `json:"upn"`
		Name              string `json:"name"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	for _, v := range []string{claims.PreferredUsername, claims.UPN, claims.Name} {
		if v != "" {
			return v
		}
	}
	return ""
}
