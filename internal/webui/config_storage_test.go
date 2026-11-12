package webui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// An empty body initializes a machine-bound keyring: nothing is typed, the
// provider protects the Main Key alone, and the store stays open across restarts
// with no human in the loop. This is the deployment's default path, and it is
// covered at the HTTP boundary because the empty password is what the Web UI
// sends when the operator leaves the password checkbox alone.
func TestConfigStorageInitializeMachineBound(t *testing.T) {
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	defer store.Close()
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config-storage/initialize", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("machine-bound initialize: %d %s", rec.Code, rec.Body.String())
	}
	var state sshconfig.EncryptionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Initialized {
		t.Fatalf("state = %+v, want initialized", state)
	}
	if state.PasswordConfigured {
		t.Fatal("machine-bound keyring reports a password; nothing was typed")
	}
	if state.Locked {
		t.Fatalf("state = %+v, want unlocked right after enabling", state)
	}
}

func TestConfigStorageHTTPEnablesEncryptsAndRestoresSelectedHosts(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32)))
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	defer store.Close()
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)
	defer store.Close()

	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	for _, name := range []string{"one", "two"} {
		put := httptest.NewRecorder()
		mux.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/api/connections/"+name, bytes.NewReader(profile)))
		if put.Code != http.StatusNoContent {
			t.Fatalf("seed %s: %d %s", name, put.Code, put.Body.String())
		}
	}

	status := getConfigStorageStatus(t, mux)
	if status.Initialized || status.EncryptNew {
		t.Fatalf("default status = %+v", status)
	}
	for _, host := range status.Hosts {
		if host.Name == "one" || host.Name == "two" {
			if host.Mode != sshconfig.StoragePlaintext {
				t.Fatalf("default host mode = %+v", host)
			}
		}
	}

	set := httptest.NewRecorder()
	initialBody, err := json.Marshal(map[string]string{"password": "synthetic first password", "confirmation": "synthetic first password"})
	if err != nil {
		t.Fatal(err)
	}
	initialize := httptest.NewRequest(http.MethodPost, "/api/config-storage/initialize", bytes.NewReader(initialBody))
	initialize.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(set, initialize)
	if set.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", set.Code, set.Body.String())
	}
	status = getConfigStorageStatus(t, mux)
	if !status.Initialized || !status.EncryptNew {
		t.Fatalf("enabled status = %+v", status)
	}

	put := httptest.NewRecorder()
	mux.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/api/connections/three", bytes.NewReader(profile)))
	if put.Code != http.StatusNoContent {
		t.Fatalf("new encrypted profile: %d %s", put.Code, put.Body.String())
	}
	if mode, _ := store.StorageMode("three"); mode != sshconfig.StorageEncrypted {
		t.Fatalf("new profile mode = %q", mode)
	}

	convert := httptest.NewRecorder()
	mux.ServeHTTP(convert, httptest.NewRequest(
		http.MethodPost,
		"/api/config-storage/encrypt",
		bytes.NewBufferString(`{"names":["one"]}`),
	))
	if convert.Code != http.StatusOK {
		t.Fatalf("encrypt selected: %d %s", convert.Code, convert.Body.String())
	}
	if mode, _ := store.StorageMode("one"); mode != sshconfig.StorageEncrypted {
		t.Fatalf("one mode = %q", mode)
	}
	if mode, _ := store.StorageMode("two"); mode != sshconfig.StoragePlaintext {
		t.Fatalf("unselected two mode = %q", mode)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "one", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("selected plaintext remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "two", "config.toml")); err != nil {
		t.Fatalf("unselected plaintext was removed: %v", err)
	}

	restore := httptest.NewRecorder()
	mux.ServeHTTP(restore, httptest.NewRequest(
		http.MethodPost,
		"/api/config-storage/restore",
		bytes.NewBufferString(`{"names":["three"]}`),
	))
	if restore.Code != http.StatusOK {
		t.Fatalf("restore selected: %d %s", restore.Code, restore.Body.String())
	}
	if mode, _ := store.StorageMode("three"); mode != sshconfig.StoragePlaintext {
		t.Fatalf("restored three mode = %q", mode)
	}
	if mode, _ := store.StorageMode("one"); mode != sshconfig.StorageEncrypted {
		t.Fatalf("unselected encrypted one mode = %q", mode)
	}
}

func TestConfigStorageHTTPChangesPasswordAndSurvivesRestart(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	t.Cleanup(func() { _ = store.Close() })
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)
	request := func(method, path, body string, expected int) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, req)
		if rec.Code != expected {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return rec
	}
	request(http.MethodPost, "/api/config-storage/password", `{}`, http.StatusConflict)
	if getConfigStorageStatus(t, mux).Initialized {
		t.Fatal("rotation initialized encryption without opting in")
	}
	profile := "kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n"
	request(http.MethodPut, "/api/connections/plain", profile, http.StatusNoContent)
	initialBody, err := json.Marshal(map[string]string{"password": "synthetic first password", "confirmation": "synthetic first password"})
	if err != nil {
		t.Fatal(err)
	}
	request(http.MethodPost, "/api/config-storage/initialize", string(initialBody), http.StatusOK)
	request(http.MethodPut, "/api/connections/encrypted", profile, http.StatusNoContent)
	before := getConfigStorageStatus(t, mux)
	request(http.MethodPost, "/api/config-storage/password", `{"unexpected":true}`, http.StatusBadRequest)
	if getConfigStorageStatus(t, mux).MainKeyID != before.MainKeyID {
		t.Fatal("invalid request changed the Main Key")
	}
	rec := request(http.MethodPost, "/api/config-storage/password", `{"current_password":"synthetic first password","new_password":"synthetic second password","confirmation":"synthetic second password"}`, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("key management response may be cached")
	}
	after := getConfigStorageStatus(t, mux)
	if len(after.MainKeyID) != 32 || before.MainKeyID == after.MainKeyID || after.RotationPending || !after.EncryptNew {
		t.Fatalf("unexpected rotation status: %+v", after)
	}
	if raw, err := store.ReadRaw("encrypted"); err != nil || string(raw) != profile {
		t.Fatalf("encrypted profile changed after rotation: %v", err)
	}
	plain, err := os.ReadFile(filepath.Join(dir, "ssh_configs", "plain", "config.toml"))
	if err != nil || string(plain) != profile {
		t.Fatalf("rotation modified the plaintext profile: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sshconfig.OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	h.SSH = reopened
	if restarted := getConfigStorageStatus(t, mux); restarted.MainKeyID != after.MainKeyID || restarted.RotationPending {
		t.Fatalf("restarted status = %+v", restarted)
	}
	request(http.MethodPost, "/api/config-storage/restore", `{"names":["encrypted"]}`, http.StatusOK)
}

func TestConfigStorageWSRetainsProfileModesAfterRotation(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x43}, 32)))
	store := sshconfig.NewStore(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.Save("plain", profile); err != nil {
		t.Fatal(err)
	}
	provider := configstore.NewEnvProvider("")
	if err := store.SetNewProfileEncryption(true, provider); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("encrypted", profile); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWithOptions("temporary", profile, true); err != nil {
		t.Fatal(err)
	}
	ws := &uiWS{
		h:   &Handler{SSH: store, Sessions: &session.Manager{}, NoInternal: true},
		ctx: context.Background(), send: make(chan []byte, 1),
	}
	for _, rotate := range []bool{false, true} {
		if rotate {
			if err := store.RotateMainKey(provider); err != nil {
				t.Fatal(err)
			}
		}
		ws.enqueueSessions()
		var data []byte
		select {
		case data = <-ws.send:
		default:
			t.Fatal("no connection frame was queued")
		}
		var frame struct {
			Connections []connectionSummary `json:"connections"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		modes := make(map[string]string)
		for _, conn := range frame.Connections {
			modes[conn.Name] = conn.Storage
			if conn.Temporary != (conn.Name == "temporary") {
				t.Fatalf("live frame lost temporary status: %+v", conn)
			}
		}
		for name, want := range map[string]string{
			"plain": sshconfig.StoragePlaintext, "encrypted": sshconfig.StorageEncrypted,
			"temporary": sshconfig.StorageTemporary,
		} {
			if modes[name] != want {
				t.Fatalf("live mode of %s = %q, want %q", name, modes[name], want)
			}
		}
	}
}

func getConfigStorageStatus(t *testing.T, mux *http.ServeMux) sshconfig.EncryptionStatus {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config-storage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var status sshconfig.EncryptionStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestConfigStorageInitializationAndProviderRecovery(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32)))
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	t.Cleanup(func() { _ = store.Close() })
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)
	request := func(method, path string, body any, expected int) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != expected {
			t.Fatalf("%s %s returned %d, wanted %d", method, path, rec.Code, expected)
		}
		return rec
	}
	for i := 0; i < 2; i++ {
		if getConfigStorageStatus(t, mux).Initialized {
			t.Fatal("opening the settings initialized a keyring")
		}
	}
	/* There is no global on/off switch any more: whether a profile is encrypted
	   is decided per profile in the connection form (see handlePutConnection's
	   ?encrypt=), and this endpoint existed only to flip that preference. The
	   route is gone, so an attempt to use it 404s — which is the honest answer
	   for a surface that no longer exists. */
	request(http.MethodPut, "/api/config-storage", map[string]bool{"encrypt_new": true}, http.StatusNotFound)
	if _, err := os.Stat(filepath.Join(dir, "keys", "config.keys")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the first enable request skipped explicit Main Key initialization")
	}
	main := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x57}, 32))
	request(http.MethodPost, "/api/config-storage/initialize", map[string]string{"password": "invalid", "confirmation": "invalid"}, http.StatusBadRequest)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.Save("plain", profile); err != nil {
		t.Fatal(err)
	}
	initialized := request(http.MethodPost, "/api/config-storage/initialize", map[string]string{"password": main, "confirmation": main}, http.StatusOK)
	if bytes.Contains(initialized.Body.Bytes(), []byte(main)) {
		t.Fatal("the initialization response exposed its Main Key")
	}
	if err := store.Save("encrypted", profile); err != nil {
		t.Fatal(err)
	}
	before := getConfigStorageStatus(t, mux)
	keyPath := filepath.Join(dir, "keys", "config.keys")
	keysBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(dir, "ssh_configs", "store.enc")
	catalogBefore, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configstore.EnvKEKVariable, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x45}, 32)))
	locked, err := sshconfig.OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	h.SSH = locked
	state := getConfigStorageStatus(t, mux)
	if !state.Initialized || !state.Locked || state.ErrorCode != "keyring_authentication_failed" || len(state.Hosts) != 0 {
		t.Fatalf("locked status = %+v", state)
	}
	// The encrypted host is locked and cannot be read.
	if _, err := locked.ReadRaw("encrypted"); !errors.Is(err, configstore.ErrLocked) {
		t.Fatalf("encrypted profile did not report locked: %v", err)
	}
	// Plaintext profile must load normally.
	if _, err := locked.ReadRaw("plain"); err != nil {
		t.Fatalf("plaintext profile failed on locked store: %v", err)
	}
	// The connections list succeeds (200), carrying the plaintext host and the
	// marked locked host, rather than dragging the whole inventory down.
	connsResp := request(http.MethodGet, "/api/connections", nil, http.StatusOK)
	var conns struct {
		Connections []struct {
			Name    string `json:"name"`
			Storage string `json:"storage"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(connsResp.Body.Bytes(), &conns); err != nil {
		t.Fatal(err)
	}
	hasPlain, hasEnc := false, false
	for _, c := range conns.Connections {
		if c.Name == "plain" && c.Storage == "plaintext" { hasPlain = true }
		if c.Name == "encrypted" && c.Storage == "encrypted" { hasEnc = true }
	}
	if !hasPlain || !hasEnc {
		t.Fatalf("connections list under lock = %+v; want plain (plaintext) and encrypted (encrypted)", conns.Connections)
	}
	request(http.MethodPut, "/api/config-storage", map[string]bool{"encrypt_new": false}, http.StatusNotFound)
	request(http.MethodPost, "/api/config-storage/recover", map[string]string{"password": "incorrect synthetic password"}, http.StatusBadRequest)
	keysWrong, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(keysWrong, keysBefore) {
		t.Fatal("wrong Main Key replaced the existing keyring")
	}
	response := request(http.MethodPost, "/api/config-storage/recover", map[string]string{"password": main}, http.StatusOK)
	if bytes.Contains(response.Body.Bytes(), []byte(main)) {
		t.Fatal("recovery returned a secret in its status response")
	}
	after := getConfigStorageStatus(t, mux)
	if after.Locked || !after.EncryptNew || after.MainKeyID != before.MainKeyID {
		t.Fatalf("recovered status = %+v", after)
	}
	for _, name := range []string{"plain", "encrypted"} {
		if raw, err := locked.ReadRaw(name); err != nil || !bytes.Equal(raw, profile) {
			t.Fatalf("profile %s was not recovered: %v", name, err)
		}
	}
	catalogAfter, err := os.ReadFile(catalogPath)
	if err != nil || !bytes.Equal(catalogBefore, catalogAfter) {
		t.Fatal("key-provider recovery changed the authenticated catalog")
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := sshconfig.OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if state, err := restarted.EncryptionStatus(); err != nil || state.Locked || !state.EncryptNew {
		t.Fatalf("status after restart = %+v, error %v", state, err)
	}
}

func TestConfigStorageMainKeyEndpointsRejectCrossOriginRequests(t *testing.T) {
	h := &Handler{SSH: sshconfig.NewStore(t.TempDir())}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, path := range []string{"initialize", "recover", "unlock", "password"} {
		req := httptest.NewRequest(http.MethodPost, "/api/config-storage/"+path, bytes.NewBufferString(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://foreign.example")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cross-origin %s response = %d", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/config-storage/initialize", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON key initialization response = %d", rec.Code)
	}
}

func TestConfigStorageDoesNotExposeBackupOrRandomRotation(t *testing.T) {
	h := &Handler{SSH: sshconfig.NewStore(t.TempDir())}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, path := range []string{"backup", "rotate"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config-storage/"+path, bytes.NewBufferString(`{}`)))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("removed %s endpoint returned %d", path, rec.Code)
		}
	}
}
