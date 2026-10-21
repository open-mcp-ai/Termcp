package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/vault"
)

// vaultTestHandler wires a Handler with a real vault in a per-test dir, on a
// mux carrying only the vault routes — the unit under test is the HTTP
// translation, not the vault (its own package tests cover that).
func vaultTestHandler(t *testing.T, v *vault.Vault) *httptest.Server {
	t.Helper()
	h := &Handler{Vault: v}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vault", h.handleVaultStatus)
	mux.HandleFunc("POST /api/vault/unlock", h.handleVaultUnlock)
	mux.HandleFunc("POST /api/vault/lock", h.handleVaultLock)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestVaultEndpointsWithoutAVault(t *testing.T) {
	srv := vaultTestHandler(t, vault.New(t.TempDir()))

	resp, err := http.Get(srv.URL + "/api/vault")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st["initialized"] != false || st["unlocked"] != false {
		t.Fatalf("status without vault: %v", st)
	}

	for _, path := range []string{"/api/vault/unlock", "/api/vault/lock"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{"passphrase":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s without vault: got %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestVaultUnlockLockCycle(t *testing.T) {
	v := vault.New(t.TempDir())
	if _, err := v.Init("endpoint passphrase"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v.Lock()
	srv := vaultTestHandler(t, v)

	// Wrong passphrase: 401, and the vault stays locked.
	resp, err := http.Post(srv.URL+"/api/vault/unlock", "application/json", strings.NewReader(`{"passphrase":"wrong"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong passphrase: got %d, want 401", resp.StatusCode)
	}
	if v.IsUnlocked() {
		t.Fatal("vault unlocked by a wrong passphrase")
	}

	// Right passphrase: 204 and the state flips.
	resp, err = http.Post(srv.URL+"/api/vault/unlock", "application/json", strings.NewReader(`{"passphrase":"endpoint passphrase"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unlock: got %d, want 204", resp.StatusCode)
	}
	if !v.IsUnlocked() {
		t.Fatal("vault still locked after a good unlock")
	}

	// Lock: 204 and the state flips back.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/vault/lock", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("lock: got %d, want 204", resp.StatusCode)
	}
	if v.IsUnlocked() {
		t.Fatal("vault still unlocked after lock")
	}
}

func TestVaultStatusReportsProfiles(t *testing.T) {
	v := vault.New(t.TempDir())
	if _, err := v.Init("status passphrase"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := v.Encrypt("prod", []byte("kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n")); err != nil {
		t.Fatal(err)
	}
	srv := vaultTestHandler(t, v)

	resp, err := http.Get(srv.URL + "/api/vault")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Initialized bool     `json:"initialized"`
		Unlocked    bool     `json:"unlocked"`
		Profiles    []string `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if !st.Initialized || !st.Unlocked {
		t.Fatalf("status: %+v", st)
	}
	if len(st.Profiles) != 1 || st.Profiles[0] != "prod" {
		t.Fatalf("profiles: %v, want [prod]", st.Profiles)
	}
}
