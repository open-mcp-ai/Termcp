package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/vault"
)

// vaultedStore returns a store whose data dir holds an initialized, unlocked
// vault. One Init per test function: the scrypt passphrase KDF costs ~1 s, so
// subtests share the vault — they use distinct profile names to stay
// independent of each other and of execution order.
func vaultedStore(t *testing.T) (*Store, *vault.Vault, string) {
	t.Helper()
	dir := t.TempDir()
	v := vault.New(dir)
	if _, err := v.Init("test vault passphrase"); err != nil {
		t.Fatalf("vault Init: %v", err)
	}
	st := NewStore(dir)
	st.SetVault(v)
	return st, v, dir
}

func remoteProfileTOML(user, password string) []byte {
	return []byte(`kind = "remote"
host = "example.com"
user = "` + user + `"
password = "` + password + `"
`)
}

// TestStoreVaultBackend covers the unlocked-vault paths: writes seal, reads
// open, plaintext is only the migration fallback.
func TestStoreVaultBackend(t *testing.T) {
	st, v, dir := vaultedStore(t)

	t.Run("save seals and writes no plaintext", func(t *testing.T) {
		if err := st.Save("sealed", remoteProfileTOML("root", "c4nary-sealed-secret")); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if !v.Has("sealed") {
			t.Fatal("no .age file after Save")
		}
		if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "sealed", "config.toml")); !os.IsNotExist(err) {
			t.Fatalf("plaintext config.toml exists next to the ciphertext: %v", err)
		}
		ent, err := st.Load("sealed")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if ent.Password != "c4nary-sealed-secret" {
			t.Fatalf("Load lost the password: %+v", ent)
		}
		// Byte-level canary: the secret must appear nowhere under the data dir.
		err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "c4nary-sealed-secret") {
				t.Errorf("plaintext secret found in %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
	})

	t.Run("load falls back to plaintext before migration", func(t *testing.T) {
		legacyDir := filepath.Join(dir, "ssh_configs", "legacy")
		if err := os.MkdirAll(legacyDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyDir, "config.toml"), remoteProfileTOML("op", "legacy-plain"), 0600); err != nil {
			t.Fatal(err)
		}
		ent, err := st.Load("legacy")
		if err != nil {
			t.Fatalf("Load plaintext fallback: %v", err)
		}
		if ent.Password != "legacy-plain" {
			t.Fatalf("fallback lost the password: %+v", ent)
		}
	})

	t.Run("rewriting a plaintext profile migrates and scrubs it", func(t *testing.T) {
		if err := st.Save("legacy", remoteProfileTOML("op", "legacy-now-sealed")); err != nil {
			t.Fatalf("Save over plaintext: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "legacy", "config.toml")); !os.IsNotExist(err) {
			t.Fatalf("stale plaintext survived re-save: %v", err)
		}
		ent, err := st.Load("legacy")
		if err != nil || ent.Password != "legacy-now-sealed" {
			t.Fatalf("migrated load: %+v %v", ent, err)
		}
	})

	t.Run("list merges vault, disk, temporary and internal", func(t *testing.T) {
		// One profile per storage tier: sealed (vault), plus a fresh plaintext
		// one written directly, plus a temporary one.
		diskDir := filepath.Join(dir, "ssh_configs", "ondisk")
		if err := os.MkdirAll(diskDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(diskDir, "config.toml"), remoteProfileTOML("op", "disk-pw"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveWithOptions("temp", remoteProfileTOML("op", "temp-pw"), true); err != nil {
			t.Fatal(err)
		}
		names, err := st.List()
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(names, ",")
		for _, want := range []string{"internal", "sealed", "legacy", "ondisk", "temp"} {
			if !strings.Contains(joined, want) {
				t.Errorf("List() = %v, missing %q", names, want)
			}
		}
	})

	t.Run("delete removes the ciphertext", func(t *testing.T) {
		if err := st.Delete("sealed"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if v.Has("sealed") {
			t.Fatal(".age survived Delete")
		}
	})

	t.Run("rename reseals under the new name and scrubs the old", func(t *testing.T) {
		if err := st.Save("before", remoteProfileTOML("op", "rename-me")); err != nil {
			t.Fatal(err)
		}
		if err := st.Rename("before", "after"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		if v.Has("before") {
			t.Fatal("old ciphertext survived Rename")
		}
		ent, err := st.Load("after")
		if err != nil || ent.Password != "rename-me" {
			t.Fatalf("renamed load: %+v %v", ent, err)
		}
	})

	t.Run("temporary supersedes and drops the vaulted copy", func(t *testing.T) {
		if err := st.Save("dual", remoteProfileTOML("op", "persistent-pw")); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveWithOptions("dual", remoteProfileTOML("op", "temp-pw"), true); err != nil {
			t.Fatal(err)
		}
		if v.Has("dual") {
			t.Fatal("vaulted copy kept alongside a temporary one")
		}
		ent, err := st.Load("dual")
		if err != nil || ent.Password != "temp-pw" {
			t.Fatalf("temporary did not win Load order: %+v %v", ent, err)
		}
	})

	t.Run("plaintext export withholds vaulted profiles and names them", func(t *testing.T) {
		res, err := st.ExportBatch(nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.SkippedVaulted) == 0 {
			t.Fatal("no vaulted profiles reported as skipped")
		}
		for _, skipped := range res.SkippedVaulted {
			if strings.Contains(string(res.Document), skipped) {
				t.Errorf("withheld profile %q leaked into the plaintext export", skipped)
			}
		}
		// The plaintext (not-yet-migrated) profile is still exported.
		if !strings.Contains(string(res.Document), "ondisk") {
			t.Error("plaintext profile missing from plaintext export")
		}
	})

	t.Run("sealed export decrypts vaulted profiles into the envelope", func(t *testing.T) {
		res, err := st.ExportBatch(nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.SkippedVaulted) != 0 {
			t.Errorf("sealed export skipped %v", res.SkippedVaulted)
		}
		if !strings.Contains(string(res.Document), "legacy") {
			t.Error("vaulted profile missing from sealed export")
		}
	})
}

// TestStoreVaultLocked covers the locked-vault paths: credential-consuming
// operations fail with ErrLocked, reads of unmigrated plaintext still work.
func TestStoreVaultLocked(t *testing.T) {
	st, v, dir := vaultedStore(t)
	if err := st.Save("sealed", remoteProfileTOML("op", "locked-away")); err != nil {
		t.Fatal(err)
	}
	// A plaintext leftover from before the vault existed.
	legacyDir := filepath.Join(dir, "ssh_configs", "legacy")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.toml"), remoteProfileTOML("op", "legacy-pw"), 0600); err != nil {
		t.Fatal(err)
	}
	v.Lock()

	t.Run("load of a vaulted profile fails locked", func(t *testing.T) {
		if _, err := st.Load("sealed"); !errors.Is(err, vault.ErrLocked) {
			t.Fatalf("Load: got %v, want vault.ErrLocked", err)
		}
	})
	t.Run("save fails locked", func(t *testing.T) {
		if err := st.Save("new", remoteProfileTOML("op", "x")); !errors.Is(err, vault.ErrLocked) {
			t.Fatalf("Save: got %v, want vault.ErrLocked", err)
		}
	})
	t.Run("readraw fails locked", func(t *testing.T) {
		if _, err := st.ReadRaw("sealed"); !errors.Is(err, vault.ErrLocked) {
			t.Fatalf("ReadRaw: got %v, want vault.ErrLocked", err)
		}
	})
	t.Run("rename fails locked", func(t *testing.T) {
		if err := st.Rename("sealed", "sealed2"); !errors.Is(err, vault.ErrLocked) {
			t.Fatalf("Rename: got %v, want vault.ErrLocked", err)
		}
	})
	t.Run("sealed export fails locked, plaintext export skips", func(t *testing.T) {
		if _, err := st.ExportBatch(nil, true); !errors.Is(err, vault.ErrLocked) {
			t.Fatalf("sealed ExportBatch: got %v, want vault.ErrLocked", err)
		}
		res, err := st.ExportBatch(nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.SkippedVaulted) != 1 || res.SkippedVaulted[0] != "sealed" {
			t.Fatalf("SkippedVaulted = %v, want [sealed]", res.SkippedVaulted)
		}
	})
	t.Run("plaintext fallback still loads while locked", func(t *testing.T) {
		ent, err := st.Load("legacy")
		if err != nil || ent.Password != "legacy-pw" {
			t.Fatalf("locked fallback load: %+v %v", ent, err)
		}
	})
	t.Run("list shows vaulted names while locked", func(t *testing.T) {
		names, err := st.List()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(names, ","), "sealed") {
			t.Fatalf("locked List() = %v, want sealed present", names)
		}
	})
}
