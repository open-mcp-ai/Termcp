package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// Exercise the rendered form over HTTP, including the browser's cookie jar;
// handler-only tests that supply state themselves cannot detect a broken form.
func TestBuiltinOAuthRenderedConsentSubmission(t *testing.T) {
	m, _, _ := configuredManaged(t)
	m.enableOAuthTestMode()
	idp := newTestOIDCProvider(t)
	public := httptest.NewServer(m.Wrap(http.NotFoundHandler()))
	t.Cleanup(public.Close)
	_, _, err := m.updateOAuthServerSettings(context.Background(), oauthSettingsInput{
		Enabled: true, PublicURL: public.URL, OIDCIssuer: idp.server.URL,
		OIDCClientID: idp.clientID, OIDCClientSecret: "test-secret",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	callbackServer := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(callbackServer.Close)
	callback := callbackServer.URL + "/callback"
	registered := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPost, "/oauth/register",
		fmt.Sprintf(`{"redirect_uris":[%q],"client_name":"Consent browser test","token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"],"application_type":"native"}`, callback), "", "")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registered.Code, registered.Body)
	}
	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(registered.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("v", 64)
	hash := sha256.Sum256([]byte(verifier))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(r.URL.String(), callback) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	authorizeURL := public.URL + "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {registration.ClientID}, "redirect_uri": {callback},
		"scope": {"mcp offline_access"}, "state": {"browser-client-state"}, "resource": {public.URL + "/stream"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	response, err := client.Get(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", response.StatusCode, body)
	}
	// Browsers apply form-action to the redirect chain too. The client's
	// validated callback uses a different origin (notably a loopback port).
	policy := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "form-action 'self' "+callbackServer.URL+";") {
		t.Fatalf("consent policy blocks the client's callback origin: %s", policy)
	}
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	action := ""
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		attrs := map[string]string{}
		for _, attr := range node.Attr {
			attrs[attr.Key] = attr.Val
		}
		if node.Type == html.ElementNode && node.Data == "form" {
			action = attrs["action"]
		}
		if node.Type == html.ElementNode && node.Data == "input" && attrs["type"] == "hidden" {
			form.Add(attrs["name"], attrs["value"])
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	if action == "" || form.Get("state") == "" || form.Get("csrf") == "" {
		t.Fatal("consent form is incomplete")
	}
	form.Set("decision", "approve")
	request, err := http.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", public.URL)
	approved, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(approved.Body)
	approved.Body.Close()
	if approved.StatusCode != http.StatusFound {
		t.Fatalf("confirm access: %d %s", approved.StatusCode, result)
	}
	target, err := url.Parse(approved.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Query().Get("state") != "browser-client-state" || target.Query().Get("code") == "" {
		t.Fatal("confirmation did not return an authorization code to the client")
	}
	// Confirmation remains single-use even when the browser retries a POST.
	replayed, err := http.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	replayed.Header = request.Header.Clone()
	replayResponse, err := client.Do(replayed)
	if err != nil {
		t.Fatal(err)
	}
	replayResponse.Body.Close()
	if replayResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed confirmation returned %d", replayResponse.StatusCode)
	}
}

func TestOAuthConsentCSPCallbackOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, callback, source string
	}{
		{"loopback", "http://127.0.0.1:19876/callback?opaque=test-value", "http://127.0.0.1:19876"},
		{"localhost", "http://localhost:19876/callback", "http://localhost:19876"},
		{"ipv6", "http://[::1]:19876/callback", "http://[::1]:19876"},
		{"https", "https://client.example/callback?opaque=test-value", "https://client.example"},
		{"no-callback", "", ""},
		{"insecure-remote", "http://client.example/callback", ""},
		{"wildcard", "https://*.example/callback", ""},
		{"directive-separator", "https://client.example;script-src/callback", ""},
		{"policy-separator", "https://client.example,other.example/callback", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var callback *url.URL
			if tc.callback != "" {
				var err error
				callback, err = url.Parse(tc.callback)
				if err != nil {
					t.Fatal(err)
				}
			}
			policy := oauthConsentCSP(callback)
			wantSources := "'self'"
			if tc.source != "" {
				wantSources += " " + tc.source
			}
			var sources string
			for _, directive := range strings.Split(policy, ";") {
				if value, ok := strings.CutPrefix(strings.TrimSpace(directive), "form-action "); ok {
					sources = value
				}
			}
			if sources != wantSources {
				t.Fatalf("form-action = %q, want %q", sources, wantSources)
			}
			if strings.Contains(policy, "opaque") || strings.Contains(policy, "test-value") {
				t.Fatal("callback query was included in CSP")
			}
			for _, required := range []string{"default-src 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
				if !strings.Contains(policy, required) {
					t.Fatalf("consent policy lost %s", required)
				}
			}
		})
	}
}
