package configstore

import (
	"bytes"
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
	beforeRename := repo.refs["prod"]
	oldPath := repo.objectPath(beforeRename)
	if err := repo.Rename("prod", "production"); err != nil {
		t.Fatal(err)
	}
	afterRename := repo.refs["production"]
	if afterRename.objectID != beforeRename.objectID || afterRename.generationID != beforeRename.generationID {
		t.Fatal("rename changed the profile identity or generation")
	}
	if _, err := os.Stat(repo.objectPath(afterRename)); err != nil {
		t.Fatalf("renamed ciphertext missing: %v", err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old ciphertext path remains after rename: %v", err)
	}
	firstStatus := repo.Status()
	if firstStatus.Profiles != 1 || firstStatus.Revision != 3 {
		t.Fatalf("status after rename = %+v", firstStatus)
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

func TestEncryptionPreferenceIsAuthenticatedCatalogState(t *testing.T) {
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
	// A plaintext file cannot override the authenticated catalog state.
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
		t.Fatal("unauthenticated config.settings overrode the catalog")
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
	catalogPath := repositoryCatalogPath(dir)
	catalogBytes, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	catalogBytes[len(catalogBytes)-1] ^= 0x80
	if err := os.WriteFile(catalogPath, catalogBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRepository(dir, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered catalog error = %v", err)
	}
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
	before := repo.refs["alpha"]
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
	after := repo.refs["alpha"]
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

func TestRepositoryMigratesLegacyObjectsLayout(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x76))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save("kali", []byte("password = \"synthetic\"\n")); err != nil {
		t.Fatal(err)
	}
	ref := repo.refs["kali"]
	newPath := repo.objectPath(ref)
	legacyPath := repo.legacyObjectPath(ref)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newPath, legacyPath); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Dir(newPath))

	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("legacy ciphertext was not migrated to the profile directory: %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy ciphertext remains: %v", err)
	}
	got, err := reopened.Read("kali")
	if err != nil || !bytes.Equal(got, []byte("password = \"synthetic\"\n")) {
		t.Fatalf("migrated profile read = %q, err %v", got, err)
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
	ref := repo.refs["alpha"]
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
	if _, err := OpenRepository(dir, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered profile error = %v", err)
	}

	t.Setenv(EnvKEKVariable, environmentKEK(0x74))
	if _, err := OpenRepository(dir, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong Provider KEK error = %v", err)
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
