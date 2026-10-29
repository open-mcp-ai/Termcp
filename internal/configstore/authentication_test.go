package configstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticationSharesMainKeyAndRotation(t *testing.T) {
	dir := t.TempDir()
	r, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	payload := []byte("synthetic authentication payload")
	if err := r.WriteAuthentication(payload); err != nil {
		t.Fatal(err)
	}
	if err := r.Save("host", []byte("profile payload")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(authenticationPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, payload) {
		t.Fatal("authentication is plaintext")
	}
	if _, err := parseCatalogEnvelope(before, r.keyring.StoreID(), catalogObjectID); err == nil {
		t.Fatal("auth object accepted as catalog")
	}
	next := "new-master-password-for-tests"
	if err := r.ChangePassword(firstPassword, next, next); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(authenticationPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) || !bytes.Equal(before[envelopeHeaderSize:], after[envelopeHeaderSize:]) {
		t.Fatal("rotation should change only the auth DEK wrapper")
	}
	if err := r.VerifyPassword(firstPassword); err == nil {
		t.Fatal("old password accepted")
	}
	if err := r.VerifyPassword(next); err != nil {
		t.Fatal(err)
	}
	if data, err := r.ReadAuthentication(); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("authentication lost: %v", err)
	}
	if data, err := r.Read("host"); err != nil || string(data) != "profile payload" {
		t.Fatalf("host lost: %v", err)
	}
}

func TestAuthenticationRewrapResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	r, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.WriteAuthentication([]byte("authentication before rotation")); err != nil {
		t.Fatal(err)
	}
	next := "replacement-master-password"
	failure := errors.New("simulated auth publication interruption")
	err = r.changePassword(firstPassword, next, next, nil, func(path string, data []byte) error {
		if path == authenticationPath(r.dataDir) {
			return failure
		}
		return writeFileAtomic(path, data)
	})
	if !errors.Is(err, failure) || !r.Status().RotationPending {
		t.Fatalf("missing resumable transition: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRepositoryForRecovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UnlockPassword(firstPassword); err == nil {
		t.Fatal("old password unlocked pending transition")
	}
	if err := r.UnlockPassword(next); err != nil {
		t.Fatal(err)
	}
	if err := r.ChangePassword(next, "", ""); err != nil {
		t.Fatal(err)
	}
	if r.Status().RotationPending {
		t.Fatal("rotation did not finish")
	}
	if data, err := r.ReadAuthentication(); err != nil || string(data) != "authentication before rotation" {
		t.Fatalf("lost auth after resume: %v", err)
	}
}

func TestPasswordInitializationResumesOnlyRecordedPartialCreation(t *testing.T) {
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	k, err := newPasswordKeyring(id, []byte(firstPassword))
	if err != nil {
		t.Fatal(err)
	}
	defer k.Destroy()
	raw, err := k.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "keys", "config.keys"), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatal("recreated an unrecorded missing catalog", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "keys", "config.initializing"), id[:]); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(dir, "ssh_configs", "retained-host", "retained.enc")
	if err := writeFileAtomic(profilePath, []byte("retained ciphertext")); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatal("stale marker replaced a profile catalog", err)
	}
	if err := os.Remove(profilePath); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeRepositoryWithPassword(dir, "incorrect-password", "incorrect-password"); err == nil {
		t.Fatal("partial initialization accepted wrong password")
	}
	r, err := InitializeRepositoryWithPassword(dir, firstPassword, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.keyring.StoreID() != id {
		t.Fatal("partial creation replaced the keyring")
	}
	if _, err := os.Stat(filepath.Join(dir, "keys", "config.initializing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("initialization marker retained", err)
	}
}
