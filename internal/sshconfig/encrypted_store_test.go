package sshconfig

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

func TestEncryptedStoreRoundTripTemporaryAndInternal(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x31))
	dir := t.TempDir()
	repo, err := configstore.InitializeRepository(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	store := NewEncryptedStore(dir, repo)

	remote := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.Save("prod", remote); err != nil {
		t.Fatal(err)
	}
	temporary := []byte("kind = \"remote\"\nhost = \"temp.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.SaveWithOptions("scratch", temporary, true); err != nil {
		t.Fatal(err)
	}
	override := []byte("kind = \"internal\"\ndefault_approval = true\n")
	if err := store.Save("internal", override); err != nil {
		t.Fatal(err)
	}

	names, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "internal,prod,scratch" {
		t.Fatalf("list = %s", got)
	}
	if entry, err := store.Load("scratch"); err != nil || entry.Host != "temp.example" {
		t.Fatalf("temporary profile = %+v, err %v", entry, err)
	}
	if entry, err := store.Load("internal"); err != nil || !entry.DefaultApproval {
		t.Fatalf("internal override = %+v, err %v", entry, err)
	}
	if err := store.Rename("prod", "production"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("scratch"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("scratch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted temporary load error = %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedRepo, err := configstore.OpenRepository(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedRepo.Close()
	restarted := NewEncryptedStore(dir, reopenedRepo)
	if entry, err := restarted.Load("production"); err != nil || entry.Host != "host.example" {
		t.Fatalf("restarted profile = %+v, err %v", entry, err)
	}
	if _, err := restarted.Load("scratch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("temporary profile survived restart: %v", err)
	}
	if entry, err := restarted.Load("internal"); err != nil || !entry.DefaultApproval {
		t.Fatalf("restarted internal override = %+v, err %v", entry, err)
	}
}

func TestStoreRotatesMainKeyPreservingMixedProfileModes(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x35))
	dir := t.TempDir()
	store := NewStore(dir)
	t.Cleanup(func() { _ = store.Close() })
	provider := configstore.NewEnvProvider("")
	if err := store.RotateMainKey(provider); !errors.Is(err, configstore.ErrNotInitialized) {
		t.Fatalf("rotation before initialization = %v", err)
	}
	raw := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.Save("plain", raw); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNewProfileEncryption(true, provider); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("encrypted", raw); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWithOptions("temporary", raw, true); err != nil {
		t.Fatal(err)
	}
	before, err := store.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	// Callbacks may read state and must run after the Store lock is released.
	store.SetOnChange(func() { _, _ = store.EncryptionStatus() })
	if err := store.RotateMainKey(provider); err != nil {
		t.Fatal(err)
	}
	after, err := store.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if before.MainKeyID == after.MainKeyID || len(after.MainKeyID) != 32 || after.RotationPending || !after.EncryptNew {
		t.Fatalf("unexpected rotation status: %+v", after)
	}
	for name, expected := range map[string]string{
		"plain": StoragePlaintext, "encrypted": StorageEncrypted, "temporary": StorageTemporary,
	} {
		if mode, err := store.StorageMode(name); err != nil || mode != expected {
			t.Fatalf("%s mode = %q, error %v", name, mode, err)
		}
		if data, err := store.ReadRaw(name); err != nil || !bytes.Equal(data, raw) {
			t.Fatalf("%s contents changed: %v", name, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	state, err := reopened.EncryptionStatus()
	if err != nil || state.MainKeyID != after.MainKeyID || state.RotationPending {
		t.Fatalf("restarted encryption status = %+v, error %v", state, err)
	}
	if _, err := reopened.Load("encrypted"); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptionDefaultsOffAndConvertsSelectedHosts(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x32))
	dir := t.TempDir()
	store, err := OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	remote := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"synthetic-conversion-secret\"\n")
	if err := store.Save("plain-existing", remote); err != nil {
		t.Fatal(err)
	}
	if mode, err := store.StorageMode("plain-existing"); err != nil || mode != StoragePlaintext {
		t.Fatalf("default save mode = %q, err %v", mode, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "plain-existing", "config.toml")); err != nil {
		t.Fatalf("default profile is not plaintext: %v", err)
	}
	if err := store.SetNewProfileEncryption(true, configstore.NewEnvProvider("")); err != nil {
		t.Fatal(err)
	}
	status, err := store.EncryptionStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Initialized || !status.EncryptNew {
		t.Fatalf("status after enable = %+v", status)
	}
	if err := store.Save("new-encrypted", remote); err != nil {
		t.Fatal(err)
	}
	if mode, _ := store.StorageMode("new-encrypted"); mode != StorageEncrypted {
		t.Fatalf("new profile mode = %q, want encrypted", mode)
	}
	if mode, _ := store.StorageMode("plain-existing"); mode != StoragePlaintext {
		t.Fatalf("enabling new-profile encryption converted an existing host: %q", mode)
	}
	if err := store.Save("plain-existing", append(remote, []byte("# edited\n")...)); err != nil {
		t.Fatal(err)
	}
	if mode, _ := store.StorageMode("plain-existing"); mode != StoragePlaintext {
		t.Fatalf("editing an existing plaintext host converted it to %q", mode)
	}

	results, err := store.EncryptProfiles([]string{"plain-existing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("encrypt results = %+v", results)
	}
	if mode, _ := store.StorageMode("plain-existing"); mode != StorageEncrypted {
		t.Fatalf("selected profile mode = %q, want encrypted", mode)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "plain-existing", "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected plaintext source remains: %v", err)
	}

	if err := store.SetNewProfileEncryption(false); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("later-plain", remote); err != nil {
		t.Fatal(err)
	}
	if mode, _ := store.StorageMode("later-plain"); mode != StoragePlaintext {
		t.Fatalf("profile saved after disable mode = %q, want plaintext", mode)
	}
	if err := store.Save("new-encrypted", append(remote, []byte("# edited\n")...)); err != nil {
		t.Fatal(err)
	}
	if mode, _ := store.StorageMode("new-encrypted"); mode != StorageEncrypted {
		t.Fatalf("editing an encrypted profile downgraded it to %q", mode)
	}

	results, err = store.RestoreProfiles([]string{"new-encrypted"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("restore results = %+v", results)
	}
	if mode, _ := store.StorageMode("new-encrypted"); mode != StoragePlaintext {
		t.Fatalf("restored profile mode = %q, want plaintext", mode)
	}
}

func TestEncryptionTestArtifactsAreIgnoredWithoutBlockingStartup(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x33))
	dir := t.TempDir()
	marker := filepath.Join(dir, "keys", "master.key")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("incompatible"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatalf("startup rejected legacy encryption-test artifacts: %v", err)
	}
	defer store.Close()
	if warning := store.StartupWarning(); warning == "" {
		t.Fatal("startup did not report the ignored encryption-test artifacts")
	}
	if err := store.SetNewProfileEncryption(true, configstore.NewEnvProvider("")); err != nil {
		t.Fatalf("enable with ignored legacy artifacts: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("legacy artifact was modified: %v", err)
	}
}

func TestOpenConfiguredStoreKeepsMixedPlaintextAndEncryptedHosts(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x36))
	dir := t.TempDir()
	store := NewStore(dir)
	raw := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.Save("plain", raw); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNewProfileEncryption(true, configstore.NewEnvProvider("")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("secret", raw); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if mode, _ := reopened.StorageMode("plain"); mode != StoragePlaintext {
		t.Fatalf("existing plaintext mode after restart = %q", mode)
	}
	if mode, _ := reopened.StorageMode("secret"); mode != StorageEncrypted {
		t.Fatalf("encrypted mode after restart = %q", mode)
	}
	if entry, err := reopened.Load("secret"); err != nil || entry.Host != "host.example" {
		t.Fatalf("encrypted profile after restart = %+v, err %v", entry, err)
	}
}

func TestEnabledEncryptionWithoutKeyringFailsClosed(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x37))
	dir := t.TempDir()
	repo, err := configstore.InitializeRepository(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEncryptNew(true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "keys", "config.keys")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenConfiguredStore(dir, configstore.NewEnvProvider("")); !errors.Is(err, configstore.ErrNotInitialized) {
		t.Fatalf("missing keyring error = %v", err)
	}
}

func TestEncryptedStoreKeepsTemporaryCollisionAndSelectionRules(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x34))
	dir := t.TempDir()
	repo, err := configstore.InitializeRepository(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	store := NewEncryptedStore(dir, repo)
	defer store.Close()
	raw := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n")
	if err := store.Save("Prod", raw); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWithOptions("prod", raw, true); err == nil {
		t.Fatal("temporary save bypassed a case-insensitive persistent collision")
	}
	if _, err := store.ExportBatch([]string{"deleted-before-export"}); err != nil {
		t.Fatalf("selected export did not skip a missing encrypted profile: %v", err)
	}
}

func TestOpenConfiguredStoreValidatesCommittedTOML(t *testing.T) {
	t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x35))
	dir := t.TempDir()
	repo, err := configstore.InitializeRepository(dir, configstore.NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Save("broken", []byte("kind = \"remote\"\nhost = \"\"")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenConfiguredStore(dir, configstore.NewEnvProvider("")); err == nil {
		t.Fatal("configured store accepted a committed profile with invalid TOML")
	}
}

func environmentConfigKEK(value byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))
}
