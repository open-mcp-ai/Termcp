package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

const testHost = "kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n"

func TestManagedMigrationUnifiesExistingConfigurationPassword(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMCP_TEST_AUTH_KEK", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	provider := configstore.NewEnvProvider("TERMCP_TEST_AUTH_KEK")
	previous := "previous-configuration-password"
	repo, err := configstore.InitializeRepositoryWithPassword(dir, previous, previous, provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save("saved-host", []byte(testHost)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := randomSecret()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	legacy := managedState{Version: 1, InstanceID: "existing-instance", SigningKey: key, Admin: adminRecord{ID: "existing-admin", Username: "admin", Password: testPassword}, Permissions: defaultPolicy(), Revision: 7, Sessions: map[string]sessionRecord{"old-session": {Actor: Actor{ID: "existing-admin", Username: "admin", Role: RoleAdmin}, ExpiresAt: expires, CSRF: "old-session-csrf"}}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	oldKey, _ := base64.RawURLEncoding.DecodeString(key)
	oldCookie, err := jwt.NewWithClaims(jwt.SigningMethodHS256, sessionClaims{Role: RoleAdmin, RegisteredClaims: jwt.RegisteredClaims{ID: "old-session", Issuer: legacy.InstanceID, Subject: legacy.Admin.ID, Audience: jwt.ClaimStrings{"termcp-web"}, ExpiresAt: jwt.NewNumericDate(expires)}}).SignedString(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	m := openManagedAt(t, dir)
	if !m.Configured() || m.legacy == nil {
		t.Fatal("legacy account became fresh setup")
	}
	if _, _, err := m.verify(oldCookie); err == nil {
		t.Fatal("legacy JWT bypassed migration")
	}
	if _, _, err := m.login("admin", testPassword); err == nil {
		t.Fatal("unified without the old configuration password")
	}
	if _, _, err := m.loginWithOptions("admin", testPassword, "wrong-password", false); err == nil {
		t.Fatal("wrong old encryption password accepted")
	}
	if _, _, err := m.loginWithOptions("admin", testPassword, previous, false); err != nil {
		t.Fatal(err)
	}
	if m.state.Admin.ID != legacy.Admin.ID || m.state.InstanceID != legacy.InstanceID || m.state.Revision != 7 {
		t.Fatal("migration lost identity or permissions")
	}
	if m.state.SigningKey == key || m.state.Admin.Password != "" {
		t.Fatal("legacy secrets retained")
	}
	if _, _, err := m.verify(oldCookie); err == nil {
		t.Fatal("legacy JWT survived migration")
	}
	for _, name := range []string{"auth.json", "auth.enc", "keys/config.keys"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(testPassword)) || bytes.Contains(data, []byte(previous)) || bytes.Contains(data, []byte(m.state.SigningKey)) {
			t.Fatalf("secret persisted in %s", name)
		}
	}
	if err := m.vault.(*sshconfig.Store).Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = configstore.OpenRepository(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if data, err := repo.Read("saved-host"); err != nil || string(data) != testHost {
		t.Fatalf("migration damaged encrypted host: %v", err)
	}
	if err := repo.VerifyPassword(testPassword); err != nil {
		t.Fatal("Admin password did not become Main Key password", err)
	}
	if err := repo.VerifyPassword(previous); err == nil {
		t.Fatal("old configuration password remained valid")
	}
}

func TestManagedPasswordChangeKeepsHostsAndRevokesSessions(t *testing.T) {
	m, token, _ := configuredManaged(t)
	vault := m.vault.(*sshconfig.Store)
	if err := vault.SetNewProfileEncryption(true); err != nil {
		t.Fatal(err)
	}
	if err := vault.Save("encrypted-host", []byte(testHost)); err != nil {
		t.Fatal(err)
	}
	before, err := vault.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	next := "new-unified-master-password"
	if err := m.ChangePassword("wrong-current", next, next); err == nil {
		t.Fatal("changed without current password")
	}
	if _, _, err := m.verify(token); err != nil {
		t.Fatal("failed password check revoked session")
	}
	if err := m.ChangePassword(testPassword, next, next); err != nil {
		t.Fatal(err)
	}
	after, err := vault.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if before.MainKeyID == after.MainKeyID {
		t.Fatal("Main Key did not change")
	}
	if _, _, err := m.verify(token); err == nil {
		t.Fatal("password change left old sessions valid")
	}
	if _, _, err := m.login("admin", testPassword); err == nil {
		t.Fatal("old Admin password accepted")
	}
	newCookie, _, err := m.login("admin", next)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.verify(newCookie); err == nil {
		t.Fatal("automatic unlock restored a browser session from the previous run")
	}
	if _, _, err := reopened.login("admin", testPassword); err == nil {
		t.Fatal("auto unlock bypassed password verification")
	}
	if err := reopened.vault.(*sshconfig.Store).Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := configstore.OpenRepository(filepath.Dir(m.path), m.providers...)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if data, err := repo.Read("encrypted-host"); err != nil || string(data) != testHost {
		t.Fatalf("password change lost host: %v", err)
	}
}

func TestManagedMissingMarkerDoesNotReopenSetup(t *testing.T) {
	m, _, _ := configuredManaged(t)
	if err := os.Remove(m.path); err != nil {
		t.Fatal(err)
	}
	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Configured() {
		t.Fatal("missing marker reopened setup")
	}
	if err := reopened.setup("replacement", testPassword); err == nil {
		t.Fatal("overwrote encrypted Admin")
	}
}

func TestManagedEncryptionDoesNotConvertPlaintextProfiles(t *testing.T) {
	m := openManaged(t)
	vault := m.vault.(*sshconfig.Store)
	if err := vault.Save("plain-host", []byte(testHost)); err != nil {
		t.Fatal(err)
	}
	if err := m.setup("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	status, err := vault.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Initialized || status.EncryptNew {
		t.Fatal("auth initialization changed host-encryption preference")
	}
	if mode, err := vault.StorageMode("plain-host"); err != nil || mode != sshconfig.StoragePlaintext {
		t.Fatalf("converted plaintext host: %s %v", mode, err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(m.path), "ssh_configs", "plain-host", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != testHost {
		t.Fatal("plaintext profile changed")
	}
}

func TestManagedRememberUnlockAfterProviderLoss(t *testing.T) {
	m, original, _ := configuredManaged(t)
	dir := filepath.Dir(m.path)
	if err := m.vault.(*sshconfig.Store).Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_REBOUND_KEK", "")
	provider := configstore.NewEnvProvider("TERMCP_TEST_REBOUND_KEK")
	vault, err := sshconfig.OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vault.Close() })
	locked, err := OpenManaged(dir, vault, provider)
	if err != nil {
		t.Fatal(err)
	}
	if !locked.NeedsUnlock() {
		t.Fatal("missing provider did not lock authentication")
	}
	t.Setenv("TERMCP_TEST_REBOUND_KEK", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32)))
	if _, _, err := locked.loginWithOptions("admin", testPassword, "", true); err != nil {
		t.Fatal(err)
	}
	if err := vault.Close(); err != nil {
		t.Fatal(err)
	}
	vault, err = sshconfig.OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := OpenManaged(dir, vault, provider)
	if err != nil {
		t.Fatal(err)
	}
	if restored.NeedsUnlock() {
		t.Fatal("rebound provider did not restore automatic unlock")
	}
	if _, _, err := restored.verify(original); err == nil {
		t.Fatal("provider unlock restored a browser session from the previous run")
	}
	if _, _, err := restored.login("admin", "incorrect-password"); err == nil {
		t.Fatal("provider availability bypassed Admin password")
	}
}

func TestManagedHTTPChangesUnifiedPassword(t *testing.T) {
	m, token, session := configuredManaged(t)
	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	body := `{"current_password":"` + testPassword + `","new_password":"replacement-master-password","confirmation":"replacement-master-password"}`
	response := managedRequest(t, handler, "POST", "/auth/password", body, token, session.CSRF)
	if response.Code != 200 {
		t.Fatalf("password endpoint: %d %s", response.Code, response.Body.String())
	}
	if _, _, err := m.verify(token); err == nil {
		t.Fatal("old cookie survived password change")
	}
	if _, _, err := m.login("admin", "replacement-master-password"); err != nil {
		t.Fatal(err)
	}
}
