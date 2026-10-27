package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestConfigStorageHTTPEnablesEncryptsAndRestoresSelectedHosts(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32)))
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
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
	mux.ServeHTTP(set, httptest.NewRequest(
		http.MethodPut,
		"/api/config-storage",
		bytes.NewBufferString(`{"encrypt_new":true}`),
	))
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
