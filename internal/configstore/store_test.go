package configstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryRoundTripRenameAndDelete(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x71))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}

	secret := []byte("host = \"private.example\"\npassword = \"synthetic-secret-value\"\n")
	if err := repo.Save("prod", secret); err != nil {
		t.Fatal(err)
	}
	if err := assertTreeOmits(t, dir, "synthetic-secret-value"); err != nil {
		t.Fatal(err)
	}
	beforeRename := repo.refsSnapshot()["prod"]
	oldPath := repo.objectPath(beforeRename)
	if err := repo.Rename("prod", "production"); err != nil {
		t.Fatal(err)
	}
	afterRename := repo.refsSnapshot()["production"]
	if afterRename.objectID != beforeRename.objectID || afterRename.generationID != beforeRename.generationID {
		t.Fatal("rename changed the profile identity or generation")
	}
	if _, err := os.Stat(repo.objectPath(afterRename)); err != nil {
		t.Fatalf("renamed ciphertext missing: %v", err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old ciphertext path remains after rename: %v", err)
	}
	if status := repo.Status(); status.Profiles != 1 {
		t.Fatalf("status after rename = %+v", status)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Read("production")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("read = %q, want original bytes", got)
	}
	clear(got)

	if err := reopened.Delete("production"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Read("production"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after delete error = %v", err)
	}
}

/* The "encrypt new profiles" preference lives in the keychain file, not in the
 * encrypted catalog, because it has to be answerable when no key is loaded.
 * A plaintext settings file next to it is not a thing this build reads: the
 * preference has exactly one home.
 *
 * Mutation check: read keys/config.settings here and the assertion below fails.
 */
func TestEncryptionPreferenceLivesInTheKeychainFile(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x70))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if repo.EncryptNew() {
		t.Fatal("new repository defaulted to encrypted profiles")
	}
	if err := repo.SetEncryptNew(true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	// A plaintext settings file is not a second source of truth.
	settingsPath := filepath.Join(dir, "keys", "config.settings")
	settingsData := []byte(`{"version":1,"encrypt_new":false}`)
	if err := os.WriteFile(settingsPath, settingsData, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.EncryptNew() {
		t.Fatal("config.settings overrode the keychain's preference")
	}
	remainingSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remainingSettings, settingsData) {
		t.Fatal("opening the repository modified the unused plaintext settings")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	/* Tampering is detected where the ciphertext is: the store still opens (it
	   cannot police every file at startup and must not refuse over one host), and
	   reading the damaged host fails authentication rather than returning bytes
	   nobody can vouch for. There is no index left to tamper with. */
	reopened2, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatalf("a damaged host must not stop the store from opening: %v", err)
	}
	if err := reopened2.Save("prod", []byte("password = \"one\"\n")); err != nil {
		t.Fatal(err)
	}
	tampered := reopened2.refsSnapshot()["prod"]
	cipherPath := reopened2.objectPath(tampered)
	cipherBytes, err := os.ReadFile(cipherPath)
	if err != nil {
		t.Fatal(err)
	}
	cipherBytes[len(cipherBytes)-1] ^= 0x80
	if err := os.WriteFile(cipherPath, cipherBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened2.Read("prod"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered ciphertext error = %v", err)
	}
	_ = reopened2.Close()
}

func TestRepositorySaveUsesStableObjectAndFreshGeneration(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x72))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	if err := repo.Save("alpha", []byte("password = \"one\"\n")); err != nil {
		t.Fatal(err)
	}
	before := repo.refsSnapshot()["alpha"]
	first := filepath.Join(
		dir, "ssh_configs", "alpha",
		strings.ToLower(hexString(before.generationID[:]))+".enc",
	)
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save("alpha", []byte("password = \"one\"\n")); err != nil {
		t.Fatal(err)
	}
	after := repo.refsSnapshot()["alpha"]
	if after.objectID != before.objectID || after.generationID == before.generationID {
		t.Fatalf("object identity = %x/%x, want stable object and fresh generation", after.objectID, after.generationID)
	}
	secondBytes, err := os.ReadFile(filepath.Join(
		dir, "ssh_configs", "alpha",
		strings.ToLower(hexString(after.generationID[:]))+".enc",
	))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("saving identical plaintext produced identical envelopes")
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old generation remains after save: %v", err)
	}
}

/* The pre-directory layout kept ciphertext in objects/<object>/<generation>.enc
 * and the name of each host in the old `store.enc` index. Migration therefore needs
 * the index: an object file carries no name, so without the index there is nothing
 * to name the host with and guessing one would recreate the wrong record.
 *
 * Mutation check: migrate before reading the index and the ciphertext never lands in
 * the host directory, because no name is known yet.
 */
func TestRepositoryMigratesTheIndexedLegacyLayout(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x76))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("password = \"synthetic\"\n")
	if err := repo.Save("kali", body); err != nil {
		t.Fatal(err)
	}
	ref := repo.refsSnapshot()["kali"]
	newPath := repo.objectPath(ref)
	legacyPath := repo.legacyObjectPath(ref)

	// Recreate the old shape: ciphertext under objects/, and an index that names it.
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newPath, legacyPath); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Dir(newPath))
	_ = os.Remove(repo.activeMarkerPath(ref))
	writeLegacyIndexForTest(t, repo, ref, true)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("legacy ciphertext was not migrated to the host directory: %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy ciphertext remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "objects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the legacy objects directory remains: %v", err)
	}
	if _, err := os.Stat(legacyCatalogPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the migrated index was not removed")
	}
	got, err := reopened.Read("kali")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("migrated profile read = %q, err %v", got, err)
	}
	/* The index's activation boolean becomes the host's own marker, so the host comes
	   back ACTIVE instead of silently losing its second layer. */
	moved := reopened.refsSnapshot()["kali"]
	if !moved.activated {
		t.Fatal("migration dropped the activation state")
	}
}

// writeLegacyIndexForTest seals a version-1 index naming one host, exactly as the
// previous format wrote it.
func writeLegacyIndexForTest(t *testing.T, repo *Repository, ref catalogRef, activated bool) {
	t.Helper()
	keyring := repo.snap().keyring
	keyID, ok := keyring.anySealableKey()
	if !ok {
		t.Fatal("no plugged key to seal a legacy index with")
	}
	storeID := keyring.StoreID()
	document := legacyCatalogDocument{
		Version:  legacyRepositoryVersion,
		StoreID:  append([]byte(nil), storeID[:]...),
		Revision: 2,
		Profiles: []legacyCatalogProfile{{
			Name:         ref.name,
			ObjectID:     append([]byte(nil), ref.objectID[:]...),
			GenerationID: append([]byte(nil), ref.generationID[:]...),
			Activated:    activated,
		}},
	}
	plaintext, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	generationID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := keyring.SealWith(plaintext,
		Identity{StoreID: storeID, ObjectID: legacyCatalogObjectID, GenerationID: generationID, Kind: KindCatalog},
		keyID, noActivationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(legacyCatalogPath(repo.dataDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyCatalogPath(repo.dataDir), envelope.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryDetectsTamperingAndWrongProvider(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x73))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save("alpha", []byte("password = \"synthetic\"\n")); err != nil {
		t.Fatal(err)
	}
	ref := repo.refsSnapshot()["alpha"]
	path := repo.objectPath(ref)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x80
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	/* Tampering is caught where the ciphertext is read. The store itself still
	   opens: it does not police every host at startup, and refusing here would take
	   every plaintext profile down over one damaged file. */
	damaged, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatalf("a tampered profile stopped the store from opening: %v", err)
	}
	if _, err := damaged.Read("alpha"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered profile error = %v; want ErrAuthentication", err)
	}
	_ = damaged.Close()

	/* A wrong KEK no longer refuses to open the store: the keychain is intact,
	   nobody can unwrap it, and every encrypted profile reports that its key is
	   not loaded. Failing the open would take the plaintext profiles down with it. */
	t.Setenv(EnvKEKVariable, environmentKEK(0x74))
	locked, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatalf("wrong Provider KEK prevented startup: %v", err)
	}
	defer locked.Close()
	if status := locked.Status(); status.Loaded || status.LoadedKeys != 0 ||
		status.ErrorCode != "keyring_authentication_failed" {
		t.Fatalf("status with the wrong KEK = %+v", status)
	}
	if _, err := locked.Read("alpha"); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("reading an unreadable profile = %v; want ErrKeyNotLoaded", err)
	}
}

/* An unreadable keychain file is damage, not a reason to refuse to start. The
 * encrypted profiles are lost either way, but refusing to open also loses every
 * plaintext profile and leaves no screen that could say what happened.
 *
 * Mutation check: return the parse error from OpenRepository instead of a
 * damaged repository and this test sees the open fail.
 */
func TestUnparseableKeychainOpensAsDamageRatherThanRefusingToStart(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x76))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save("alpha", []byte("password = \"synthetic\"\n")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keys", "config.keys")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	/* A truncated file: the length prefix promises bytes that are not there, so
	   parsing fails before any slot is even attempted. */
	if err := os.WriteFile(path, raw[:len(raw)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	damaged, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatalf("an unparseable keychain stopped the store from opening: %v", err)
	}
	defer damaged.Close()
	status := damaged.Status()
	if status.Loaded || status.ErrorCode != "keyring_missing" {
		t.Fatalf("status of a damaged keychain = %+v", status)
	}
	if _, err := damaged.Read("alpha"); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("reading a profile behind a damaged keychain = %v; want ErrKeyNotLoaded", err)
	}
}

func TestRepositoryCoexistsWithPlaintextAndRejectsConcurrentOpen(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x75))
	dir := t.TempDir()
	legacy := filepath.Join(dir, "ssh_configs", "alpha", "config.toml")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("kind = \"remote\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("initialization moved an unselected plaintext profile: %v", err)
	}
	if _, err := OpenRepository(dir, NewEnvProvider("")); err == nil {
		t.Fatal("a second repository opened while the first held the directory lock")
	}
}

func assertTreeOmits(t *testing.T, root, needle string) error {
	t.Helper()
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		// The directory lock is an artifact, not content: on Windows the byte
		// range LockFileEx holds cannot be read by anyone else, so walking the
		// tree while the repository is open fails on this file alone.
		if entry.Name() == ".config.lock" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(needle)) {
			t.Fatalf("persisted file %s contains synthetic plaintext", path)
		}
		return nil
	})
}

func hexString(value []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(value)*2)
	for i, b := range value {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&0x0f]
	}
	return string(out)
}
