package auth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestLegacyEncryptedLayoutMigratesAccountTokensAndHostIdentity(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	credential := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"legacy client"}`)
	vault := m.vault.(*sshconfig.Store)
	if err := vault.SetNewProfileEncryption(true); err != nil {
		t.Fatal(err)
	}
	if err := vault.Save("saved-host", []byte(testHost)); err != nil {
		t.Fatal(err)
	}
	hostID, err := vault.Identity("saved-host")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(filepath.Dir(configstore.LocalStatePath(filepath.Dir(m.path))))
	// Materialize the real previous storage contract: full v1 envelopes alongside
	// each profile, a separate keyring/catalog/auth object, and no login verifier.
	var state struct {
		Keyring      json.RawMessage   `json:"keyring"`
		Heads        map[string]string `json:"heads"`
		Envelopes    map[string][]byte `json:"envelopes"`
		ProfilePaths map[string]string `json:"profile_paths"`
	}
	raw, err := os.ReadFile(configstore.LocalStatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	k, err := configstore.UnlockKeyring(state.Keyring, m.providers...)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Destroy()
	for key, header := range state.Envelopes {
		parts := strings.Split(key, "/")
		bodyPath := filepath.Join(dir, "local-state", "objects", parts[0], parts[1]+".enc")
		if rel := state.ProfilePaths[key]; rel != "" {
			bodyPath = filepath.Join(dir, filepath.FromSlash(rel))
		}
		body, err := os.ReadFile(bodyPath)
		if err != nil {
			t.Fatal(err)
		}
		complete := append(bytes.Clone(header), body[89:]...)
		target := filepath.Join(dir, "ssh_configs", "saved-host", parts[1]+".enc")
		if key == state.Heads["catalog"] {
			target = filepath.Join(dir, "ssh_configs", "store.enc")
			var identity configstore.Identity
			copy(identity.StoreID[:], header[13:29])
			copy(identity.ObjectID[:], header[29:45])
			copy(identity.GenerationID[:], header[45:61])
			identity.Kind = configstore.KindCatalog
			envelope, e := configstore.Parse(complete, identity)
			if e != nil {
				t.Fatal(e)
			}
			plaintext, e := k.Open(envelope)
			if e != nil {
				t.Fatal(e)
			}
			var doc map[string]any
			if e = json.Unmarshal(plaintext, &doc); e != nil {
				t.Fatal(e)
			}
			clear(plaintext)
			for _, p := range doc["profiles"].([]any) {
				delete(p.(map[string]any), "profile_identity")
			}
			plaintext, e = json.Marshal(doc)
			if e != nil {
				t.Fatal(e)
			}
			envelope, e = k.Seal(plaintext, identity)
			clear(plaintext)
			if e != nil {
				t.Fatal(e)
			}
			complete = envelope.Bytes()
		} else if key == state.Heads["authentication"] {
			target = filepath.Join(dir, "auth.enc")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(target, complete, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.MkdirAll(filepath.Join(dir, "keys"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "keys", "config.keys"), state.Keyring, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"version":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = vault.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(dir, "local-state")); err != nil {
		t.Fatal(err)
	}
	loaded := openManagedAt(t, dir)
	if !loaded.Configured() || loaded.credential != nil {
		t.Fatal("legacy account became an unconfigured/new account")
	}
	if _, _, err = loaded.login("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	if loaded.credential == nil || loaded.state.Admin.ID != m.state.Admin.ID {
		t.Fatal("account identity was replaced")
	}
	if _, _, err = loaded.verify(cookie); err == nil {
		t.Fatal("migration restored a prior-run browser cookie")
	}
	if got, err := loaded.vault.(*sshconfig.Store).Identity("saved-host"); err != nil || got != hostID {
		t.Fatal("host identity changed", err)
	}
	if _, ok := loaded.state.Tokens[credential.Credential.ID]; !ok {
		t.Fatal("MCP credentials lost")
	}
	if entry, err := loaded.vault.(*sshconfig.Store).Load("saved-host"); err != nil || entry.Host != "host.example" {
		t.Fatal("profile lost", err)
	}
	for _, path := range []string{"auth.json", "auth.enc", "keys/config.keys", "ssh_configs/store.enc"} {
		if _, err = os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("verified legacy file remains: %s", path)
		}
	}
}
