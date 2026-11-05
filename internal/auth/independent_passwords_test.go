package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestIndependentPasswordsAndAuthenticatedLockedVault(t *testing.T) {
	m := openManaged(t)
	if err := m.setup("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	cookie, _, err := m.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	vault := m.vault.(*sshconfig.Store)
	if err = vault.SetNewProfileEncryption(true); err != nil {
		t.Fatal(err)
	}
	if err = vault.Save("saved-host", []byte(testHost)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configstore.LocalStatePath(filepath.Dir(m.path)))
	if err != nil {
		t.Fatal(err)
	}
	var original struct {
		Keyring   json.RawMessage   `json:"keyring"`
		Envelopes map[string][]byte `json:"envelopes"`
	}
	if err = json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	loginPassword := "replacement-login-password"
	if err = m.ChangePassword(testPassword, loginPassword, loginPassword); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(configstore.LocalStatePath(filepath.Dir(m.path)))
	if err != nil {
		t.Fatal(err)
	}
	var current struct {
		Keyring   json.RawMessage   `json:"keyring"`
		Envelopes map[string][]byte `json:"envelopes"`
	}
	if err = json.Unmarshal(after, &current); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original.Keyring, current.Keyring) || !reflect.DeepEqual(original.Envelopes, current.Envelopes) {
		t.Fatal("login password replacement changed MK or EDKs")
	}
	if _, _, err = m.verify(cookie); err == nil {
		t.Fatal("old browser session survived login-password replacement")
	}
	if err = vault.UnlockAuthentication(loginPassword); err == nil {
		t.Fatal("login password implicitly replaced configuration password")
	}
	configPassword := "replacement-configuration-password"
	if err = vault.ChangeEncryptionPassword(testPassword, configPassword, configPassword, m.providers...); err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.login("admin", configPassword); err == nil {
		t.Fatal("configuration password replaced login password")
	}
	m, err = reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if !m.NeedsUnlock() {
		t.Fatal("manual vault did not restart locked")
	}
	cookie, session, err := m.login("admin", loginPassword)
	if err != nil {
		t.Fatal("login verifier depended on a locked MK", err)
	}
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	if w := managedRequest(t, h, "GET", "/api/sessions", "", cookie, ""); w.Code != 423 {
		t.Fatalf("authenticated locked vault business request: %d", w.Code)
	}
	if w := managedRequest(t, h, "GET", "/auth/status", "", cookie, ""); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"authenticated":true`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"locked":true`)) {
		t.Fatal("missing authenticated/locked state", w.Body.String())
	}
	if w := managedRequest(t, h, "POST", "/auth/unlock", `{"password":"`+loginPassword+`"}`, cookie, session.CSRF); w.Code != 400 || !strings.Contains(w.Body.String(), "matches the login password") {
		t.Fatalf("login password unlocked an independent vault or lacked the diagnostic: %d %s", w.Code, w.Body.String())
	}
	if w := managedRequest(t, h, "POST", "/auth/unlock", `{"password":"`+configPassword+`"}`, cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, _, err = m.verify(cookie); err != nil {
		t.Fatal("vault unlock discarded the current browser identity", err)
	}
	if w := managedRequest(t, h, "GET", "/api/sessions", "", cookie, ""); w.Code != 204 {
		t.Fatal("unlocked session could not continue", w.Code)
	}
	data, err := m.vault.(*sshconfig.Store).Load("saved-host")
	if err != nil || data.Host != "host.example" {
		t.Fatal("password changes lost configuration", err)
	}
	if bytes.Contains(after, []byte(testPassword)) || bytes.Contains(after, []byte(loginPassword)) {
		t.Fatal("plaintext passwords persisted")
	}
}

func TestIndependentLoginPasswordCanChangeWhileVaultLocked(t *testing.T) {
	m := openManaged(t)
	if err := m.setup("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	var err error
	m, err = reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.login("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	replacement := "login-password-changed-while-locked"
	if err = m.ChangePassword(testPassword, replacement, replacement); err != nil {
		t.Fatal(err)
	}
	if !m.NeedsUnlock() {
		t.Fatal("changing login password unlocked the vault")
	}
	if _, _, err = m.login("admin", replacement); err != nil {
		t.Fatal(err)
	}
	if err = m.UnlockVault(testPassword); err != nil {
		t.Fatal("old configuration password stopped working", err)
	}
}

func TestExistingVaultDoesNotAllowShortLoginPassword(t *testing.T) {
	m := openManaged(t)
	if err := m.vault.PrepareAuthentication(testPassword, "", false, m.providers...); err != nil {
		t.Fatal(err)
	}
	if err := m.setupWithOptions("admin", "short", testPassword, false); err == nil {
		t.Fatal("existing encryption password bypassed login password policy")
	}
	if m.Configured() {
		t.Fatal("failed registration published login credentials")
	}
}
