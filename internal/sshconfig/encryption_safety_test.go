package sshconfig

import (
	"encoding/base64"
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

	store.importCiphertext = func(*configstore.Repository, []configstore.Item, configstore.SaveOptions) error {
		return errInjectedPublish
	}
	if _, err := store.EncryptProfiles([]string{"alpha", "beta"}, configstore.ID{}, ""); !errors.Is(err, errInjectedPublish) {
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

/* The panel's "lock this host" path proves the passphrase before it seals.
 *
 * The store can only check what a key already seals, and this is the layer that
 * supplies the passphrase, so the flag has to be set HERE: without it a typo sealed
 * the host to the typo and the request answered 200.
 *
 * Mutation check: drop VerifyPassphrase from the save options this function builds
 * and this fails with a nil error.
 */
func TestLockingAHostProvesTheKeysPassphrase(t *testing.T) {
	const passphrase = "synthetic passphrase for the lock test"
	store, repo, dir := newEncryptingStore(t)
	writePlaintextProfile(t, store, "first", safetyProfile)
	if _, err := store.EncryptProfiles([]string{"first"}, configstore.ID{}, ""); err != nil {
		t.Fatalf("sealing the first host: %v", err)
	}
	keys, err := repo.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keychain = %+v, %v", keys, err)
	}
	var id configstore.ID
	if err := id.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	// The key gets a passphrase and re-seals the host it already has, so "what this
	// key seals" exists for the next host to be checked against.
	if err := repo.ChangePassphrase(id, "", passphrase); err != nil {
		t.Fatalf("give the key a passphrase: %v", err)
	}

	writePlaintextProfile(t, store, "second", safetyProfile)
	if _, err := store.EncryptProfiles([]string{"second"}, id, "not the passphrase"); !errors.Is(err, configstore.ErrPassphraseWrong) {
		t.Fatalf("locking with a wrong passphrase = %v; want ErrPassphraseWrong", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ssh_configs", "second", "config.toml")); statErr != nil {
		t.Fatal("the refused lock removed the plaintext profile")
	}
	if _, err := store.EncryptProfiles([]string{"second"}, id, passphrase); err != nil {
		t.Fatalf("locking with the key's own passphrase: %v", err)
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

/* A keychain whose only ways in are phrases must let the process start — the
 * panel is where one is typed — and must refuse to write until a key is loaded,
 * rather than writing cleartext.
 *
 * Mutation check: treat "no key loaded" as "then plaintext is fine" and the save
 * below succeeds in cleartext.
 */
func TestUnloadedKeychainRefusesEncryptedWrite(t *testing.T) {
	dir := t.TempDir()
	provider := configstore.NewEnvProvider("")
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x42))
	repo, err := configstore.InitializeRepository(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEncryptNew(true); err != nil {
		t.Fatal(err)
	}
	keys, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	var id configstore.ID
	if err := id.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	// Drop the machine wrapper: from here only a phrase can load the key, which a
	// restart has nobody to type.
	if err := repo.RemoveUnlockFrom(id, "env"); !errors.Is(err, configstore.ErrLastWrapper) {
		t.Fatalf("removing the only wrapper = %v; want ErrLastWrapper", err)
	}
	/* A mounted KEK file is the other way in: machine-presentable, but this start
	   does not name it, so the key stays out of memory exactly as a phrase-only
	   keychain would. */
	keyFilePath := writeSyntheticKEK(t)
	if err := repo.AddUnlockTo(id, configstore.NewFileProvider(keyFilePath)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveUnlockFrom(id, "env"); err != nil {
		t.Fatalf("removing the machine wrapper once another way in exists: %v", err)
	}
	t.Setenv(configstore.EnvKEKFileVariable, "")
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("a phrase-only keychain must still open so the panel can load it: %v", err)
	}
	defer store.Close()
	status, err := store.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.KeysPlugged != 0 || status.Keys != 1 {
		t.Fatalf("status = %+v; want one key, none loaded", status)
	}

	if err := store.Save("prod", []byte(safetyProfile)); err == nil {
		t.Fatal("a store with no key loaded accepted an encrypted write")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ssh_configs", "prod", "config.toml")); statErr == nil {
		t.Fatal("the store with no key loaded wrote cleartext instead")
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

	/* Opening succeeds with the fault recorded: the daemon still has to serve the
	   plaintext profiles that share this data directory, and the Web UI needs a
	   running process to act on it. What must never happen is reading or writing
	   the encrypted profile. */
	reopened, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("a catalog without its keychain must open with the fault recorded, not fail: %v", err)
	}
	defer reopened.Close()
	if status, err := reopened.EncryptionStatus(); err != nil || status.Keys != 0 || status.ErrorCode == "" {
		t.Fatalf("status = %+v, err %v; want no keys and an error code", status, err)
	}
	if _, err := reopened.Load("prod"); err == nil {
		t.Fatal("an unreadable encrypted profile was loaded")
	}
}

// writeSyntheticKEK writes a synthetic 32-byte KEK to a per-test file and returns
// its path. It stands in for a container's mounted secret.
func writeSyntheticKEK(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kek")
	raw, err := base64.StdEncoding.DecodeString(environmentConfigKEK(0x43))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Re-keying moves an already-sealed host to another key.
// The plaintext is verified in memory first: moving needs the old key plugged AND
// the host activated. Moving to the SAME key reports already_encrypted.
func TestRekeyMovesHostBetweenKeysAndHonorsValves(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x42))
	dir := t.TempDir()
	env := configstore.NewEnvProvider("")
	repo, err := configstore.InitializeRepository(dir, env)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	store := NewHybridStore(dir, repo, true)
	defer store.Close()

	keyA, _, err := store.CreateKeyWithLabel("", "", "key-a", env)
	if err != nil {
		t.Fatalf("key A: %v", err)
	}
	keyB, _, err := store.CreateKeyWithLabel("", "", "key-b", env)
	if err != nil {
		t.Fatalf("key B: %v", err)
	}
	idA, err := parseKeyID(keyA.ID)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := parseKeyID(keyB.ID)
	if err != nil {
		t.Fatal(err)
	}

	writePlaintextProfile(t, store, "node-1", safetyProfile)

	/* 1. Seal under Key A. */
	resA, err := store.EncryptProfiles([]string{"node-1"}, idA, "")
	if err != nil || len(resA) != 1 || !resA[0].OK {
		t.Fatalf("encrypt A: %+v, %v", resA, err)
	}
	stA, err := repo.HostState("node-1")
	if err != nil || stA.KeyID != idA {
		t.Fatalf("host should be under key A (%s), got %+v, %v", idA, stA, err)
	}

	/* 2. Re-key to Key B: succeeds while key A is plugged and host active. */
	resB, err := store.EncryptProfiles([]string{"node-1"}, idB, "")
	if err != nil || len(resB) != 1 || !resB[0].OK {
		t.Fatalf("rekey to B: %+v, %v", resB, err)
	}
	stB, err := repo.HostState("node-1")
	if err != nil || stB.KeyID != idB {
		t.Fatalf("host should be under key B (%s), got %+v, %v", idB, stB, err)
	}
	readBack, err := store.ReadRaw("node-1")
	if err != nil || string(readBack) != safetyProfile {
		t.Fatalf("read back: %q, %v", string(readBack), err)
	}

	/* 3. Re-key to the SAME key: already_encrypted no-op. */
	resSame, err := store.EncryptProfiles([]string{"node-1"}, idB, "")
	if err != nil || len(resSame) != 1 || !resSame[0].OK || resSame[0].Code != "already_encrypted" {
		t.Fatalf("rekey same: %+v, %v", resSame, err)
	}

	/* 4. Deactivated host refuses to re-key: valve 2 is shut. */
	if err := store.Deactivate([]string{"node-1"}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	resDeact, err := store.EncryptProfiles([]string{"node-1"}, idA, "")
	if err != nil || len(resDeact) != 1 || resDeact[0].OK || resDeact[0].Code != "not_activated" {
		t.Fatalf("rekey deactivated: want not_activated, got %+v, %v", resDeact, err)
	}

	/* Reactivate node-1 under key B. */
	if actRes, err := store.Activate([]string{"node-1"}, nil); err != nil || actRes["node-1"] != nil {
		t.Fatalf("reactivate: %v, %v", actRes, err)
	}

	/* 5. Unplugged key refuses to re-key: valve 1 is shut. */
	if err := store.UnplugKey(idB); err != nil {
		t.Fatalf("unplug B: %v", err)
	}
	resUnplug, err := store.EncryptProfiles([]string{"node-1"}, idA, "")
	if err != nil || len(resUnplug) != 1 || resUnplug[0].OK || resUnplug[0].Code != "key_not_loaded" {
		t.Fatalf("rekey unplugged: want key_not_loaded, got %+v, %v", resUnplug, err)
	}
}
