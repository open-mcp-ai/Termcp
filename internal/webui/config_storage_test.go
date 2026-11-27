package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func newKeychainServer(t *testing.T, kek byte) (*sshconfig.Store, string, *http.ServeMux) {
	t.Helper()
	t.Setenv(configstore.EnvKEKVariable, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{kek}, 32)))
	dir := t.TempDir()
	store := sshconfig.NewStore(dir)
	t.Cleanup(func() { _ = store.Close() })
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)
	return store, dir, mux
}

func keychainCall(t *testing.T, mux *http.ServeMux, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s = %d %s; want %d", method, path, rec.Code, rec.Body.String(), want)
	}
	if strings.Contains(path, "/api/keychain") && rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s is cacheable; key material must not be", method, path)
	}
	return rec
}

type keychainBody struct {
	Keys         []configstore.KeyInfo `json:"keys"`
	CatalogKeyID string                `json:"catalog_key_id"`
	Mnemonic     string                `json:"mnemonic"`
}

func decodeKeychain(t *testing.T, rec *httptest.ResponseRecorder) keychainBody {
	t.Helper()
	var body keychainBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode keychain body: %v", err)
	}
	return body
}

/* The keychain panel's whole backend: an empty store lists nothing, the first key
 * creates the keychain, the phrase comes back exactly once, and the four actions
 * are per key: plug, unplug, export, remove.
 */
func TestKeychainHTTPCreateListPlugExportRemove(t *testing.T) {
	store, _, mux := newKeychainServer(t, 0x61)
	empty := decodeKeychain(t, keychainCall(t, mux, http.MethodGet, "/api/keychain", "", http.StatusOK))
	if len(empty.Keys) != 0 || empty.CatalogKeyID != "" {
		t.Fatalf("empty keychain = %+v", empty)
	}

	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\nport = 2222\npassword = \"placeholder\"\n")
	keychainCall(t, mux, http.MethodPut, "/api/connections/alpha", string(profile), http.StatusNoContent)

	first := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-1"}`, http.StatusOK))
	second := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-2"}`, http.StatusOK))
	var other string
	for _, key := range second.Keys {
		if key.ID != first.Keys[0].ID {
			other = key.ID
		}
	}
	if other == "" {
		t.Fatalf("second key not listed: %+v", second.Keys)
	}

	keychainCall(t, mux, http.MethodPost, "/api/config-storage/encrypt",
		`{"names":["alpha"],"key":"`+other+`"}`, http.StatusOK)
	if mode, err := store.StorageMode("alpha"); err != nil || mode != sshconfig.StorageEncrypted {
		t.Fatalf("alpha mode = %q, %v", mode, err)
	}
	// The human half of the guarantee: that key now seals a host, so removing it
	// would strand the profile.
	conflict := keychainCall(t, mux, http.MethodDelete, "/api/keychain/"+other, `{"backup_confirmed":true}`, http.StatusConflict)
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(conflict.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "key_in_use" {
		t.Fatalf("removal code = %q; want key_in_use", body.Code)
	}
	if _, err := store.ReadRaw("alpha"); err != nil {
		t.Fatalf("refused removal damaged the host: %v", err)
	}

	// The connection form's own route: ?key= seals the profile with that key.
	keychainCall(t, mux, http.MethodPut, "/api/connections/beta?key="+other, string(profile), http.StatusNoContent)
	if mode, err := store.StorageMode("beta"); err != nil || mode != sshconfig.StorageEncrypted {
		t.Fatalf("beta mode = %q, %v", mode, err)
	}
	if _, err := store.ReadRaw("beta"); err != nil {
		t.Fatalf("profile saved with an explicit key is unreadable: %v", err)
	}
}

/* Unplugging and plugging are one key's business: the host sealed by the unplugged
 * key reports which key it needs, and the other host keeps working.
 */
func TestKeychainHTTPUnplugAndPlugOneKey(t *testing.T) {
	store, _, mux := newKeychainServer(t, 0x63)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	keychainCall(t, mux, http.MethodPut, "/api/connections/alpha", string(profile), http.StatusNoContent)
	keychainCall(t, mux, http.MethodPut, "/api/connections/beta", string(profile), http.StatusNoContent)

	first := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-3"}`, http.StatusOK))
	second := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-4"}`, http.StatusOK))
	var other string
	for _, key := range second.Keys {
		if key.ID != first.Keys[0].ID {
			other = key.ID
		}
	}
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/encrypt", `{"names":["beta"],"key":"`+other+`"}`, http.StatusOK)

	off := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain/"+other+"/unplug", "", http.StatusOK))
	for _, key := range off.Keys {
		if key.ID == other && key.Plugged {
			t.Fatalf("the unplugged key still reports plugged: %+v", key)
		}
	}
	if _, err := store.ReadRaw("beta"); !errors.Is(err, configstore.ErrKeyNotLoaded) {
		t.Fatalf("reading a host whose key is unplugged = %v; want ErrKeyNotLoaded", err)
	}
	if _, err := store.ReadRaw("alpha"); err != nil {
		t.Fatalf("unplugging one key broke another key's host: %v", err)
	}

	keychainCall(t, mux, http.MethodPost, "/api/keychain/"+other+"/plug", "", http.StatusOK)
	if _, err := store.ReadRaw("beta"); err != nil {
		t.Fatalf("host after plugging its key back in: %v", err)
	}
}

/* The old model's endpoints are gone, not aliased. A stale client must get a 404
 * rather than a half-working door.
 */
func TestRemovedEndpointsAreGone(t *testing.T) {
	_, _, mux := newKeychainServer(t, 0x64)
	for _, path := range []string{
		"/api/config-storage/initialize",
		"/api/config-storage/unlock",
		"/api/config-storage/recover",
		"/api/config-storage/lock",
		"/api/config-storage/destroy",
		"/api/config-storage/password",
		"/api/keychain/00000000000000000000000000000000/load",
		"/api/keychain/00000000000000000000000000000000/unload",
		"/api/keychain/00000000000000000000000000000000/phrase",
		"/api/keychain/00000000000000000000000000000000/auto",
		"/api/keychain/00000000000000000000000000000000/default",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("removed endpoint %s returned %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/config-storage/keys", "/api/config-storage/keys/mnemonic"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("removed endpoint %s returned %d", path, rec.Code)
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

/* Where each host is stored, and which part of the keychain is plugged in, are the
 * two things the panel reads from this route.
 */
func TestConfigStorageStatusReportsKeysAndHostModes(t *testing.T) {
	store, _, mux := newKeychainServer(t, 0x65)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.Save("plain", profile); err != nil {
		t.Fatal(err)
	}
	status := getConfigStorageStatus(t, mux)
	if status.Initialized || status.Keys != 0 {
		t.Fatalf("status before any key = %+v", status)
	}
	keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-5"}`, http.StatusOK)
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/encrypt", `{"names":["plain"]}`, http.StatusOK)

	status = getConfigStorageStatus(t, mux)
	if !status.Initialized || status.Keys != 1 || status.KeysPlugged != 1 {
		t.Fatalf("status after enabling = %+v", status)
	}
	modes := map[string]sshconfig.EncryptionHost{}
	for _, host := range status.Hosts {
		modes[host.Name] = host
	}
	if modes["plain"].Mode != sshconfig.StorageEncrypted {
		t.Fatalf("host mode = %q; want encrypted", modes["plain"].Mode)
	}
	// A host under a key without a passphrase is ACTIVE, and says it needs none.
	if !modes["plain"].Activated || modes["plain"].Passphrase {
		t.Fatalf("host facts = %+v; want activated and no passphrase", modes["plain"])
	}
}

/* A keychain file that does not parse is damage, and the status has to say so
 * rather than pretend the store is merely unplugged. What may happen NEXT depends
 * on whether anything is sealed by it.
 */
func TestDamagedKeychainIsNamedRatherThanReportedAsUnplugged(t *testing.T) {
	store, dir, mux := newKeychainServer(t, 0x67)
	keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-6"}`, http.StatusOK)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := dir + "/keys/config.keys"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("a damaged keychain prevented startup: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	h := &Handler{SSH: reopened}
	fresh := http.NewServeMux()
	h.Register(fresh)
	status := getConfigStorageStatus(t, fresh)
	if status.ErrorCode != "keyring_missing" || status.KeysPlugged != 0 {
		t.Fatalf("status with a damaged keychain = %+v", status)
	}
	/* Nothing is sealed by the damaged file, so adding a key is the way out. */
	keychainCall(t, fresh, http.MethodPost, "/api/keychain", `{"label":"key-7"}`, http.StatusOK)
	if status := getConfigStorageStatus(t, fresh); status.ErrorCode != "" || status.KeysPlugged != 1 {
		t.Fatalf("status after replacing a damaged keychain = %+v", status)
	}
}

/* A store with no key plugged in still has to open, list its hosts, and refuse to
 * write cleartext over ciphertext.
 */
func TestConfigStorageStatusSurvivesAnUnpluggedKeychain(t *testing.T) {
	store, dir, mux := newKeychainServer(t, 0x66)
	keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-8"}`, http.StatusOK)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("a keychain that needs plugging in prevented startup: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	h := &Handler{SSH: reopened}
	fresh := http.NewServeMux()
	h.Register(fresh)
	status := getConfigStorageStatus(t, fresh)
	if !status.Initialized || status.Keys != 1 || status.KeysPlugged != 0 {
		t.Fatalf("status with nothing plugged in = %+v", status)
	}
	rec := httptest.NewRecorder()
	fresh.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/connections/prod",
		bytes.NewBufferString("kind = \"remote\"\nhost = \"host.example\"\nuser = \"u\"\npassword = \"p\"\n")))
	if rec.Code == http.StatusNoContent {
		t.Fatal("a store with nothing plugged in wrote a profile")
	}
	if listed := decodeKeychain(t, keychainCall(t, fresh, http.MethodGet, "/api/keychain", "", http.StatusOK)); len(listed.Keys) != 1 {
		t.Fatalf("keychain list while nothing is plugged in = %+v", listed)
	}
}

/* A damaged keychain with a sealed profile still on disk must not be replaced,
 * because that profile could only ever be opened by the key this file holds.
 */
func TestDamagedKeychainWithSealedProfilesRefusesReplacement(t *testing.T) {
	store, dir, _ := newKeychainServer(t, 0x69)
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)
	keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-9"}`, http.StatusOK)
	profile := []byte("kind = \"remote\"\nhost = \"sealed.example\"\nuser = \"ops\"\npassword = \"synthetic\"\n")
	encrypt := true
	if err := store.SaveWithMode("sealed", profile, sshconfig.SaveMode{Encrypt: &encrypt}); err != nil {
		t.Fatalf("save sealed profile: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := dir + "/keys/config.keys"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("damaged keychain prevented startup: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	damaged := &Handler{SSH: reopened}
	fresh := http.NewServeMux()
	damaged.Register(fresh)
	rec := keychainCall(t, fresh, http.MethodPost, "/api/keychain", `{"label":"key-10"}`, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "keychain_seals_hosts") {
		t.Fatalf("replacing a keychain that seals a host = %s; want keychain_seals_hosts", rec.Body.String())
	}
}

/* A damaged keychain with nothing behind it must have a button, not just a status
 * line: the panel can report "broken" forever, but only this route lets the human
 * move on. It replaces what POST /api/keychain does implicitly, and it must refuse
 * the two cases where discarding is a loss rather than an escape.
 */
func TestDiscardKeychainIsTheExplicitExitForADamagedFile(t *testing.T) {
	store, dir, _ := newKeychainServer(t, 0x6b)
	h := &Handler{SSH: store}
	mux := http.NewServeMux()
	h.Register(mux)
	keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-11"}`, http.StatusOK)

	/* A healthy keychain is not thrown away by this route: deleting keys you can
	   still read is DELETE /api/keychain/{id}, with its own guards. */
	rec := keychainCall(t, mux, http.MethodPost, "/api/config-storage/discard-keychain", `{}`, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "keychain_readable") {
		t.Fatalf("discarding a readable keychain = %s; want keychain_readable", rec.Body.String())
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := dir + "/keys/config.keys"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("damaged keychain prevented startup: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	damaged := &Handler{SSH: reopened}
	fresh := http.NewServeMux()
	damaged.Register(fresh)

	keychainCall(t, fresh, http.MethodPost, "/api/config-storage/discard-keychain", `{}`, http.StatusOK)
	status := getConfigStorageStatus(t, fresh)
	if status.ErrorCode != "" || status.Initialized || status.Keys != 0 {
		t.Fatalf("status after discarding a damaged keychain = %+v", status)
	}
	/* And the panel must then be able to start over. */
	keychainCall(t, fresh, http.MethodPost, "/api/keychain", `{"label":"key-12"}`, http.StatusOK)
	if status := getConfigStorageStatus(t, fresh); status.ErrorCode != "" || !status.Initialized {
		t.Fatalf("status after starting over = %+v", status)
	}
}

/* The activation valve at the HTTP layer: a passphrase per key, one batch, and a
 * refusal that names the key rather than a host.
 */
func TestActivationHTTPTakesOnePassphrasePerKey(t *testing.T) {
	store, _, mux := newKeychainServer(t, 0x6a)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	keychainCall(t, mux, http.MethodPut, "/api/connections/alpha", string(profile), http.StatusNoContent)
	keychainCall(t, mux, http.MethodPut, "/api/connections/beta", string(profile), http.StatusNoContent)

	guarded := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain",
		`{"label":"guarded","passphrase":"synthetic passphrase for tests"}`, http.StatusOK))
	keyID := guarded.Keys[0].ID
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/encrypt",
		`{"names":["alpha","beta"],"key":"`+keyID+`","passphrase":"synthetic passphrase for tests"}`, http.StatusOK)

	// Both are active right after creation.
	status := getConfigStorageStatus(t, mux)
	for _, host := range status.Hosts {
		if host.Name != "internal" && !host.Activated {
			t.Fatalf("host %q = %+v; want activated after creation", host.Name, host)
		}
		if host.Name != "internal" && !host.Passphrase {
			t.Fatalf("host %q does not report that its key has a passphrase", host.Name)
		}
	}

	keychainCall(t, mux, http.MethodPost, "/api/config-storage/deactivate", `{"names":["alpha","beta"]}`, http.StatusOK)
	for _, name := range []string{"alpha", "beta"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/connections/"+name, nil))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "host_not_activated") {
			t.Fatalf("reading %s while inactive = %d %s; want 409 host_not_activated", name, rec.Code, rec.Body.String())
		}
	}

	// One request, one passphrase, two hosts.
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/activate",
		`{"names":["alpha","beta"],"passphrases":{"`+keyID+`":"synthetic passphrase for tests"}}`, http.StatusOK)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.ReadRaw(name); err != nil {
			t.Fatalf("reading %s after activating: %v", name, err)
		}
	}

	// A request that forgets the passphrase is refused, and says which key it needs.
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/deactivate", `{"names":["alpha","beta"]}`, http.StatusOK)
	rec := keychainCall(t, mux, http.MethodPost, "/api/config-storage/activate",
		`{"names":["alpha","beta"]}`, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "passphrase_required") {
		t.Fatalf("activating without the passphrase = %s; want passphrase_required", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), keyID[:8]) {
		t.Fatalf("the refusal does not name the key %s: %s", keyID[:8], rec.Body.String())
	}

	// A wrong passphrase refuses the batch and names the key, never a host.
	rec = keychainCall(t, mux, http.MethodPost, "/api/config-storage/activate",
		`{"names":["alpha","beta"],"passphrases":{"`+keyID+`":"not the passphrase"}}`, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "passphrase_incorrect") {
		t.Fatalf("activating with a wrong passphrase = %s; want passphrase_incorrect", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "alpha") || strings.Contains(rec.Body.String(), "beta") {
		t.Fatalf("a passphrase error named a host: %s", rec.Body.String())
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.ReadRaw(name); !errors.Is(err, configstore.ErrNotActivated) {
			t.Fatalf("a refused batch activated %s: %v", name, err)
		}
	}
}

/* Masking is a display-layer switch: it changes what the list serializes and
 * nothing else. §10.2
 */
func TestMaskingChangesSerializationOnly(t *testing.T) {
	store, _, mux := newKeychainServer(t, 0x6b)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	keychainCall(t, mux, http.MethodPut, "/api/connections/alpha", string(profile), http.StatusNoContent)

	read := func(reveal bool) connectionSummary {
		t.Helper()
		path := "/api/connections"
		if reveal {
			path += "?reveal=true"
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Connections []connectionSummary `json:"connections"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, entry := range body.Connections {
			if entry.Name == "alpha" {
				return entry
			}
		}
		t.Fatal("alpha is missing from the list")
		return connectionSummary{}
	}

	unmasked := read(false)
	if unmasked.Host != "host.example" || unmasked.User != "operator" {
		t.Fatalf("list before masking = %+v", unmasked)
	}
	keychainCall(t, mux, http.MethodPut, "/api/config-storage/mask", `{"masked":true}`, http.StatusOK)

	masked := read(false)
	if masked.Host != maskFixedWidth || masked.User != maskFixedWidth {
		t.Fatalf("masked list = %+v; want fixed-width dots", masked)
	}
	/* The port goes with them, and `masked` is what says so: a masked row that
	   still printed `:2222` would leak which machine this is through the one field
	   the mask did not cover. */
	if masked.Port != 0 || !masked.Masked {
		t.Fatalf("masked list kept the port or lost the flag: %+v", masked)
	}
	if masked.Storage != unmasked.Storage {
		t.Fatalf("masking changed more than the address: %+v", masked)
	}
	// Connectivity and validation are untouched: the profile still loads.
	if entry, err := store.Load("alpha"); err != nil || entry.Host != "host.example" {
		t.Fatalf("masking changed what the profile means: %+v, %v", entry, err)
	}
	revealed := read(true)
	if revealed.Host != "host.example" || revealed.User != "operator" {
		t.Fatalf("revealed list = %+v", revealed)
	}
}

/* The masking switch is a TOGGLE, and its state has to survive a round trip: on,
 * off, on. The bug this locks down was the state never reaching the client at
 * all — the file was written, `PUT` answered `{"masked":true}`, and every
 * subsequent status read still said false, so the eye flipped back on the next
 * paint and the list never changed.
 */
func TestMaskingTogglesOnOffOnAndSurvivesAStatusRead(t *testing.T) {
	_, _, mux := newKeychainServer(t, 0x6d)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\nport = 2222\npassword = \"placeholder\"\n")
	keychainCall(t, mux, http.MethodPut, "/api/connections/alpha", string(profile), http.StatusNoContent)

	masked := func() bool { return getConfigStorageStatus(t, mux).Masked }

	if masked() {
		t.Fatal("masking should start off")
	}
	for i, want := range []bool{true, false, true} {
		keychainCall(t, mux, http.MethodPut, "/api/config-storage/mask",
			fmt.Sprintf(`{"masked":%t}`, want), http.StatusOK)
		/* Read it back through the status route, which is what the panel paints
		   from — the response body of the PUT is not the source of truth. */
		if got := masked(); got != want {
			t.Fatalf("toggle %d: status reports masked=%t, want %t", i+1, got, want)
		}
	}
}

/* A store with NO keychain still has a mask. Hiding addresses is a property of
 * what the page prints, not of whether encryption was ever enabled — and reading
 * it only out of the repository made the switch look dead on a fresh install,
 * which is exactly where a new user meets it first.
 */
func TestMaskingWorksBeforeAnyKeychainExists(t *testing.T) {
	_, _, mux := newKeychainServer(t, 0x6e)

	if getConfigStorageStatus(t, mux).Masked {
		t.Fatal("masking should start off")
	}
	keychainCall(t, mux, http.MethodPut, "/api/config-storage/mask", `{"masked":true}`, http.StatusOK)
	if !getConfigStorageStatus(t, mux).Masked {
		t.Fatal("masking was accepted but the status still reports it off")
	}
	keychainCall(t, mux, http.MethodPut, "/api/config-storage/mask", `{"masked":false}`, http.StatusOK)
	if getConfigStorageStatus(t, mux).Masked {
		t.Fatal("masking did not turn back off")
	}
}

/* Turning the valves is human-only: an Agent holding the MCP token must be refused
 * with 403 (§7.2, §10.3).
 */
func TestKeychainValvesRefuseAgents(t *testing.T) {
	_, _, mux := newKeychainServer(t, 0x6c)
	keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"key-13"}`, http.StatusOK)
	keys := decodeKeychain(t, keychainCall(t, mux, http.MethodGet, "/api/keychain", "", http.StatusOK))
	id := keys.Keys[0].ID
	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/keychain", `{"label":"key-14"}`},
		{http.MethodPost, "/api/keychain/" + id + "/plug", ""},
		{http.MethodPost, "/api/keychain/" + id + "/unplug", ""},
		{http.MethodGet, "/api/keychain/" + id + "/mnemonic", ""},
		{http.MethodDelete, "/api/keychain/" + id, `{"backup_confirmed":true}`},
		{http.MethodPost, "/api/config-storage/activate", `{"names":[]}`},
		{http.MethodPost, "/api/config-storage/deactivate", `{"names":[]}`},
	} {
		req := httptest.NewRequest(request.method, request.path, bytes.NewBufferString(request.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Termcp-Role", "agent")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as an agent = %d %s; want 403", request.method, request.path, rec.Code, rec.Body.String())
		}
	}
}

/* Changing a passphrase over HTTP: the key keeps its id and its address, the hosts
 * keep their names and their plaintext, and the OLD passphrase stops working while
 * the new one opens every host the key seals.
 *
 * The route exists because rotating the key is the only other way to change a
 * passphrase, and it changes the words too — which is more than the question asked.
 */
func TestChangingAPassphraseOverHTTPKeepsTheKey(t *testing.T) {
	const old, next = "synthetic passphrase for tests", "another synthetic passphrase"
	store, _, mux := newKeychainServer(t, 0x6b)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	keychainCall(t, mux, http.MethodPut, "/api/connections/alpha", string(profile), http.StatusNoContent)
	keychainCall(t, mux, http.MethodPut, "/api/connections/beta", string(profile), http.StatusNoContent)

	guarded := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain",
		`{"passphrase":"`+old+`","label":"work"}`, http.StatusOK))
	keyID := guarded.Keys[0].ID
	address := guarded.Keys[0].PublicKey
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/encrypt",
		`{"names":["alpha","beta"],"key":"`+keyID+`","passphrase":"`+old+`"}`, http.StatusOK)

	// A wrong current passphrase is refused before anything is written.
	wrong := keychainCall(t, mux, http.MethodPost, "/api/keychain/"+keyID+"/passphrase",
		`{"old":"not the passphrase","new":"`+next+`"}`, http.StatusBadRequest)
	if !strings.Contains(wrong.Body.String(), "passphrase_incorrect") {
		t.Fatalf("a wrong current passphrase = %s; want a passphrase error", wrong.Body.String())
	}

	changed := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain/"+keyID+"/passphrase",
		`{"old":"`+old+`","new":"`+next+`"}`, http.StatusOK))
	if len(changed.Keys) != 1 {
		t.Fatalf("keychain = %+v; want the one key", changed.Keys)
	}
	if changed.Keys[0].ID != keyID || changed.Keys[0].PublicKey != address {
		t.Fatalf("the key changed identity: %+v", changed.Keys[0])
	}
	if !changed.Keys[0].Passphrase {
		t.Fatal("the key lost its passphrase flag")
	}

	// Both hosts still read, under the new passphrase only.
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/deactivate", `{"names":["alpha","beta"]}`, http.StatusOK)
	stale := keychainCall(t, mux, http.MethodPost, "/api/config-storage/activate",
		`{"names":["alpha","beta"],"passphrases":{"`+keyID+`":"`+old+`"}}`, http.StatusBadRequest)
	if !strings.Contains(stale.Body.String(), "passphrase_incorrect") {
		t.Fatalf("the old passphrase still activates the hosts: %s", stale.Body.String())
	}
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/activate",
		`{"names":["alpha","beta"],"passphrases":{"`+keyID+`":"`+next+`"}}`, http.StatusOK)
	for _, name := range []string{"alpha", "beta"} {
		got, err := store.ReadRaw(name)
		if err != nil {
			t.Fatalf("reading %s after the change: %v", name, err)
		}
		if string(got) != string(profile) {
			t.Errorf("%s = %q, want the profile back", name, got)
		}
	}
}

/* A key without a name is refused at the route, with a code the page can turn into
 * a sentence — not a 500, and not a silently-created nameless key.
 *
 * Mutation check: make NormalizeKeyLabel accept an empty name and this fails on the
 * first case.
 */
func TestCreatingAKeyWithoutANameIsRefused(t *testing.T) {
	_, _, mux := newKeychainServer(t, 0x7d)
	for _, body := range []string{`{}`, `{"label":""}`, `{"label":"   "}`} {
		rec := keychainCall(t, mux, http.MethodPost, "/api/keychain", body, http.StatusBadRequest)
		if !strings.Contains(rec.Body.String(), "key_label_invalid") {
			t.Fatalf("%s = %s; want key_label_invalid", body, rec.Body.String())
		}
	}
	if listed := decodeKeychain(t, keychainCall(t, mux, http.MethodGet, "/api/keychain", "", http.StatusOK)); len(listed.Keys) != 0 {
		t.Fatalf("a refused name still created %d key(s)", len(listed.Keys))
	}
	// And the name is required on rename too, so a key cannot lose its name later.
	created := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain", `{"label":"work"}`, http.StatusOK))
	rec := keychainCall(t, mux, http.MethodPost, "/api/keychain/"+created.Keys[0].ID+"/rename", `{"label":""}`, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "key_label_invalid") {
		t.Fatalf("rename to blank = %s; want key_label_invalid", rec.Body.String())
	}
}

/* POST /api/keychain/{id}/plug with {"passphrase":"…"} is the HTTP side of the
 * one-step unlock: a wrong passphrase rolls back the plug (400 passphrase_incorrect)
 * and the right one makes every sealed host readable in one request.
 */
func TestPluggingAKeyWithPassphraseOverHTTPOpensItsHosts(t *testing.T) {
	const pass = "synthetic passphrase for tests"
	store, _, mux := newKeychainServer(t, 0x7f)
	profile := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	keychainCall(t, mux, http.MethodPut, "/api/connections/alpha", string(profile), http.StatusNoContent)
	keychainCall(t, mux, http.MethodPut, "/api/connections/beta", string(profile), http.StatusNoContent)

	guarded := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain",
		`{"passphrase":"`+pass+`","label":"work"}`, http.StatusOK))
	keyID := guarded.Keys[0].ID
	keychainCall(t, mux, http.MethodPost, "/api/config-storage/encrypt",
		`{"names":["alpha","beta"],"key":"`+keyID+`","passphrase":"`+pass+`"}`, http.StatusOK)
	keychainCall(t, mux, http.MethodPost, "/api/keychain/"+keyID+"/unplug", "", http.StatusOK)

	wrong := keychainCall(t, mux, http.MethodPost, "/api/keychain/"+keyID+"/plug",
		`{"passphrase":"wrong"}`, http.StatusBadRequest)
	if !strings.Contains(wrong.Body.String(), "passphrase_incorrect") {
		t.Fatalf("wrong passphrase = %s; want passphrase_incorrect", wrong.Body.String())
	}
	listed := decodeKeychain(t, keychainCall(t, mux, http.MethodGet, "/api/keychain", "", http.StatusOK))
	if listed.Keys[0].Plugged {
		t.Fatal("a wrong passphrase still left the key plugged over HTTP")
	}

	ok := decodeKeychain(t, keychainCall(t, mux, http.MethodPost, "/api/keychain/"+keyID+"/plug",
		`{"passphrase":"`+pass+`"}`, http.StatusOK))
	if !ok.Keys[0].Plugged {
		t.Fatal("right passphrase left the key unplugged")
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.ReadRaw(name); err != nil {
			t.Fatalf("%s unread after one-step HTTP plug: %v", name, err)
		}
	}
}
