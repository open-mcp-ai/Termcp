package configstore

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func userMainKeyForTest() (MainKey, string) {
	var main MainKey
	copy(main[:], bytes.Repeat([]byte{0x54}, 32))
	return main, base64.StdEncoding.EncodeToString(main[:])
}

func TestRecoveryWithUserMainKeyAndBackupPreservesCiphertext(t *testing.T) {
	for _, useBackup := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw-main-key", true: "backup-code"}[useBackup], func(t *testing.T) {
			t.Setenv(EnvKEKVariable, environmentKEK(0x21))
			main, rawMain := userMainKeyForTest()
			defer clear(main[:])
			dir := t.TempDir()
			provider := NewEnvProvider("")
			repo, err := InitializeRepositoryWithMainKey(dir, main, provider)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			if err := repo.SetEncryptNew(true); err != nil {
				t.Fatal(err)
			}
			body := []byte("password = \"placeholder\"\n")
			if err := repo.Save("alpha", body); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(dir, "keys", "config.keys")
			originalKeys := readRotationFile(t, keyPath)
			before := repo.Status()
			original := make(map[string][]byte)
			for _, target := range repo.rotationTargets() {
				original[target.path] = readRotationFile(t, target.path)
			}
			backup, err := exportLegacyTestRecoveryKey(repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			t.Setenv(EnvKEKVariable, environmentKEK(0x22))
			if _, err := OpenRepository(dir, provider); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("strict open with wrong KEK = %v", err)
			}
			locked, err := OpenRepositoryForRecovery(dir, provider)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = locked.Close() })
			if status := locked.Status(); !status.Locked || status.ErrorCode != "keyring_authentication_failed" {
				t.Fatalf("locked status = %+v", status)
			}
			if _, err := locked.List(); !errors.Is(err, ErrLocked) {
				t.Fatalf("locked list = %v", err)
			}
			if err := locked.Save("other", body); !errors.Is(err, ErrLocked) {
				t.Fatalf("locked save = %v", err)
			}
			if _, err := OpenRepositoryForRecovery(dir, provider); err == nil {
				t.Fatal("a locked repository released its exclusive directory lock")
			}
			wrong := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x56}, 32))
			if err := locked.RecoverMainKey(wrong, nil, provider); !errors.Is(err, ErrRecoveryKey) {
				t.Fatalf("wrong recovery key = %v", err)
			}
			if !bytes.Equal(originalKeys, readRotationFile(t, keyPath)) || !locked.Status().Locked {
				t.Fatal("a wrong Main Key replaced existing key protection")
			}
			input := rawMain
			if useBackup {
				input = backup
			}
			if err := locked.RecoverMainKey(input, nil, provider); err != nil {
				t.Fatal(err)
			}
			after := locked.Status()
			if after.Locked || !after.EncryptNew || after.MainKeyID != before.MainKeyID || after.Revision != before.Revision {
				t.Fatalf("recovered status = %+v", after)
			}
			for path, expected := range original {
				if !bytes.Equal(expected, readRotationFile(t, path)) {
					t.Fatal("recovery rewrote a profile or the authenticated catalog")
				}
			}
			if bytes.Equal(originalKeys, readRotationFile(t, keyPath)) {
				t.Fatal("recovery did not replace the failed provider wrapper")
			}
			if err := locked.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenRepository(dir, provider)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if data, err := reopened.Read("alpha"); err != nil || !bytes.Equal(data, body) {
				t.Fatalf("restarted profile = %v", err)
			}
		})
	}
}

type recoveryCredentials struct{ entries map[string]string }

func (s *recoveryCredentials) store(reference, secret string) error {
	s.entries[reference] = secret
	return nil
}

func (s *recoveryCredentials) load(reference string) (string, error) {
	secret, ok := s.entries[reference]
	if !ok {
		return "", errors.New("credential unavailable")
	}
	return secret, nil
}

func TestRecoveryRecreatesLostSystemCredential(t *testing.T) {
	credentials := &recoveryCredentials{entries: map[string]string{}}
	provider := credentialProvider{name: "keychain", store: credentials}
	main, rawMain := userMainKeyForTest()
	defer clear(main[:])
	dir := t.TempDir()
	repo, err := InitializeRepositoryWithMainKey(dir, main, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Save("alpha", []byte("opaque configuration")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	clear(credentials.entries)
	locked, err := OpenRepositoryForRecovery(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	if locked.Status().ErrorCode != "key_provider_unavailable" {
		t.Fatal("lost system credential was not identified")
	}
	if err := locked.RecoverMainKey(rawMain, nil, provider); err != nil {
		t.Fatal(err)
	}
	if len(credentials.entries) != 1 || locked.Status().Locked {
		t.Fatal("system credential was not recreated")
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepository(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
}

func TestRecoveryNeedsAllKeysDuringInterruptedRotation(t *testing.T) {
	repo, provider := newRotationRepository(t)
	_, keys, activeID, err := repo.keyring.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	oldMain := keys[activeID]
	oldRaw := base64.StdEncoding.EncodeToString(oldMain[:])
	clear(oldMain[:])
	clearMainKeyMap(keys)
	writes := 0
	injected := errors.New("injected failure")
	if err := repo.rotateMainKey([]Provider{provider}, func(path string, data []byte) error {
		writes++
		if writes == 3 {
			return injected
		}
		return writeFileAtomic(path, data)
	}); !errors.Is(err, injected) {
		t.Fatalf("interrupted rotation = %v", err)
	}
	backup, err := exportLegacyTestRecoveryKey(repo)
	if err != nil {
		t.Fatal(err)
	}
	dir := repo.DataDir()
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKEKVariable, environmentKEK(0x23))
	locked, err := OpenRepositoryForRecovery(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	if err := locked.RecoverMainKey(oldRaw, nil, provider); !errors.Is(err, ErrRecoveryKey) {
		t.Fatalf("incomplete key recovery = %v", err)
	}
	if err := locked.RecoverMainKey(backup, nil, provider); err != nil {
		t.Fatal(err)
	}
	if !locked.Status().RotationPending {
		t.Fatal("recovery dropped the pending rotation key")
	}
	if err := locked.RotateMainKey(provider); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryValidatesBeforePublishingAndTracksVisibleWrites(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "publication-failed", true: "directory-sync-failed"}[published], func(t *testing.T) {
			repo, provider := newRotationRepository(t)
			backup, err := exportLegacyTestRecoveryKey(repo)
			if err != nil {
				t.Fatal(err)
			}
			dir := repo.DataDir()
			keyPath := filepath.Join(dir, "keys", "config.keys")
			originalKeys := readRotationFile(t, keyPath)
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			t.Setenv(EnvKEKVariable, environmentKEK(0x24))
			locked, err := OpenRepositoryForRecovery(dir, provider)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = locked.Close() })
			validator := func(string, []byte) error { return errors.New("invalid profile") }
			if err := locked.RecoverMainKey(backup, validator, provider); !errors.Is(err, ErrRecoveryKey) {
				t.Fatalf("business validation error = %v", err)
			}
			if !bytes.Equal(originalKeys, readRotationFile(t, keyPath)) {
				t.Fatal("recovery published before profile validation")
			}
			injected := errors.New("injected publication failure")
			err = locked.recoverMainKey(backup, nil, []Provider{provider}, func(path string, data []byte) error {
				if published {
					if err := writeFileAtomic(path, data); err != nil {
						return err
					}
					return ErrDurability
				}
				return injected
			})
			if published {
				if !errors.Is(err, ErrDurability) || locked.Status().Locked {
					t.Fatalf("published recovery state = %v", err)
				}
			} else if !errors.Is(err, injected) || !locked.Status().Locked || !bytes.Equal(originalKeys, readRotationFile(t, keyPath)) {
				t.Fatal("failed publication changed existing key protection")
			}
		})
	}
}

func TestDamagedCatalogDoesNotBecomeProviderRecovery(t *testing.T) {
	repo, provider := newRotationRepository(t)
	dir := repo.DataDir()
	path := repositoryCatalogPath(dir)
	raw := readRotationFile(t, path)
	raw[len(raw)-1] ^= 0x80
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenRepositoryForRecovery(dir, provider); opened != nil || !errors.Is(err, ErrAuthentication) {
		t.Fatalf("damaged catalog was accepted as a recoverable provider: %v", err)
	}
}

// This test fixture exercises legacy decoder compatibility; no export API is shipped.
func exportLegacyTestRecoveryKey(r *Repository) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := r.keyring.marshalPayload()
	if err != nil {
		return "", err
	}
	defer clear(data)
	return recoveryKeyPrefix + base64.RawURLEncoding.EncodeToString(data), nil
}
