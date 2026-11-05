package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
)

const managedSessionCookie = "termcp_session"

// ErrManagedAuthentication reports that an endpoint does not expose the
// managed local-password login flow.
var ErrManagedAuthentication = errors.New("managed authentication is unavailable")

// ManagedAuthStatus is the public pre-login state used to choose the prompts.
type ManagedAuthStatus struct {
	Configured bool
	Locked     bool
}

// ManagedLogin keeps the pre-login cookie and CSRF challenge for one login.
type ManagedLogin struct {
	baseURL string
	client  *http.Client
	status  ManagedAuthStatus
	csrf    string
}

// ManagedAdminSession is an authenticated Admin browser session used only by
// daemon management.
type ManagedAdminSession struct {
	cookie string
	csrf   string
}

// BeginManagedLogin starts a managed local-password login and returns the public state
// needed to decide whether the vault password must also be requested.
func BeginManagedLogin(ctx context.Context, host string, port int) (*ManagedLogin, error) {
	baseURL, err := ManagementURL(host, port, "")
	if err != nil {
		return nil, err
	}
	return BeginManagedLoginURL(ctx, baseURL)
}

// BeginManagedLoginURL starts a managed login at an already-validated origin.
func BeginManagedLoginURL(ctx context.Context, baseURL string) (*ManagedLogin, error) {
	baseURL, err := ManagementURL("", 0, baseURL)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return beginManagedLoginWithClient(ctx, baseURL, &http.Client{Jar: jar})
}

func beginManagedLoginWithClient(ctx context.Context, baseURL string, client *http.Client) (*ManagedLogin, error) {
	if client == nil {
		return nil, errors.New("managed login HTTP client is required")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errManagementRedirect
	}
	login := &ManagedLogin{baseURL: baseURL, client: &c}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/auth/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := login.client.Do(req)
	if err != nil {
		if errors.Is(err, errManagementRedirect) {
			return nil, fmt.Errorf("%w: /auth/status redirects are not followed", ErrUnsafeManagementURL)
		}
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrManagedAuthentication
	default:
		return nil, fmt.Errorf("/auth/status answered %s", resp.Status)
	}
	var status struct {
		Mode       string `json:"mode"`
		Configured bool   `json:"configured"`
		Locked     bool   `json:"locked"`
		CSRF       string `json:"csrf"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&status); err != nil {
		return nil, ErrManagedAuthentication
	}
	if status.Mode != "managed" {
		return nil, ErrManagedAuthentication
	}
	if status.CSRF == "" {
		return nil, errors.New("/auth/status returned no CSRF token")
	}
	login.status = ManagedAuthStatus{Configured: status.Configured, Locked: status.Locked}
	login.csrf = status.CSRF
	return login, nil
}

// Status reports whether the local identity exists and the vault is locked.
func (l *ManagedLogin) Status() ManagedAuthStatus {
	return l.status
}

// Login exchanges the local login password for a browser session. When the
// vault is locked, configPassword supplies the independent decryption password.
func (l *ManagedLogin) Login(ctx context.Context, password, configPassword string) (ManagedAdminSession, error) {
	body, err := json.Marshal(map[string]any{
		"password":        password,
		"config_password": configPassword,
	})
	if err != nil {
		return ManagedAdminSession{}, err
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/auth/login", bytes.NewReader(body))
	if err != nil {
		return ManagedAdminSession{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Termcp-CSRF", l.csrf)
	resp, err := l.client.Do(req)
	if err != nil {
		if errors.Is(err, errManagementRedirect) {
			return ManagedAdminSession{}, fmt.Errorf("%w: /auth/login redirects are not followed", ErrUnsafeManagementURL)
		}
		return ManagedAdminSession{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ManagedAdminSession{}, fmt.Errorf("login was rejected; check the login password, plus the decryption password when the vault is locked")
	}
	var result struct {
		CSRF   string `json:"csrf"`
		Locked bool   `json:"locked"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&result); err != nil {
		return ManagedAdminSession{}, errors.New("login returned an invalid response")
	}
	if result.Locked {
		return ManagedAdminSession{}, errors.New("configuration vault is still locked")
	}
	if result.CSRF == "" {
		return ManagedAdminSession{}, errors.New("login returned no CSRF token")
	}
	origin, err := url.Parse(l.baseURL)
	if err != nil {
		return ManagedAdminSession{}, err
	}
	for _, cookie := range l.client.Jar.Cookies(origin) {
		if cookie.Name == managedSessionCookie && cookie.Value != "" {
			return ManagedAdminSession{cookie: cookie.Value, csrf: result.CSRF}, nil
		}
	}
	return ManagedAdminSession{}, errors.New("login returned no session cookie")
}
