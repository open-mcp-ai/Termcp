package sshconfig

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

var identityFixture = []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"operator\"\npassword = \"placeholder\"\n# keep this comment\n")

func TestIdentitySurvivesRenameReloadAndStorageConversion(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plaintext", true: "encrypted"}[encrypted], func(t *testing.T) {
			t.Setenv(configstore.EnvKEKVariable, environmentConfigKEK(0x36))
			dir := t.TempDir()
			store := NewStore(dir)
			t.Cleanup(func() { _ = store.Close() })
			if encrypted {
				if err := store.SetNewProfileEncryption(true, configstore.NewEnvProvider("")); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Save("original", identityFixture); err != nil {
				t.Fatal(err)
			}
			id, err := store.Identity("original")
			if err != nil || !validProfileIdentity(id) {
				t.Fatalf("identity error = %v", err)
			}
			if raw, err := os.ReadFile(filepath.Join(store.ConfigDir("original"), "identity")); err != nil || string(raw) != id+"\n" {
				t.Fatalf("identity sidecar error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(store.ConfigDir("original"), "related-file.txt"), []byte("associated file"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := store.Rename("original", "renamed"); err != nil {
				t.Fatal(err)
			}
			if got, err := store.Identity("renamed"); err != nil || got != id {
				t.Fatalf("renamed identity error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(store.ConfigDir("renamed"), "related-file.txt")); err != nil {
				t.Fatal("profile rename did not carry associated files")
			}
			if err := os.Rename(store.ConfigDir("renamed"), store.ConfigDir("moved")); err != nil {
				t.Fatal(err)
			}
			if got, err := store.NameByIdentity(id); err != nil || got != "moved" {
				t.Fatalf("directory rename resolved to %q, error %v", got, err)
			}
			if raw, err := store.ReadRaw("moved"); err != nil || !bytes.Equal(raw, identityFixture) {
				t.Fatalf("directory rename changed the profile: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenConfiguredStore(dir, configstore.NewEnvProvider(""))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if got, err := reopened.Identity("moved"); err != nil || got != id {
				t.Fatalf("restarted identity error = %v", err)
			}
			if encrypted {
				if _, err := reopened.RestoreProfiles([]string{"moved"}); err != nil {
					t.Fatal(err)
				}
				if got, _ := reopened.Identity("moved"); got != id {
					t.Fatal("restore changed the identity")
				}
				if _, err := reopened.EncryptProfiles([]string{"moved"}); err != nil {
					t.Fatal(err)
				}
				if got, _ := reopened.Identity("moved"); got != id {
					t.Fatal("encryption changed the identity")
				}
			}
			if err := reopened.Delete("moved"); err != nil {
				t.Fatal(err)
			}
			if err := reopened.Save("moved", identityFixture); err != nil {
				t.Fatal(err)
			}
			if fresh, err := reopened.Identity("moved"); err != nil || fresh == id {
				t.Fatalf("same-name recreation reused the identity: %v", err)
			}
			if _, _, err := reopened.ResolveReference("moved", id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted identity fell back to a reused name: %v", err)
			}
		})
	}
}

func TestIdentityBackfillsWithoutRewritingPlaintext(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "ssh_configs", "legacy")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "config.toml"), identityFixture, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := os.Stat(filepath.Join(profileDir, "identity")); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(profileDir, "config.toml")); err != nil || !bytes.Equal(raw, identityFixture) {
		t.Fatal("identity initialization rewrote the plaintext configuration")
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "internal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the virtual internal profile created a directory")
	}
}

func TestIdentityRejectsDuplicatesAndMalformedSidecars(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, name := range []string{"one", "two"} {
		if err := store.Save(name, identityFixture); err != nil {
			t.Fatal(err)
		}
	}
	id, err := store.Identity("one")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.ConfigDir("two"), "identity")
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshIdentities(); !errors.Is(err, ErrDuplicateIdentity) {
		t.Fatalf("duplicate identity error = %v", err)
	}
	invalid := []byte("not-a-uuid\n")
	if err := os.WriteFile(path, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshIdentities(); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("malformed identity error = %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, invalid) {
		t.Fatal("a malformed identity was silently regenerated")
	}
}

func TestIdentityConcurrentBackfillPublishesOneCompleteUUID(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "ssh_configs", "shared")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "config.toml"), identityFixture, 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 16)
	errorsCh := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := NewStore(dir).Identity("shared")
			results <- id
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	winner := ""
	for id := range results {
		if !validProfileIdentity(id) || winner != "" && id != winner {
			t.Fatal("concurrent readers observed incomplete or differing identities")
		}
		winner = id
	}
}

func TestTemporaryIdentityFollowsRenameAndPersistence(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.SaveWithOptions("scratch", identityFixture, true); err != nil {
		t.Fatal(err)
	}
	id, err := store.Identity("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.ConfigDir("scratch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary profile persisted its identity")
	}
	if err := store.Rename("scratch", "saved"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("saved", identityFixture); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Identity("saved"); got != id {
		t.Fatal("making a temporary profile persistent changed its identity")
	}
	if err := store.SaveWithOptions("saved", identityFixture, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Identity("saved"); got != id {
		t.Fatal("making a profile temporary changed its identity")
	}
}
