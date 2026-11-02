package configstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCentralProfileReturnsToHostDirectoryAfterUnlock(t *testing.T) {
	dir := t.TempDir()
	r, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err = r.Save("host", []byte("synthetic configuration")); err != nil {
		t.Fatal(err)
	}
	ref := r.refs["host"]
	key := hex.EncodeToString(ref.objectID[:]) + "/" + hex.EncodeToString(ref.generationID[:])
	host := r.objectPath(ref)
	body, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	central, _ := localBodyPath(dir, key)
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(central), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(host, central); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	state = cloneLocalState(state)
	delete(state.ProfilePaths, key)
	rel, err := filepath.Rel(r.dataDir, host)
	if err != nil {
		t.Fatal(err)
	}
	state.Legacy = append(state.Legacy, filepath.ToSlash(rel))
	if err = publishLocalState(dir, state, writeFileAtomic); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Status().Locked {
		t.Fatal("password-only store unexpectedly unlocked")
	}
	if _, err = os.Stat(host); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("locked migration relocated profile")
	}
	if err = r.UnlockPassword("incorrect-password"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err = os.Stat(central); err != nil {
		t.Fatal("failed unlock removed migration source", err)
	}
	if err = r.UnlockPassword(firstPassword); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Read("host"); err != nil || string(got) != "synthetic configuration" {
		t.Fatal("migration lost profile", err)
	}
	got, err := os.ReadFile(host)
	if err != nil || !bytes.Equal(body, got) {
		t.Fatal("migration changed profile ciphertext", err)
	}
	if _, err = os.Stat(central); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verified profile retained in central body storage", err)
	}
	// A removed host body cannot silently fall back to stale migration copies.
	complete, err := r.readObjectRaw(ref)
	if err != nil {
		t.Fatal(err)
	}
	legacy := r.legacyObjectPath(ref)
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = writeFileAtomic(central, body); err != nil {
		t.Fatal(err)
	}
	if err = writeFileAtomic(legacy, complete); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(host); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.UnlockPassword(firstPassword); err == nil {
		t.Fatal("missing host body fell back to central shadow")
	}
}

func TestHostBodyMappingRejectsOutsidePaths(t *testing.T) {
	r := passwordRepository(t)
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	state = cloneLocalState(state)
	for key := range state.ProfilePaths {
		state.ProfilePaths[key] = "../outside.enc"
		break
	}
	if len(state.ProfilePaths) == 0 {
		t.Fatal("fixture lacks a host profile")
	}
	if err = publishLocalState(r.dataDir, state, writeFileAtomic); err != nil {
		t.Fatal(err)
	}
	if _, err = loadLocalState(r.dataDir); err == nil {
		t.Fatal("untrusted body path escaped host storage")
	}
}

func TestHostMigrationRetainsLegacySourceWhenPublicationFails(t *testing.T) {
	r := passwordRepository(t)
	var ref catalogRef
	for _, v := range r.refs {
		ref = v
		break
	}
	raw, err := r.readObjectRaw(ref)
	if err != nil {
		t.Fatal(err)
	}
	root, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry := "ssh_configs/host/" + hex.EncodeToString(ref.generationID[:]) + ".enc"
	source := filepath.Join(root, filepath.FromSlash(entry))
	if err = writeFileAtomic(source, raw); err != nil {
		t.Fatal(err)
	}
	state := emptyLocalState()
	if err = applyLocalEntry(root, &state, entry, raw); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected state publication failure")
	err = publishLocalState(root, state, func(string, []byte) error { return injected })
	if !errors.Is(err, injected) {
		t.Fatal("publication failure lost", err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("uncommitted migration changed legacy host ciphertext", err)
	}
	if _, err = os.Stat(LocalStatePath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed migration published global state")
	}
	plain, err := r.openObjectRaw(ref, after)
	clear(plain)
	if err != nil {
		t.Fatal("legacy source no longer decrypts", err)
	}
}

func TestManualHostDirectoryRenameKeepsCiphertextAndIdentity(t *testing.T) {
	r := passwordRepository(t)
	var old string
	var ref catalogRef
	for name, v := range r.refs {
		old, ref = name, v
		break
	}
	before, err := os.ReadFile(r.objectPath(ref))
	if err != nil {
		t.Fatal(err)
	}
	dir := r.dataDir
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(dir, "ssh_configs", old), filepath.Join(dir, "ssh_configs", "moved-host")); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.UnlockPassword(firstPassword); err != nil {
		t.Fatal(err)
	}
	next, exists := reopened.refs["moved-host"]
	if !exists || next.objectID != ref.objectID || next.generationID != ref.generationID || next.profileIdentity != ref.profileIdentity {
		t.Fatal("directory rename lost authenticated host identity")
	}
	after, err := os.ReadFile(reopened.objectPath(next))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("directory rename rewrote ciphertext", err)
	}
}

func TestMissingGlobalStateCannotOverwriteHostCiphertext(t *testing.T) {
	r := passwordRepository(t)
	dir := r.dataDir
	bodies := map[string][]byte{}
	for _, ref := range r.refs {
		raw, err := os.ReadFile(r.objectPath(ref))
		if err != nil {
			t.Fatal(err)
		}
		bodies[r.objectPath(ref)] = raw
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "local-state")); err != nil {
		t.Fatal(err)
	}
	if opened, err := InitializeRepositoryWithPassword(dir, secondPassword, secondPassword); err == nil {
		_ = opened.Close()
		t.Fatal("missing state authorized replacing encrypted profiles")
	}
	for path, before := range bodies {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("failed initialization changed retained host ciphertext", err)
		}
	}
}

func TestHostBodyMappingMustMatchAuthenticatedCatalog(t *testing.T) {
	r := passwordRepository(t)
	dir := r.dataDir
	state, err := loadLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	state = cloneLocalState(state)
	var path string
	for _, ref := range r.refs {
		path = r.objectPath(ref)
		key := hex.EncodeToString(ref.objectID[:]) + "/" + hex.EncodeToString(ref.generationID[:])
		state.ProfilePaths[key] = "ssh_configs/other-host/" + hex.EncodeToString(ref.generationID[:]) + ".enc"
		break
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = publishLocalState(dir, state, writeFileAtomic); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.UnlockPassword(firstPassword); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatal("unauthenticated host path overrode the catalog", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid mapping removed the real host body", err)
	}
}
