package extensions

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestExtensionTargetIdentitySurvivesDirectoryRenameAndRestart(t *testing.T) {
	dir := t.TempDir()
	profiles := sshconfig.NewStore(dir)
	raw := []byte("kind = \"internal\"\n")
	if err := profiles.Save("original", raw); err != nil {
		t.Fatal(err)
	}
	identity, err := profiles.Identity("original")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	store.SetSSHConfigStore(profiles)
	ext := &Extension{Name: "tool", Kind: KindCustom, Target: "original", Command: "echo", Args: []string{"placeholder"}, Mode: "pipe"}
	if err := store.Save(ext); err != nil {
		t.Fatal(err)
	}
	if ext.TargetIdentity != identity {
		t.Fatal("saving the extension did not bind the target identity")
	}
	group, fingerprintBefore := groupKey(ext), fingerprint(ext)
	if err := os.Rename(profiles.ConfigDir("original"), profiles.ConfigDir("renamed")); err != nil {
		t.Fatal(err)
	}
	restartedProfiles := sshconfig.NewStore(dir)
	if err := restartedProfiles.RefreshIdentities(); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(dir)
	restarted.SetSSHConfigStore(restartedProfiles)
	loaded, err := restarted.Load("tool")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Target != "renamed" || loaded.TargetIdentity != identity {
		t.Fatalf("renamed target = %q", loaded.Target)
	}
	if groupKey(loaded) != group || fingerprint(loaded) != fingerprintBefore || namespaceTarget(loaded) != namespaceTarget(ext) {
		t.Fatal("renaming a host changed the extension connection identity")
	}
	if err := restartedProfiles.Delete("renamed"); err != nil {
		t.Fatal(err)
	}
	if err := restartedProfiles.Save("renamed", raw); err != nil {
		t.Fatal(err)
	}
	loaded, err = restarted.Load("tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restartedProfiles.ResolveReference(loaded.Target, loaded.TargetIdentity); !errors.Is(err, sshconfig.ErrNotFound) {
		t.Fatalf("old extension silently rebound to a same-named new host: %v", err)
	}
	// Explicitly removing the binding and saving chooses the named host anew.
	loaded.TargetIdentity = ""
	loaded.Target = "renamed"
	if err := restarted.Save(loaded); err != nil {
		t.Fatal(err)
	}
	newIdentity, err := restartedProfiles.Identity("renamed")
	if err != nil || loaded.TargetIdentity != newIdentity || newIdentity == identity {
		t.Fatal("explicit retargeting did not bind the new host")
	}
}

func TestLegacyExtensionIdentityBindingRetainsTemplateBytes(t *testing.T) {
	dir := t.TempDir()
	profiles := sshconfig.NewStore(dir)
	if err := profiles.Save("original", []byte("kind = \"internal\"\n")); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	store.SetSSHConfigStore(profiles)
	path, err := store.configPath("legacy")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("# keep this comment\nkind = \"custom\"\ntarget = \"original\"\ncommand = \"echo\"\nargs = [\"placeholder\"]\nmode = \"pipe\"\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("legacy")
	if err != nil || loaded.TargetIdentity == "" {
		t.Fatalf("legacy binding error = %v", err)
	}
	bound, err := os.ReadFile(path)
	if err != nil || !bytes.HasSuffix(bound, raw) {
		t.Fatal("binding rewrote the existing template")
	}
	if _, err := store.Load("legacy"); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(bound, again) {
		t.Fatal("loading an already-bound template rewrote it")
	}
}
