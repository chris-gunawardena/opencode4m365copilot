package m365auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeIDToken(username string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"none"}`))
	payload := enc.EncodeToString([]byte(fmt.Sprintf(`{"preferred_username":%q}`, username)))
	return header + "." + payload + ".sig"
}

// identityServer imitates the Microsoft identity platform token endpoints.
type identityServer struct {
	t           *testing.T
	mu          sync.Mutex
	forms       []url.Values
	pendingLeft int
	dropLeft    int // drop this many device code polls without answering, like a network failure
	challenge   string
	refreshErr  string
}

func (s *identityServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		s.t.Fatal(err)
	}
	s.forms = append(s.forms, r.PostForm)
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/contoso/oauth2/v2.0/devicecode":
		fmt.Fprint(w, `{"device_code":"dev-123","user_code":"ABCD-EFGH","verification_uri":"https://microsoft.com/devicelogin","expires_in":60,"interval":1,"message":"Enter ABCD-EFGH at https://microsoft.com/devicelogin"}`)
	case "/contoso/oauth2/v2.0/token":
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if s.dropLeft > 0 {
				s.dropLeft--
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
			if s.pendingLeft > 0 {
				s.pendingLeft--
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"authorization_pending","error_description":"AADSTS70016: pending"}`)
				return
			}
			s.writeToken(w, "access-device", "refresh-device")
		case "refresh_token":
			if s.refreshErr != "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q,"error_description":"AADSTS70008: The refresh token has expired.\r\nTrace ID: x"}`, s.refreshErr)
				return
			}
			s.writeToken(w, "access-refreshed", "refresh-rotated")
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if r.PostForm.Get("code") != "auth-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != s.challenge {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"bad code or verifier"}`)
				return
			}
			s.writeToken(w, "access-browser", "refresh-browser")
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"unsupported_grant_type"}`)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *identityServer) writeToken(w http.ResponseWriter, access, refresh string) {
	json.NewEncoder(w).Encode(map[string]any{
		"token_type":    "Bearer",
		"scope":         strings.Join(CopilotScopes, " "),
		"expires_in":    3600,
		"access_token":  access,
		"refresh_token": refresh,
		"id_token":      fakeIDToken("adele@contoso.com"),
	})
}

func newIdentityServer(t *testing.T) (*identityServer, Settings) {
	s := &identityServer{t: t}
	server := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(server.Close)
	return s, Settings{TenantID: "contoso", ClientID: "client-1", AuthorityHost: server.URL}
}

func useTempCache(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("M365_COPILOT_TOKEN_CACHE", path)
	t.Setenv(AccessTokenEnv, "")
	return path
}

func TestLoginWithDeviceCode(t *testing.T) {
	idp, settings := newIdentityServer(t)
	idp.pendingLeft = 1

	var shown DeviceCode
	tok, err := LoginWithDeviceCode(context.Background(), http.DefaultClient, settings, func(dc DeviceCode) { shown = dc })
	if err != nil {
		t.Fatal(err)
	}
	if shown.UserCode != "ABCD-EFGH" {
		t.Errorf("prompt got %+v", shown)
	}
	if tok.AccessToken != "access-device" || tok.RefreshToken != "refresh-device" || tok.Username != "adele@contoso.com" {
		t.Errorf("unexpected token %+v", tok)
	}
	if tok.TenantID != "contoso" || tok.ClientID != "client-1" {
		t.Errorf("token should remember where it came from: %+v", tok)
	}
	scope := idp.forms[0].Get("scope")
	for _, want := range append([]string{"offline_access"}, CopilotScopes...) {
		if !strings.Contains(scope, want) {
			t.Errorf("device code request scope %q is missing %s", scope, want)
		}
	}
}

func TestLoginWithDeviceCodeSurvivesNetworkErrors(t *testing.T) {
	idp, settings := newIdentityServer(t)
	idp.dropLeft = 1
	tok, err := LoginWithDeviceCode(context.Background(), http.DefaultClient, settings, func(DeviceCode) {})
	if err != nil {
		t.Fatalf("a dropped poll shouldn't end the sign-in: %v", err)
	}
	if tok.AccessToken != "access-device" {
		t.Fatalf("unexpected token %+v", tok)
	}
}

func TestLoginWithBrowser(t *testing.T) {
	idp, settings := newIdentityServer(t)
	tok, err := LoginWithBrowser(context.Background(), http.DefaultClient, settings, func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "client-1" {
			return fmt.Errorf("unexpected authorize URL %s", authURL)
		}
		idp.mu.Lock()
		idp.challenge = q.Get("code_challenge")
		idp.mu.Unlock()
		// Play the browser: follow the redirect back to opencode.
		go func() {
			redirect := q.Get("redirect_uri") + "/?code=auth-code&state=" + url.QueryEscape(q.Get("state"))
			resp, err := http.Get(redirect)
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "access-browser" || tok.Username != "adele@contoso.com" {
		t.Fatalf("unexpected token %+v", tok)
	}
}

func TestLoginWithBrowserRejectsWrongState(t *testing.T) {
	_, settings := newIdentityServer(t)
	_, err := LoginWithBrowser(context.Background(), http.DefaultClient, settings, func(authURL string) error {
		u, _ := url.Parse(authURL)
		go func() {
			resp, err := http.Get(u.Query().Get("redirect_uri") + "/?code=auth-code&state=forged")
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("expected a state error, got %v", err)
	}
}

func TestCacheTokenSourceRefreshesExpiredToken(t *testing.T) {
	path := useTempCache(t)
	idp, settings := newIdentityServer(t)
	if err := SaveCachedToken(&CachedToken{
		TenantID: settings.TenantID, ClientID: settings.ClientID, AuthorityHost: settings.AuthorityHost,
		AccessToken: "old", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute), Username: "adele@contoso.com",
	}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file should be private: %v %v", info.Mode(), err)
	}
	if !HasCredentials() {
		t.Fatal("HasCredentials should see the cached sign-in")
	}

	ts := NewCacheTokenSource()
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-refreshed" {
		t.Fatalf("got %q", token)
	}
	form := idp.forms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "refresh-1" || form.Get("client_id") != "client-1" {
		t.Fatalf("unexpected refresh request %v", form)
	}

	// The rotated refresh token is persisted, and a valid token isn't refreshed again.
	saved, err := LoadCachedToken()
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "refresh-rotated" || saved.AccessToken != "access-refreshed" {
		t.Fatalf("cache not updated: %+v", saved)
	}
	if _, err := ts.Token(context.Background()); err != nil || len(idp.forms) != 1 {
		t.Fatalf("valid token should be reused (requests: %d, err: %v)", len(idp.forms), err)
	}

	// After a 401 the token is refreshed even if it hasn't expired.
	ts.Invalidate()
	if _, err := ts.Token(context.Background()); err != nil || len(idp.forms) != 2 {
		t.Fatalf("invalidated token should be refreshed (requests: %d, err: %v)", len(idp.forms), err)
	}
}

func TestCacheTokenSourceRevokedRefreshToken(t *testing.T) {
	useTempCache(t)
	idp, settings := newIdentityServer(t)
	idp.refreshErr = "invalid_grant"
	SaveCachedToken(&CachedToken{
		TenantID: settings.TenantID, ClientID: settings.ClientID, AuthorityHost: settings.AuthorityHost,
		RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute),
	})
	_, err := NewCacheTokenSource().Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "opencode m365 login") || !strings.Contains(err.Error(), "AADSTS70008") {
		t.Fatalf("expected a sign-in-again error, got %v", err)
	}
	if strings.Contains(err.Error(), "Trace ID") {
		t.Errorf("error should drop Entra's trace lines: %v", err)
	}
}

func TestNotSignedIn(t *testing.T) {
	useTempCache(t)
	if HasCredentials() {
		t.Fatal("no credentials expected")
	}
	if _, err := NewCacheTokenSource().Token(context.Background()); err != ErrNotSignedIn {
		t.Fatalf("expected ErrNotSignedIn, got %v", err)
	}
	t.Setenv(AccessTokenEnv, "token-from-env")
	if !HasCredentials() {
		t.Fatal("an access token in the environment counts as credentials")
	}
	if err := DeleteCachedToken(); err != nil {
		t.Fatalf("logging out without a cache should succeed: %v", err)
	}
}

func TestSettingsDefaults(t *testing.T) {
	t.Setenv("M365_COPILOT_TENANT_ID", "")
	t.Setenv("M365_COPILOT_CLIENT_ID", "")
	t.Setenv("M365_COPILOT_AUTHORITY_HOST", "")
	s := Settings{}.WithDefaults()
	if s.TenantID != DefaultTenant || s.ClientID != DefaultClientID || s.AuthorityHost != DefaultAuthorityHost {
		t.Fatalf("unexpected defaults %+v", s)
	}
	if got := s.endpoint("token"); got != "https://login.microsoftonline.com/organizations/oauth2/v2.0/token" {
		t.Fatalf("unexpected endpoint %s", got)
	}
}
