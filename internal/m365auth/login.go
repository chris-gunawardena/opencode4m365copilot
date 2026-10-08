package m365auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// DeviceCode is the user-facing part of a device code sign-in.
type DeviceCode struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
}

// LoginWithDeviceCode signs the user in with the OAuth 2.0 device authorization grant.
// prompt is called once with the code the user has to enter at the verification URL.
func LoginWithDeviceCode(ctx context.Context, client *http.Client, s Settings, prompt func(DeviceCode)) (*CachedToken, error) {
	s = s.WithDefaults()
	form := url.Values{"client_id": {s.ClientID}, "scope": {requestScopes()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint("devicecode"), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		oauthErr := &OAuthError{StatusCode: resp.StatusCode}
		if json.Unmarshal(body, oauthErr) != nil || oauthErr.Code == "" {
			oauthErr.Code = resp.Status
			oauthErr.Description = strings.TrimSpace(string(body))
		}
		return nil, fmt.Errorf("device code request failed: %w", oauthErr)
	}
	var dc DeviceCode
	if err := json.Unmarshal(body, &dc); err != nil {
		return nil, fmt.Errorf("invalid device code response: %w", err)
	}
	if dc.Message == "" {
		dc.Message = fmt.Sprintf("To sign in, open %s and enter the code %s", dc.VerificationURI, dc.UserCode)
	}
	prompt(dc)

	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresIn := dc.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the device code expired before sign-in completed")
		}
		tr, err := requestToken(ctx, client, s, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {s.ClientID},
			"device_code": {dc.DeviceCode},
		})
		if err == nil {
			return tr.toCachedToken(s), nil
		}
		var oauthErr *OAuthError
		if !errors.As(err, &oauthErr) {
			return nil, err
		}
		switch oauthErr.Code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return nil, err
		}
	}
}

// LoginWithBrowser signs the user in with the authorization code flow and PKCE,
// receiving the redirect on a loopback address. openURL is called with the
// sign-in URL; pass nil to open it in the default browser.
func LoginWithBrowser(ctx context.Context, client *http.Client, s Settings, openURL func(string) error) (*CachedToken, error) {
	s = s.WithDefaults()
	if openURL == nil {
		openURL = OpenBrowser
	}

	// Entra ID accepts any port for http://localhost redirects of public clients.
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, fmt.Errorf("failed to listen for the sign-in redirect: %w", err)
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port)

	verifier := randomString(64)
	challenge := sha256.Sum256([]byte(verifier))
	state := randomString(24)

	authURL := s.endpoint("authorize") + "?" + url.Values{
		"client_id":             {s.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"response_mode":         {"query"},
		"scope":                 {requestScopes()},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}.Encode()

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("code") == "" && q.Get("error") == "" {
				http.NotFound(w, r)
				return
			}
			var res result
			switch {
			case q.Get("state") != state:
				res.err = errors.New("sign-in redirect had an unexpected state parameter")
			case q.Get("error") != "":
				res.err = &OAuthError{Code: q.Get("error"), Description: q.Get("error_description")}
			default:
				res.code = q.Get("code")
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if res.err != nil {
				fmt.Fprintf(w, "<html><body><h3>Sign-in failed</h3><p>%s</p><p>You can close this tab.</p></body></html>", html.EscapeString(res.err.Error()))
			} else {
				fmt.Fprint(w, "<html><body><h3>Signed in to Microsoft 365</h3><p>You can close this tab and return to opencode.</p></body></html>")
			}
			select {
			case results <- res:
			default:
			}
		}),
	}
	go server.Serve(listener)
	defer server.Close()

	if err := openURL(authURL); err != nil {
		return nil, fmt.Errorf("failed to open the browser (try --device-code): %w", err)
	}

	var res result
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res = <-results:
	}
	if res.err != nil {
		return nil, res.err
	}

	tr, err := requestToken(ctx, client, s, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.ClientID},
		"code":          {res.code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"scope":         {requestScopes()},
	})
	if err != nil {
		return nil, err
	}
	return tr.toCachedToken(s), nil
}

// OpenBrowser opens url in the user's default browser.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}
