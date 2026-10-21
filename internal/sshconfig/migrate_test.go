package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/vault"
)

// writeLegacyProfile plants a plaintext profile the way a pre-vault release
// would have left it.
func writeLegacyProfile(t *testing.T, dir, name, password string) {
	t.Helper()
	d := filepath.Join(dir, "ssh_configs", name)
	if err := os.MkdirAll(d, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "config.toml"), remoteProfileTOML("op", password), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateToVault(t *testing.T) {
	st, v, dir := vaultedStore(t)
	writeLegacyProfile(t, dir, "prod", "prod-secret")
	writeLegacyProfile(t, dir, "staging", "staging-secret")

	res, err := st.MigrateToVault()
	if err != nil {
		t.Fatalf("MigrateToVault: %v", err)
	}
	if len(res.Migrated) != 2 || res.Migrated[0] != "prod" || res.Migrated[1] != "staging" {
		t.Fatalf("Migrated = %v, want [prod staging]", res.Migrated)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v", res.Failed)
	}

	// Plaintext is gone, ciphertext serves the load, secrets survive.
	for _, name := range []string{"prod", "staging"} {
		if _, err := os.Stat(filepath.Join(dir, "ssh_configs", name, "config.toml")); !os.IsNotExist(err) {
			t.Errorf("%s: plaintext survived migration", name)
		}
		if !v.Has(name) {
			t.Errorf("%s: no ciphertext after migration", name)
		}
	}
	ent, err := st.Load("prod")
	if err != nil || ent.Password != "prod-secret" {
		t.Fatalf("migrated prod load: %+v %v", ent, err)
	}

	// Idempotent: the rerun finds nothing to do.
	res, err = st.MigrateToVault()
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if len(res.Migrated) != 0 || len(res.Failed) != 0 {
		t.Fatalf("rerun Migrated=%v Failed=%v, want both empty", res.Migrated, res.Failed)
	}
}

func TestMigrateToVaultKeepsAFailedProfilePlaintext(t *testing.T) {
	st, v, dir := vaultedStore(t)
	writeLegacyProfile(t, dir, "good", "good-secret")
	// A broken TOML profile: validation must fail it without touching others.
	badDir := filepath.Join(dir, "ssh_configs", "bad")
	if err := os.MkdirAll(badDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "config.toml"), []byte("kind = \"remote\"\nhost = \"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := st.MigrateToVault()
	if err != nil {
		t.Fatalf("MigrateToVault: %v", err)
	}
	if len(res.Migrated) != 1 || res.Migrated[0] != "good" {
		t.Fatalf("Migrated = %v, want [good]", res.Migrated)
	}
	if len(res.Failed) != 1 || res.Failed[0].Name != "bad" {
		t.Fatalf("Failed = %+v, want [bad]", res.Failed)
	}
	// The failure's plaintext stays put — losing it would be data loss.
	if _, err := os.Stat(filepath.Join(badDir, "config.toml")); err != nil {
		t.Errorf("bad profile's plaintext removed: %v", err)
	}
	if v.Has("bad") {
		t.Error("bad profile got sealed despite failing validation")
	}
}

func TestMigrateToVaultNeedsAnUnlockedVault(t *testing.T) {
	st, v, dir := vaultedStore(t)
	writeLegacyProfile(t, dir, "prod", "x")
	v.Lock()
	if _, err := st.MigrateToVault(); !errors.Is(err, vault.ErrLocked) {
		t.Fatalf("locked MigrateToVault: got %v, want vault.ErrLocked", err)
	}
}

func TestMigrateToVaultWithoutAVault(t *testing.T) {
	st := NewStore(t.TempDir())
	if _, err := st.MigrateToVault(); err == nil {
		t.Fatal("migrate without a vault succeeded")
	}
}
