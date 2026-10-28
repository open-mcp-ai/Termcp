package configstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const firstPassword = "synthetic first password"
const secondPassword = "synthetic second password"

func passwordRepository(t *testing.T, providers ...Provider) *Repository {
	t.Helper()
	r, err := InitializeRepositoryWithPassword(t.TempDir(), firstPassword, firstPassword, providers...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Save("alpha", []byte("opaque configuration")); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPasswordDirectlyDerivesMainKeyAndUnlocksWithoutKEK(t *testing.T) {
	r := passwordRepository(t)
	path := filepath.Join(r.dataDir, "keys", "config.keys")
	data := readRotationFile(t, path)
	stored, err := parseStoredKeyring(data)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := deriveMain([]byte(firstPassword), stored.Password)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(derived[:])
	if r.keyring.mainKeys[r.keyring.activeKeyID] != derived || len(stored.Slots) != 0 {
		t.Fatal("Main Key is not the direct password derivation, or manual mode persisted a provider slot")
	}
	if bytes.Contains(data, []byte(firstPassword)) || bytes.Contains(data, derived[:]) {
		t.Fatal("a password or plaintext Main Key was persisted")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	locked, err := OpenRepositoryForRecovery(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	if state := locked.Status(); !state.Locked || !state.PasswordConfigured || state.ErrorCode != "password_required" {
		t.Fatalf("manual restart status = %+v", state)
	}
	if err := locked.UnlockPassword("incorrect password"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong password = %v", err)
	}
	if !bytes.Equal(data, readRotationFile(t, path)) {
		t.Fatal("wrong password rewrote key metadata")
	}
	if err := locked.UnlockPassword(firstPassword); err != nil {
		t.Fatal(err)
	}
	if locked.Status().Locked || locked.Status().AutomaticUnlock {
		t.Fatal("manual unlock did not remain independent from a KEK")
	}
	if !bytes.Equal(data, readRotationFile(t, path)) {
		t.Fatal("manual unlock changed the on-disk keyring")
	}
}

func TestPasswordChangeRewrapsEDKsAndRejectsOldPassword(t *testing.T) {
	r := passwordRepository(t)
	beforeID := r.Status().MainKeyID
	original := make(map[string][]byte)
	for _, target := range r.rotationTargets() {
		original[target.path] = readRotationFile(t, target.path)
	}
	path := filepath.Join(r.dataDir, "keys", "config.keys")
	before := readRotationFile(t, path)
	if err := r.ChangePassword("incorrect current password", secondPassword, secondPassword); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("incorrect current password = %v", err)
	}
	if err := r.ChangePassword(firstPassword, secondPassword, "different confirmation"); !errors.Is(err, ErrPasswordConfirmation) {
		t.Fatalf("confirmation error = %v", err)
	}
	if !bytes.Equal(before, readRotationFile(t, path)) {
		t.Fatal("a rejected password change modified the keyring")
	}
	if err := r.ChangePassword(firstPassword, secondPassword, secondPassword); err != nil {
		t.Fatal(err)
	}
	if state := r.Status(); state.MainKeyID == beforeID || state.RotationPending {
		t.Fatal("password change did not replace and finish the Main Key")
	}
	for path, old := range original {
		current := readRotationFile(t, path)
		if !bytes.Equal(current[:dataHeaderSize], old[:dataHeaderSize]) || !bytes.Equal(current[envelopeHeaderSize:], old[envelopeHeaderSize:]) {
			t.Fatal("password change re-encrypted a configuration body")
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	locked, err := OpenRepositoryForRecovery(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	if err := locked.UnlockPassword(firstPassword); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("old password = %v", err)
	}
	if err := locked.UnlockPassword(secondPassword); err != nil {
		t.Fatal(err)
	}
	if body, err := locked.Read("alpha"); err != nil || string(body) != "opaque configuration" {
		t.Fatalf("configuration after restart = %v", err)
	}
}

func TestPasswordChangeResumesUsingNewPasswordWithoutKEK(t *testing.T) {
	// One profile plus catalog, between two keyring publications.
	for failAt := 1; failAt <= 4; failAt++ {
		t.Run(fmt.Sprintf("publication-%d", failAt), func(t *testing.T) {
			r := passwordRepository(t)
			initialID := r.Status().MainKeyID
			injected := errors.New("injected directory-sync failure")
			writes := 0
			err := r.changePassword(firstPassword, secondPassword, secondPassword, nil, func(path string, data []byte) error {
				writes++
				if err := writeFileAtomic(path, data); err != nil {
					return err
				}
				if writes == failAt {
					return fmt.Errorf("%w: %v", ErrDurability, injected)
				}
				return nil
			})
			if !errors.Is(err, ErrDurability) {
				t.Fatalf("interruption error = %v", err)
			}
			published := r.Status().MainKeyID
			if published == initialID {
				t.Fatal("the new derived key was not published")
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			locked, err := OpenRepositoryForRecovery(r.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = locked.Close() })
			if err := locked.UnlockPassword(secondPassword); err != nil {
				t.Fatal(err)
			}
			if locked.Status().RotationPending {
				if err := locked.ChangePassword(secondPassword, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			if state := locked.Status(); state.RotationPending || state.MainKeyID != published {
				t.Fatal("resume changed the selected derived key")
			}
			if _, err := locked.Read("alpha"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPasswordAutoUnlockAndProviderRecovery(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x51))
	p := NewEnvProvider("")
	r := passwordRepository(t, p)
	keyID := r.Status().MainKeyID
	if !r.Status().AutomaticUnlock {
		t.Fatal("available ENV KEK was not recorded")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	ready, err := OpenRepository(r.dataDir, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := ready.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKEKVariable, environmentKEK(0x52))
	locked, err := OpenRepositoryForRecovery(r.dataDir, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	if err := locked.UnlockPassword(firstPassword); err != nil {
		t.Fatal(err)
	}
	if err := locked.RebindPasswordProvider(p); err != nil {
		t.Fatal(err)
	}
	if locked.Status().MainKeyID != keyID {
		t.Fatal("provider recovery changed the derived Main Key")
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRepository(r.dataDir, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
}

func TestLegacyStoreCanSetItsFirstPassword(t *testing.T) {
	r, p := newRotationRepository(t)
	if err := r.ChangePassword("", firstPassword, firstPassword, p); err != nil {
		t.Fatal(err)
	}
	if !r.Status().PasswordConfigured {
		t.Fatal("legacy store did not acquire password derivation metadata")
	}
	for _, name := range []string{"alpha", "beta", "internal"} {
		if _, err := r.Read(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPasswordRejectsInvalidDerivationBeforeKDF(t *testing.T) {
	r := passwordRepository(t)
	path := filepath.Join(r.dataDir, "keys", "config.keys")
	data := readRotationFile(t, path)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Altering the fixed cost is rejected structurally before allocating memory.
	data = bytes.Replace(data, []byte("\"memory_kib\":65536"), []byte("\"memory_kib\":4294967295"), 1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenRepositoryForRecovery(r.dataDir); opened != nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("unbounded KDF metadata = %v", err)
	}
}
