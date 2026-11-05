package webui

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestManagedLogoutClosesActiveWebSocket(t *testing.T) {
	dir := t.TempDir()
	vault, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vault.Close() })
	account, err := auth.OpenManaged(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	extensionTestHandler(t).Register(mux)
	server := httptest.NewServer(account.Wrap(mux))
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	response, err := client.Get(server.URL + "/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		CSRF string `json:"csrf"`
	}
	err = json.NewDecoder(response.Body).Decode(&state)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, body []byte, csrf string) *http.Response {
		t.Helper()
		request, err := http.NewRequest("POST", server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Termcp-CSRF", csrf)
		request.Header.Set("Origin", server.URL)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response = post("/auth/setup", []byte(`{"password":"sample-password-for-tests","config_password":"sample-password-for-tests"}`), state.CSRF)
	if response.StatusCode != 200 {
		response.Body.Close()
		t.Fatalf("setup: %d", response.StatusCode)
	}
	err = json.NewDecoder(response.Body).Decode(&state)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse(server.URL)
	header := http.Header{"Origin": {server.URL}}
	for _, cookie := range jar.Cookies(origin) {
		header.Add("Cookie", cookie.String())
	}
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/ui/ws", header)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	response = post("/auth/logout", nil, state.CSRF)
	if response.StatusCode != 200 {
		response.Body.Close()
		t.Fatalf("logout: %d", response.StatusCode)
	}
	response.Body.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("revoked WebSocket remained open")
		}
		break
	}
}
