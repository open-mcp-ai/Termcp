package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestConfigurationPasswordRouteChangesAdminPassword(t *testing.T) {
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
	h := extensionTestHandler(t)
	h.SSH = vault
	h.ChangeMainPassword = account.ChangePassword
	mux := http.NewServeMux()
	h.Register(mux)
	server := httptest.NewServer(account.Wrap(mux))
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	status, err := client.Get(server.URL + "/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		CSRF string `json:"csrf"`
	}
	err = json.NewDecoder(status.Body).Decode(&state)
	status.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, body string) *http.Response {
		t.Helper()
		request, err := http.NewRequest("POST", server.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", server.URL)
		request.Header.Set("X-Termcp-CSRF", state.CSRF)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := post("/auth/setup", `{"username":"test-admin","password":"sample-password-for-tests"}`)
	if response.StatusCode != 200 {
		response.Body.Close()
		t.Fatalf("setup: %d", response.StatusCode)
	}
	err = json.NewDecoder(response.Body).Decode(&state)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	response = post("/api/config-storage/password", `{"current_password":"sample-password-for-tests","new_password":"replacement-master-password","confirmation":"replacement-master-password"}`)
	if response.StatusCode != 200 || response.Header.Get("X-Termcp-Reauthenticate") != "1" {
		response.Body.Close()
		t.Fatalf("unified password route: %d, missing sign-in instruction", response.StatusCode)
	}
	response.Body.Close()
	response, err = client.Get(server.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("old Admin session survived configuration password change")
	}
	response, err = client.Get(server.URL + "/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewDecoder(response.Body).Decode(&state)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	response = post("/auth/login", `{"username":"test-admin","password":"replacement-master-password"}`)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("new configuration password did not become Admin password")
	}
}
