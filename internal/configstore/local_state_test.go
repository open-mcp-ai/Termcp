package configstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalStateDetachesEDKsAndRotatesWithoutRewritingBodies(t *testing.T) {
	dir := t.TempDir()
	r, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err = r.Save("one", []byte("first configuration")); err != nil {
		t.Fatal(err)
	}
	if err = r.Save("two", []byte("second configuration")); err != nil {
		t.Fatal(err)
	}
	if err = r.WriteAuthentication([]byte("authentication payload")); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{}
	infos := map[string]os.FileInfo{}
	for key, header := range state.Envelopes {
		path, _ := bodyPathInState(dir, state, key)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body[:dataHeaderSize], header[:dataHeaderSize]) {
			t.Fatal("detached header does not identify body")
		}
		bodies[path] = body
		infos[path], err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(dir, "keys", "config.keys"), repositoryCatalogPath(dir), authenticationPath(dir)} {
		if _, err = os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy storage remained: %s", p)
		}
	}
	beforeKey := r.Status().MainKeyID
	if err = r.ChangePassword(firstPassword, secondPassword, secondPassword); err != nil {
		t.Fatal(err)
	}
	if r.Status().MainKeyID == beforeKey || r.Status().RotationPending {
		t.Fatal("MK transition incomplete")
	}
	after, err := loadLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	for key, header := range state.Envelopes {
		if bytes.Equal(header, after.Envelopes[key]) {
			t.Fatal("EDK was not rewrapped")
		}
	}
	for path, before := range bodies {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("MK rotation changed a body", err)
		}
		info, err := os.Stat(path)
		if err != nil || !os.SameFile(infos[path], info) {
			t.Fatal("MK rotation replaced an immutable body file", err)
		}
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.UnlockPassword(secondPassword); err != nil {
		t.Fatal(err)
	}
	if data, err := reopened.Read("two"); err != nil || string(data) != "second configuration" {
		t.Fatal("body lost after restart", err)
	}
}

func TestLocalStateMigratesLegacyEncryptedLayoutAfterProof(t *testing.T) {
	dir, err := prepareDataDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewID()
	k, err := newPasswordKeyring(store, []byte(firstPassword))
	if err != nil {
		t.Fatal(err)
	}
	defer k.Destroy()
	keys, err := k.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err = writeFileAtomic(filepath.Join(dir, "keys", "config.keys"), keys); err != nil {
		t.Fatal(err)
	}
	object, _ := NewID()
	generation, _ := NewID()
	catalogGen, _ := NewID()
	identity := Identity{StoreID: store, ObjectID: object, GenerationID: generation, Kind: KindProfile}
	envelope, err := k.Seal([]byte("legacy body"), identity)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, "ssh_configs", "old", filepath.Base(localEnvelopeKey(envelope.Bytes()))+".enc")
	if err = writeFileAtomic(legacyPath, envelope.Bytes()); err != nil {
		t.Fatal(err)
	}
	doc := catalogDocument{Version: repositoryVersion, StoreID: store[:], Revision: 1, Profiles: []catalogProfile{{Name: "old", ObjectID: object[:], GenerationID: generation[:]}}}
	raw, _ := json.Marshal(doc)
	catalog, err := k.Seal(raw, Identity{StoreID: store, ObjectID: catalogObjectID, GenerationID: catalogGen, Kind: KindCatalog})
	if err != nil {
		t.Fatal(err)
	}
	if err = writeFileAtomic(repositoryCatalogPath(dir), catalog.Bytes()); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = os.Stat(legacyPath); err != nil {
		t.Fatal("locked migration deleted source before authentication")
	}
	if err = r.UnlockPassword(firstPassword); err != nil {
		t.Fatal(err)
	}
	if data, err := r.Read("old"); err != nil || string(data) != "legacy body" {
		t.Fatal("migration lost profile", err)
	}
	if _, err = os.Stat(legacyPath); err != nil {
		t.Fatal("verified host ciphertext was moved out of its directory")
	}
	if err = os.WriteFile(LocalStatePath(dir), []byte("broken state"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if _, err = OpenRepositoryForRecovery(dir); err == nil {
		t.Fatal("corrupt state reopened initialization")
	}
}
