package configstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func newRotationRepository(t *testing.T) (*Repository, Provider) {
	t.Helper()
	t.Setenv(EnvKEKVariable, environmentKEK(0x73))
	provider := NewEnvProvider("")
	repo, err := InitializeRepository(t.TempDir(), provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.SetEncryptNew(true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "internal"} {
		if err := repo.Save(name, []byte("host = \"host.example\"\npassword = \"placeholder\"\n")); err != nil {
			t.Fatal(err)
		}
	}
	return repo, provider
}

func readRotationFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRepositoryRotatesMainKeyWithoutChangingDataCiphertext(t *testing.T) {
	repo, provider := newRotationRepository(t)
	before := repo.Status()
	originalRefs := cloneRefs(repo.refs)
	oldOnly, err := repo.keyring.activeOnly()
	if err != nil {
		t.Fatal(err)
	}
	defer oldOnly.Destroy()
	oldMain := oldOnly.mainKeys[before.MainKeyID]
	defer clear(oldMain[:])
	targets := repo.rotationTargets()
	original := make(map[string][]byte, len(targets))
	for _, target := range targets {
		original[target.path] = readRotationFile(t, target.path)
	}

	for round := 0; round < 2; round++ {
		if err := repo.RotateMainKey(provider); err != nil {
			t.Fatal(err)
		}
		after := repo.Status()
		if after.MainKeyID == before.MainKeyID || after.RotationPending ||
			after.Revision != before.Revision || after.StoreID != before.StoreID || !after.EncryptNew {
			t.Fatalf("unexpected rotation status: %+v", after)
		}
		if !reflect.DeepEqual(repo.refs, originalRefs) || len(repo.keyring.mainKeys) != 1 {
			t.Fatal("rotation changed profile identities or retained an old Main Key")
		}
		for _, target := range targets {
			raw := readRotationFile(t, target.path)
			old := original[target.path]
			if !bytes.Equal(raw[:dataHeaderSize], old[:dataHeaderSize]) ||
				!bytes.Equal(raw[envelopeHeaderSize:], old[envelopeHeaderSize:]) {
				t.Fatal("rotation changed the stable data header or body ciphertext")
			}
			envelope, err := Parse(raw, target.identity)
			if err != nil {
				t.Fatal(err)
			}
			if envelope.WrapKeyID() != after.MainKeyID {
				t.Fatal("an active object still depends on an old Main Key")
			}
			if _, err := oldOnly.Open(envelope); !errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("old Main Key can still open a new wrapper: %v", err)
			}
			if _, err := envelope.Open(oldMain); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("new wrapper still authenticates with the old Main Key: %v", err)
			}
		}
		before = after
	}
	dir := repo.DataDir()
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepository(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Status().MainKeyID != before.MainKeyID || len(reopened.keyring.mainKeys) != 1 {
		t.Fatal("restart did not retain the new Main Key exclusively")
	}
	for name := range originalRefs {
		data, err := reopened.Read(name)
		if err != nil || !bytes.Contains(data, []byte("placeholder")) {
			t.Fatalf("profile %s could not be read after restart: %v", name, err)
		}
	}
	if err := reopened.Save("alpha", []byte("host = \"changed.example\"\n")); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryRotationResumesEveryInterruptedPublication(t *testing.T) {
	// Three profiles, the catalog, and the initial/final keyring publications.
	const publications = 6
	for failAt := 1; failAt <= publications; failAt++ {
		for _, afterPublication := range []bool{false, true} {
			t.Run(fmt.Sprintf("write-%d/after-%v", failAt, afterPublication), func(t *testing.T) {
				repo, provider := newRotationRepository(t)
				oldID := repo.Status().MainKeyID
				injected := errors.New("injected publication failure")
				calls := 0
				err := repo.rotateMainKey([]Provider{provider}, func(path string, data []byte) error {
					calls++
					if calls == failAt && !afterPublication {
						return injected
					}
					if err := writeFileAtomic(path, data); err != nil {
						return err
					}
					if calls == failAt {
						return fmt.Errorf("%w: injected directory sync failure", ErrDurability)
					}
					return nil
				})
				if afterPublication && !errors.Is(err, ErrDurability) ||
					!afterPublication && !errors.Is(err, injected) {
					t.Fatalf("rotation error = %v", err)
				}
				interrupted := repo.Status()
				if interrupted.RotationPending {
					for _, mutate := range []func() error{
						func() error { return repo.Save("gamma", []byte("opaque")) },
						func() error { return repo.Import([]Item{{Name: "gamma", Data: []byte("opaque")}}) },
						func() error { return repo.Rename("alpha", "gamma") },
						func() error { return repo.Delete("alpha") },
						func() error { return repo.SetEncryptNew(false) },
					} {
						if err := mutate(); !errors.Is(err, ErrRotationPending) {
							t.Fatalf("mutation during pending rotation = %v", err)
						}
					}
				}
				dir := repo.DataDir()
				if err := repo.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenRepository(dir, provider)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				if state := reopened.Status(); state.MainKeyID != interrupted.MainKeyID ||
					state.RotationPending != interrupted.RotationPending {
					t.Fatal("restart did not retain the published keyring state")
				}
				if _, err := reopened.Read("alpha"); err != nil {
					t.Fatal(err)
				}
				if interrupted.RotationPending || interrupted.MainKeyID == oldID {
					if err := reopened.RotateMainKey(provider); err != nil {
						t.Fatal(err)
					}
				}
				finished := reopened.Status()
				if finished.RotationPending || finished.MainKeyID == oldID || len(reopened.keyring.mainKeys) != 1 {
					t.Fatal("rotation did not finish with exactly one new Main Key")
				}
				if interrupted.RotationPending && finished.MainKeyID != interrupted.MainKeyID {
					t.Fatal("resume generated another Main Key instead of finishing the pending rotation")
				}
				for _, target := range reopened.rotationTargets() {
					envelope, err := reopened.verifyRotationTarget(target)
					if err != nil || envelope.WrapKeyID() != finished.MainKeyID {
						t.Fatalf("incomplete wrapper after resume: %v", err)
					}
				}
			})
		}
	}
}

func TestRepositoryRotationRequiresAndPreservesEveryProviderSlot(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x74))
	provider := NewEnvProvider("")
	password, err := NewPasswordProvider([]byte("synthetic recovery password"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer password.Destroy()
	repo, err := InitializeRepository(t.TempDir(), provider, password)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	path := filepath.Join(repo.DataDir(), "keys", "config.keys")
	before := readRotationFile(t, path)
	if err := repo.RotateMainKey(provider); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("missing recovery provider = %v", err)
	}
	wrong, err := NewPasswordProvider([]byte("incorrect synthetic password"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Destroy()
	if err := repo.RotateMainKey(provider, wrong); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("incorrect recovery password = %v", err)
	}
	if !bytes.Equal(before, readRotationFile(t, path)) || repo.Status().RotationPending {
		t.Fatal("failed provider verification modified the keyring")
	}
	if err := repo.RotateMainKey(provider, password); err != nil {
		t.Fatal(err)
	}
	after := readRotationFile(t, path)
	oldStored, err := parseStoredKeyring(before)
	if err != nil {
		t.Fatal(err)
	}
	newStored, err := parseStoredKeyring(after)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldStored.Slots) != len(newStored.Slots) {
		t.Fatal("rotation removed an existing provider slot")
	}
	for i, slot := range oldStored.Slots {
		updated := newStored.Slots[i]
		if slot.Provider != updated.Provider || !bytes.Equal(slot.ID, updated.ID) ||
			!reflect.DeepEqual(slot.Parameters, updated.Parameters) {
			t.Fatal("rotation changed an existing provider reference or recovery parameters")
		}
	}
	for _, unlocker := range []Provider{provider, password} {
		unlocked, err := UnlockKeyring(after, unlocker)
		if err != nil {
			t.Fatal(err)
		}
		if !sameKeyring(unlocked, repo.keyring) {
			t.Fatal("an existing provider did not recover the new Main Key")
		}
		unlocked.Destroy()
	}
}

func TestRepositoryRotationRejectsTamperedDataBeforeChangingKeys(t *testing.T) {
	repo, provider := newRotationRepository(t)
	keyPath := filepath.Join(repo.DataDir(), "keys", "config.keys")
	before := readRotationFile(t, keyPath)
	path := repo.objectPath(repo.refs["beta"])
	data := readRotationFile(t, path)
	data[len(data)-1] ^= 0x80
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repo.RotateMainKey(provider); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered profile error = %v", err)
	}
	if !bytes.Equal(before, readRotationFile(t, keyPath)) || repo.Status().RotationPending {
		t.Fatal("rotation modified the keyring before verifying the existing data")
	}
}

func TestRepositoryRotationSerializesWithProfileWrites(t *testing.T) {
	repo, provider := newRotationRepository(t)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 4)
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := 0; iteration < 5; iteration++ {
				name := fmt.Sprintf("worker-%d", worker)
				if err := repo.Save(name, []byte("host = \"host.example\"\n")); err != nil {
					errorsCh <- err
					return
				}
				if _, err := repo.Read(name); err != nil {
					errorsCh <- err
					return
				}
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3; i++ {
			if err := repo.RotateMainKey(provider); err != nil {
				errorsCh <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	for worker := 0; worker < 3; worker++ {
		if _, err := repo.Read(fmt.Sprintf("worker-%d", worker)); err != nil {
			t.Fatal(err)
		}
	}
}
