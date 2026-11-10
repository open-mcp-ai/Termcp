package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

// The two properties that decide whether this feature can lose a profile are
// asserted here by injecting the failure that matters, not by timing.

const safetyProfile = "kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n"

var errInjectedPublish = errors.New("injected: ciphertext publication failed")

func newEncryptingStore(t *testing.T) (*Store, *configstore.Repository, string) {
	t.Helper()
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x41))
	dir := t.TempDir()
	repo, err := configstore.InitializeRepository(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatalf("initialize repository: %v", err)
	}
	store := NewHybridStore(dir, repo, true)
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store, repo, dir
}

// writePlaintextProfile creates a profile on disk the way the plaintext layout
// stores one, without going through Save.
func writePlaintextProfile(t *testing.T, store *Store, name, body string) string {
	t.Helper()
	path := store.plaintextPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}
	return path
}

// Converting a selection publishes the whole batch before any source is
// removed. With the publish failing, every source must still be there.
func TestFailedConversionKeepsEveryPlaintextSource(t *testing.T) {
	store, _, _ := newEncryptingStore(t)
	first := writePlaintextProfile(t, store, "alpha", safetyProfile)
	second := writePlaintextProfile(t, store, "beta", safetyProfile)

	store.importCiphertext = func(*configstore.Repository, []configstore.Item) error {
		return errInjectedPublish
	}
	if _, err := store.EncryptProfiles([]string{"alpha", "beta"}); !errors.Is(err, errInjectedPublish) {
		t.Fatalf("EncryptProfiles = %v, want the injected publication failure", err)
	}

	for _, path := range []string{first, second} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("a failed conversion removed %s: %v", path, err)
		}
		if string(body) != safetyProfile {
			t.Fatalf("%s was rewritten by a failed conversion", path)
		}
	}
}

// Overwriting an encrypted profile that still has a plaintext copy left over
// from an interrupted conversion publishes the ciphertext first. With the
// publish failing, the leftover must survive.
func TestFailedOverwriteKeepsTheLeftoverPlaintextCopy(t *testing.T) {
	store, _, _ := newEncryptingStore(t)
	if err := store.Save("prod", []byte(safetyProfile)); err != nil {
		t.Fatalf("encrypted save: %v", err)
	}
	leftover := writePlaintextProfile(t, store, "prod", safetyProfile)

	store.writeCiphertext = func(*configstore.Repository, string, []byte) error {
		return errInjectedPublish
	}
	if err := store.Save("prod", []byte(safetyProfile+"port = 2222\n")); !errors.Is(err, errInjectedPublish) {
		t.Fatalf("Save = %v, want the injected publication failure", err)
	}

	if _, err := os.Stat(leftover); err != nil {
		t.Fatalf("the leftover plaintext was removed by a failed overwrite: %v", err)
	}
}

// A store that must encrypt but has no usable keyring refuses the write. Falling
// back to plaintext would put credentials in cleartext exactly when the operator
// asked for the opposite.
func TestEncryptingStoreWithoutKeyringRefusesToWrite(t *testing.T) {
	dir := t.TempDir()
	store := NewHybridStore(dir, nil, true)

	err := store.Save("prod", []byte(safetyProfile))
	if err == nil {
		t.Fatal("a store configured to encrypt saved without a keyring")
	}
	if !errors.Is(err, errEncryptionUnavailable) {
		t.Fatalf("save error = %v, want the keyring-unavailable refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ssh_configs", "prod", "config.toml")); statErr == nil {
		t.Fatal("the refused save still wrote a plaintext profile")
	}
	if names, listErr := store.List(); listErr != nil || len(names) != 1 {
		t.Fatalf("the refused save left a profile behind: %v (%v)", names, listErr)
	}
}

// A locked repository must let the process start — the settings page is where
// the operator enters the password — and must refuse to write until it is
// unlocked, rather than writing cleartext.
func TestLockedRepositoryRefusesEncryptedWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Initialize with a password, then reopen without one: that is the state a
	// restart leaves behind.
	repo, err := configstore.InitializeRepositoryWithPassword(dir, "a-long-password", "a-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEncryptNew(true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("a password-locked store must still open so the UI can unlock it: %v", err)
	}
	defer store.Close()
	status, err := store.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Locked {
		t.Fatal("the store reopened unlocked")
	}

	if err := store.Save("prod", []byte(safetyProfile)); err == nil {
		t.Fatal("a locked store accepted an encrypted write")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ssh_configs", "prod", "config.toml")); statErr == nil {
		t.Fatal("the locked store wrote cleartext instead")
	}
}

// A catalog whose keyring vanished must not be mistaken for a plaintext store:
// the daemon would then write cleartext over profiles the operator encrypted.
func TestCatalogWithoutKeyringFailsClosed(t *testing.T) {
	store, _, dir := newEncryptingStore(t)
	if err := store.Save("prod", []byte(safetyProfile)); err != nil {
		t.Fatalf("encrypted save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "keys", "config.keys")); err != nil {
		t.Fatalf("remove keyring: %v", err)
	}

	if _, err := OpenConfiguredStore(dir); err == nil {
		t.Fatal("opening a catalog without its keyring succeeded")
	}
}
