package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

/* The contract this file pins down is the deployment's whole reason for having a
 * system provider: a key bound to this machine must load itself after a restart
 * with nobody at the keyboard. A phrase cannot do that — it needs a human by
 * definition — so the machine-bound key is the default path, and a phrase is an
 * additional way to load the same key on another machine.
 *
 * Mutation check: opening the store with no provider leaves the key unloaded and
 * fails the restart half of this test.
 */
func TestMachineBoundKeyReopensWithoutAPhrase(t *testing.T) {
	system := configstore.SystemProvider()
	if !configstore.SystemAvailable() {
		t.Skipf("no native credential store on %s", runtime.GOOS)
	}
	dir := t.TempDir()
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateKey("", "", system); err != nil {
		t.Fatalf("machine-bound key: %v", err)
	}
	remote := []byte(`kind = "remote"
host = "host.example"
user = "operator"
password = "synthetic-disable-secret"
`)
	if err := store.Save("locked-host", remote); err != nil {
		t.Fatal(err)
	}
	if results, err := store.EncryptProfiles([]string{"locked-host"}, configstore.ID{}, ""); err != nil || !results[0].OK {
		t.Fatalf("encrypt: %+v, %v", results, err)
	}
	if mode, err := store.StorageMode("locked-host"); err != nil || mode != StorageEncrypted {
		t.Fatalf("mode = %q, err %v", mode, err)
	}
	state, err := store.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if state.KeysPlugged != 1 {
		t.Fatalf("keys loaded = %d; want the machine-bound key", state.KeysPlugged)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	/* Restart, with nothing typed. This is the daily case: the daemon is
	   re-spawned by its MCP client and the agent has to keep working. */
	reopened, err := OpenConfiguredStore(dir, configstore.SystemProvider())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := reopened.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if restarted.KeysPlugged != 1 {
		t.Fatalf("keys loaded after restart = %d; a machine-bound key must load itself", restarted.KeysPlugged)
	}
	entry, err := reopened.Load("locked-host")
	if err != nil {
		t.Fatalf("reading an encrypted host after restart: %v", err)
	}
	if entry.Host != "host.example" {
		t.Fatalf("host = %q", entry.Host)
	}
}

/* A copy of the data directory on another machine must not open by itself: that
 * is the protection the encryption exists for. Proven here by opening the same
 * directory with a provider whose key material is a different DPAPI user context
 * — simulated by an unrelated env provider, which cannot unwrap a dpapi slot. */
func TestMachineBoundKeyStaysShutWithoutItsProvider(t *testing.T) {
	if !configstore.SystemAvailable() {
		t.Skipf("no native credential store on %s", runtime.GOOS)
	}
	dir := t.TempDir()
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateKey("", "", configstore.SystemProvider()); err != nil {
		t.Fatalf("machine-bound key: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys", "config.keys")); err != nil {
		t.Fatalf("keychain file missing: %v", err)
	}
	foreign, err := OpenConfiguredStore(dir, configstore.NewEnvProvider("TERMCP_SYNTHETIC_ABSENT_KEK"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	state, err := foreign.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if state.KeysPlugged != 0 {
		t.Fatal("a key whose machine wrapper is absent loaded anyway")
	}
}

/* Deleting the last key with nothing sealed by it turns encryption off, and it
 * must leave a store that still works: the keychain file and the catalog index go
 * together, so no reader ever meets a catalog whose key is gone.
 *
 * Mutation check: remove only config.keys and the next Save reports
 * "repository is closed" — the state that used to answer 500.
 */
func TestRemovingTheLastKeyTurnsEncryptionOffCleanly(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x83))
	dir := t.TempDir()
	store, err := OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateKey("", "", configstore.NewEnvProvider("")); err != nil {
		t.Fatalf("first key: %v", err)
	}
	keys, err := store.KeychainStatus()
	if err != nil || len(keys.Keys) != 1 {
		t.Fatalf("keychain = %+v, %v", keys, err)
	}
	var id configstore.ID
	if err := id.UnmarshalText([]byte(keys.Keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveKey(id, true); err != nil {
		t.Fatalf("removing the only unused key: %v", err)
	}
	for _, path := range []string{"keys/config.keys", "ssh_configs/store.enc"} {
		if _, err := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(err) {
			t.Fatalf("%s survived turning encryption off", path)
		}
	}
	status, err := store.EncryptionStatus()
	if err != nil {
		t.Fatalf("status after turning encryption off: %v", err)
	}
	if status.Keys != 0 || status.EncryptNew {
		t.Fatalf("status = %+v; want no keys and no encryption", status)
	}
	raw := []byte(`kind = "remote"
host = "host.example"
user = "operator"
password = "synthetic-plain-secret"
`)
	if err := store.Save("plain", raw); err != nil {
		t.Fatalf("saving after encryption was turned off: %v", err)
	}
	if mode, err := store.StorageMode("plain"); err != nil || mode != StoragePlaintext {
		t.Fatalf("mode after turning encryption off = %q, err %v", mode, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatalf("reopening a store whose encryption was turned off: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if entry, err := reopened.Load("plain"); err != nil || entry.Host != "host.example" {
		t.Fatalf("plaintext profile after restart = %+v, %v", entry, err)
	}
}

/* An unreadable profile must still be deletable. When the key that made it is
 * gone, nothing can ever open those bytes, so removing them is the whole point of
 * the delete button — and it is exactly what the panel advises ("delete this
 * profile and create a new one").
 *
 * Mutation check: restore the old `repo == nil → errEncryptionUnavailable` branch
 * and this delete fails with "no key is loaded".
 */
func TestDeleteRemovesCiphertextWhoseKeyIsGone(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "ssh_configs", "tencentcloud")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(profileDir, "0123456789abcdef0123456789abcdef.enc")
	if err := os.WriteFile(generation, []byte("TCPCFG01synthetic-orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Delete("tencentcloud"); err != nil {
		t.Fatalf("deleting an unreadable profile: %v", err)
	}
	if _, err := os.Stat(generation); !os.IsNotExist(err) {
		t.Fatalf("ciphertext survived the delete: %v", err)
	}
	if _, err := os.Stat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("empty profile directory survived the delete: %v", err)
	}
}

/* The counterpart: while a key that might open the bytes exists but is not
 * loaded, the same shape must be REFUSED — loading it may well make the profile
 * readable.
 *
 * Mutation check: drop the !repo.Loaded() branch and the delete destroys a profile
 * that a load would have recovered.
 */
func TestDeleteRefusesCiphertextWhileNoKeyIsLoaded(t *testing.T) {
	dir := t.TempDir()
	provider := configstore.NewEnvProvider("")
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x85))
	store, err := OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateKey("", "", provider); err != nil {
		t.Fatalf("first key: %v", err)
	}
	remote := []byte(`kind = "remote"
host = "host.example"
user = "operator"
password = "synthetic-locked-secret"
`)
	if err := store.Save("prod", remote); err != nil {
		t.Fatal(err)
	}
	if results, err := store.EncryptProfiles([]string{"prod"}, configstore.ID{}, ""); err != nil || !results[0].OK {
		t.Fatalf("encrypt: %+v, %v", results, err)
	}
	keys, err := store.KeychainStatus()
	if err != nil {
		t.Fatal(err)
	}
	var id configstore.ID
	if err := id.UnmarshalText([]byte(keys.Keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	/* Only a mounted KEK file can open the key now, and this start does not name
	   it — the same shape as a keychain waiting for a person. */
	keyFilePath := writeSyntheticKEK(t)
	if err := store.AddUnlockTo(id, configstore.NewFileProvider(keyFilePath)); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveUnlockFrom(id, "env"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configstore.EnvKEKFileVariable, "")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Delete("prod"); !errors.Is(err, configstore.ErrKeyNotLoaded) {
		t.Fatalf("delete with no key loaded = %v; want ErrKeyNotLoaded", err)
	}
	if mode, err := reopened.StorageMode("prod"); err != nil || mode != StorageEncrypted {
		t.Fatalf("profile after refused delete = %q, err %v", mode, err)
	}
	// Plugging the key in makes the profile readable again, which is what the
	// refusal protected.
	if err := reopened.PlugKey(id, configstore.NewFileProvider(keyFilePath)); err != nil {
		t.Fatalf("plugging the key back in: %v", err)
	}
	if entry, err := reopened.Load("prod"); err != nil || entry.Host != "host.example" {
		t.Fatalf("profile after loading its key = %+v, %v", entry, err)
	}
}

/* An unlocked catalog only answers for the names it still lists, so a leftover
 * generation from a deleted key looks absent — and the delete used to return
 * success while removing nothing. Ask the disk as well.
 *
 * Mutation check: drop `|| onDisk` from the Delete entry condition and the files
 * survive a 204.
 */
func TestDeleteRemovesOrphanedCiphertextWhileAKeyIsLoaded(t *testing.T) {
	dir := t.TempDir()
	provider := configstore.NewEnvProvider("")
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x87))
	store, err := OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateKey("", "", provider); err != nil {
		t.Fatalf("first key: %v", err)
	}
	// A generation the catalog never listed: exactly what a deleted key leaves.
	profileDir := filepath.Join(dir, "ssh_configs", "tencentcloud")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(profileDir, "0123456789abcdef0123456789abcdef.enc")
	if err := os.WriteFile(generation, []byte("TCPCFG01synthetic-orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("tencentcloud"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(generation); !os.IsNotExist(err) {
		t.Fatalf("ciphertext survived a successful delete: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

/* The panel is opened precisely to deal with a broken profile, so one unreadable
 * name must not fail the whole status call — that turned the encryption screen
 * into a permanent "the operation failed".
 *
 * Mutation check: return the StorageMode error instead of an "unknown" host and
 * this call fails with "ssh config ... not found".
 */
func TestEncryptionStatusSurvivesAnUnreadableProfile(t *testing.T) {
	dir := t.TempDir()
	provider := configstore.NewEnvProvider("")
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x89))
	store, err := OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, _, err := store.CreateKey("", "", provider); err != nil {
		t.Fatalf("first key: %v", err)
	}
	profileDir := filepath.Join(dir, "ssh_configs", "tencentcloud")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "0123456789abcdef0123456789abcdef.enc"), []byte("TCPCFG01synthetic-orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := store.EncryptionStatus()
	if err != nil {
		t.Fatalf("status failed over one unreadable profile: %v", err)
	}
	found := ""
	for _, host := range status.Hosts {
		if host.Name == "tencentcloud" {
			found = host.Mode
		}
	}
	if found != StorageEncrypted {
		t.Fatalf("orphaned profile reported as %q; want %q", found, StorageEncrypted)
	}
}

/* After a destructive delete the profile still exists on disk but has no key.
 * Reading it must say that, not fall through to the plaintext read and return a
 * bare file-not-found — which the HTTP layer turns into 404 for a host that is
 * right there in the list.
 *
 * Mutation check: remove the orphan branch from ReadRaw and this returns an
 * *os.PathError instead of orphanedCiphertextError.
 */
func TestReadRawNamesTheDeletedKey(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "ssh_configs", "prod")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "0123456789abcdef0123456789abcdef.enc"), []byte("TCPCFG01synthetic-orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.ReadRaw("prod")
	if !IsOrphanedCiphertext(err) {
		t.Fatalf("ReadRaw = %v; want an orphaned-ciphertext error", err)
	}
}

/* The point of sharing is that two people never exchange a key: the sender seals
 * the profile to the recipient's public key, and only that store can open it.
 *
 * Mutation check: seal to the sender's own key instead and the second Open fails
 * with ErrDecrypt.
 */
func TestSharingSealsToTheRecipient(t *testing.T) {
	newStore := func(t *testing.T, kek byte) *Store {
		t.Helper()
		dir := t.TempDir()
		provider := configstore.NewEnvProvider("")
		t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(kek))
		store, err := OpenConfiguredStore(dir, provider)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if _, _, err := store.CreateKey("", "", provider); err != nil {
			t.Fatalf("first key: %v", err)
		}
		return store
	}
	sender := newStore(t, 0x91)
	recipient := newStore(t, 0x93)

	profile := []byte(`kind = "remote"
host = "shared.example"
user = "ops"
password = "synthetic-shared-secret"
`)
	if err := sender.Save("shared", profile); err != nil {
		t.Fatal(err)
	}
	/* An address belongs to a key, so the test picks one: the recipient's own key,
	   which is also the one that has to open the blob. */
	recipientKeys, err := recipient.KeychainStatus()
	if err != nil || len(recipientKeys.Keys) == 0 {
		t.Fatalf("recipient keys: %v %+v", err, recipientKeys)
	}
	var recipientKeyID configstore.ID
	if err := recipientKeyID.UnmarshalText([]byte(recipientKeys.Keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	recipientPub, err := recipient.SharePublicKey(recipientKeyID)
	if err != nil {
		t.Fatalf("recipient public key: %v", err)
	}
	blob, err := sender.ExportSharedProfile("shared", recipientPub)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.HasPrefix(blob, "termcp-host-v1:") {
		t.Fatalf("blob = %q", blob[:24])
	}
	name, err := recipient.ImportSharedProfile(blob)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if name != "shared" {
		t.Fatalf("imported as %q", name)
	}
	got, err := recipient.ReadRaw("shared")
	if err != nil {
		t.Fatalf("read imported: %v", err)
	}
	if !strings.Contains(string(got), "shared.example") {
		t.Fatalf("imported profile = %q", got)
	}
	// A second import lands beside the first instead of overwriting it.
	second, err := recipient.ImportSharedProfile(blob)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second == name {
		t.Fatalf("second import overwrote %q", name)
	}
	// The sender cannot open what it sealed for someone else.
	if _, err := sender.ImportSharedProfile(blob); err == nil {
		t.Fatal("sender opened a blob addressed to the recipient")
	}
}
