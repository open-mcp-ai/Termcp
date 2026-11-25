package configstore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeKeyFile writes a synthetic 32-byte KEK to a per-test file and returns its
// path. It stands in for a container's mounted secret.
func writeKeyFile(t *testing.T, value byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kek")
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func jsonUnmarshal(data []byte, dst any) error { return json.Unmarshal(data, dst) }

func secondPhrase(t *testing.T) string { return testPhrase(t) }

/* The keychain holds several keys, each with its own mnemonic. None of them is
 * special: with no index there is no owner to move or to protect from deletion.
 *
 * Mutation check: make Keys() report a key as plugged when it is not and the
 * first assertion fails.
 */
func TestKeychainHoldsSeveralIndependentKeys(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x91))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	keys, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].Plugged || !keys[0].Auto {
		t.Fatalf("initial keychain = %+v; want one plugged, machine-bound key", keys)
	}
	first := keys[0].ID

	added, err := repo.CreateKey(testPhrase(t), "", NewEnvProvider(""))
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	if added.ID == first || !added.Plugged {
		t.Fatalf("added key = %+v; want a plugged key distinct from %s", added, first)
	}
	if keys, err = repo.Keys(); err != nil || len(keys) != 2 {
		t.Fatalf("keychain after add = %+v, %v", keys, err)
	}

	var newID ID
	if err := newID.UnmarshalText([]byte(added.ID)); err != nil {
		t.Fatal(err)
	}
	/* Any key can be deleted while it seals nothing: there is no index whose
	   ownership has to be handed over first. */
	if err := repo.RemoveKey(newID, false); !errors.Is(err, ErrBackupUnconfirmed) {
		t.Fatalf("removing without a backup confirmation = %v; want ErrBackupUnconfirmed", err)
	}

	var oldID ID
	if err := oldID.UnmarshalText([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveKey(oldID, true); err != nil {
		t.Fatalf("removing the first key: %v", err)
	}
	keys, err = repo.Keys()
	if err != nil || len(keys) != 1 || keys[0].ID != added.ID {
		t.Fatalf("after removing the first key: %+v, %v", keys, err)
	}
	/* The last key with nothing sealed by it now means "stop encrypting" rather
	   than "lose data", so it is allowed and turns the store off. */
	if err := repo.RemoveKey(newID, true); err != nil {
		t.Fatalf("removing the last unused key: %v", err)
	}
	if !repo.KeyringDetached() {
		t.Fatal("encryption survived removing the last key")
	}
	if keys, err := repo.Keys(); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("keychain after the last key = %+v, %v; want ErrNotInitialized", keys, err)
	}
}


/* A key that seals hosts cannot be removed: those hosts would become unreadable
 * with nothing to warn about it afterwards.
 *
 * Mutation check: drop the usage guard and this removal succeeds, stranding the
 * profile.
 */
func TestRemovingAKeyInUseIsRefused(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x92))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	b, err := repo.CreateKey(testPhrase(t), "", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	var bID ID
	if err := bID.UnmarshalText([]byte(b.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWithKey("beta", []byte("host = \"b.example\"\n"), bID); err != nil {
		t.Fatal(err)
	}
	keys, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.ID == b.ID && key.Hosts != 1 {
			t.Fatalf("hosts for the sealing key = %d; want 1", key.Hosts)
		}
	}
	if err := repo.RemoveKey(bID, true); !errors.Is(err, ErrKeyInUse) {
		t.Fatalf("removing a key in use = %v; want ErrKeyInUse", err)
	}
	if data, err := repo.Read("beta"); err != nil || len(data) == 0 {
		t.Fatalf("refused removal damaged the profile: %v", err)
	}
}

/* Plugging and unplugging are the first valve, and their state is in the file.
 * Unplugging a key makes its hosts refuse to open; plugging it back restores them
 * without touching any ciphertext.
 */
func TestPlugAndUnplugKeepTheKeychainFileAgreeing(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x93))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	keys, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	var id ID
	if err := id.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save("alpha", []byte("host = \"a.example\"\n")); err != nil {
		t.Fatal(err)
	}
	if err := repo.UnplugKey(id); err != nil {
		t.Fatalf("unplugging: %v", err)
	}
	if keys, err := repo.Keys(); err != nil || keys[0].Plugged {
		t.Fatalf("key after unplug = %+v, %v", keys, err)
	}
	if _, err := repo.Read("alpha"); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("reading with the key unplugged = %v; want ErrKeyNotLoaded", err)
	}
	if err := repo.PlugKey(id, NewEnvProvider("")); err != nil {
		t.Fatalf("plugging back in: %v", err)
	}
	if data, err := repo.Read("alpha"); err != nil || len(data) == 0 {
		t.Fatalf("reading after plugging back in: %q, %v", data, err)
	}
	/* The file has to agree, or the next start disagrees with this one. */
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if status := reopened.Status(); status.LoadedKeys != 1 {
		t.Fatalf("reopened with %d keys; want the plugged one", status.LoadedKeys)
	}
}

/* The words are the only copy, so exporting them is the backup. It needs no
 * passphrase: this machine already opened the slot (§9.4).
 */
func TestExportMnemonicReturnsTheKeysWords(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x94))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	keys, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	var id ID
	if err := id.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	exported, err := repo.ExportMnemonic(id)
	if err != nil {
		t.Fatalf("exporting the mnemonic: %v", err)
	}
	if len(splitWords(exported)) != 24 {
		t.Fatalf("exported phrase has %d words; want 24", len(splitWords(exported)))
	}
	// The exported words must be the ones the key was made from: reimporting them
	// into a new store reproduces the same Main Key.
	normalized, err := NormalizeMnemonic(exported)
	if err != nil {
		t.Fatal(err)
	}
	exportedMain, err := deriveMainKey(repo.Status().StoreID, normalized)
	if err != nil {
		t.Fatal(err)
	}
	plugged, _ := repo.snap().keyring.key(id)
	if plugged != exportedMain {
		t.Fatal("the exported words do not reproduce the plugged Main Key")
	}
}

/* A keychain whose keys cannot be opened here is not damaged: it is waiting for the
 * words. Adding a key must not delete it, because it is the only thing that can
 * ever read the profiles it seals.
 *
 * Mutation check: treat every loadErr as damage (drop the keyring == nil guard in
 * CreateKey) and the original key is gone after this call.
 */
func TestAddingAKeyKeepsAnUnopenableKeychain(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x95))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := repo.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("initial keychain = %+v, %v", keys, err)
	}
	var id ID
	if err := id.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddUnlockTo(id, NewFileProvider(writeKeyFile(t, 0x96))); err != nil {
		t.Fatalf("adding a file wrapper: %v", err)
	}
	if err := repo.RemoveUnlockFrom(id, "env"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	/* Reopen with nothing that can open the first key: the env KEK is gone and the
	   mounted file has not been named. */
	t.Setenv(EnvKEKVariable, "")
	t.Setenv(EnvKEKFileVariable, "")
	reopened, err := OpenRepository(dir)
	if err != nil {
		t.Fatalf("a file-only keychain must still open: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Loaded() {
		t.Fatal("nothing should be readable")
	}
	second, err := reopened.CreateKey(testPhrase(t), "", NewFileProvider(writeKeyFile(t, 0x99)))
	if err != nil {
		t.Fatalf("adding a key beside an unopenable one: %v", err)
	}
	if second.ID == "" {
		t.Fatal("the added key has no id")
	}
	keys, err = reopened.Keys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keychain after add = %+v; the unopenable key must survive", keys)
	}
	if _, err := reopened.ExportMnemonic(id); err == nil {
		t.Fatal("exporting an unopenable key succeeded")
	}
}

/* A keychain that is readable but unopenable must still start. The index cannot be
 * written without a key, and "cannot write the index" must not become "this store
 * is broken" — that turned every plaintext profile into an unavailable one.
 *
 * Mutation check: fail the open when the index cannot be committed and
 * OpenRepository returns an error here instead of a usable repository.
 */
func TestUnopenableKeychainWithoutAnIndexStillStarts(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x97))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := repo.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("initial keychain = %+v, %v", keys, err)
	}
	var id ID
	if err := id.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	keyFilePath := writeKeyFile(t, 0x98)
	if err := repo.AddUnlockTo(id, NewFileProvider(keyFilePath)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveUnlockFrom(id, "env"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKEKVariable, "")
	t.Setenv(EnvKEKFileVariable, "")
	reopened, err := OpenRepository(dir)
	if err != nil {
		t.Fatalf("a keychain with no index and no openable key must still open: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if status := reopened.Status(); status.Closed {
		t.Fatalf("reopened repository = %+v", status)
	}
	/* Plugging the key in writes the index from the profile directories. */
	if err := reopened.PlugKey(id, NewFileProvider(keyFilePath)); err != nil {
		t.Fatalf("plugging the key: %v", err)
	}
	if !reopened.Loaded() {
		t.Fatal("plugging the only key did not make the store readable")
	}
	if keys, err := reopened.Keys(); err != nil || len(keys) != 1 || !keys[0].Plugged {
		t.Fatalf("keychain after plugging = %+v, %v", keys, err)
	}
}
