package vault

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
)

const testPassphrase = "correct horse battery staple"

// fastScrypt drops the identity-file work factor for the duration of a test.
// The production default (18) costs ~1 s per Init/Unlock — fine against a
// brute-forcer, fatal for a test suite under -race on a loaded CI runner.
// Tests in this package run sequentially, so the restore in Cleanup is safe.
func fastScrypt(t *testing.T) {
	t.Helper()
	prev := scryptLogN
	scryptLogN = 10
	t.Cleanup(func() { scryptLogN = prev })
}

// initVault returns an initialized, unlocked vault in a per-test directory.
func initVault(t *testing.T) *Vault {
	t.Helper()
	fastScrypt(t)
	v := New(t.TempDir())
	if _, err := v.Init(testPassphrase); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return v
}

func TestRoundTrip(t *testing.T) {
	v := initVault(t)
	want := []byte("kind = \"remote\"\nhost = \"example.com\"\npassword = \"hunter2\"\n")
	if err := v.Encrypt("prod", want); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := v.Decrypt("prod")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip mismatch:\ngot  %q\nwant %q", got, want)
	}
	names, err := v.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "prod" {
		t.Fatalf("List = %v, want [prod]", names)
	}
}

func TestLockedVaultRefusesCrypto(t *testing.T) {
	v := New(t.TempDir()) // never Init'd, never unlocked
	if _, err := v.Decrypt("prod"); !errors.Is(err, ErrLocked) {
		t.Fatalf("Decrypt on locked vault: got %v, want ErrLocked", err)
	}
	if err := v.Encrypt("prod", []byte("x")); !errors.Is(err, ErrLocked) {
		t.Fatalf("Encrypt on locked vault: got %v, want ErrLocked", err)
	}
}

func TestUnlockWrongThenRightPassphrase(t *testing.T) {
	fastScrypt(t)
	dir := t.TempDir()
	v := New(dir)
	if _, err := v.Init(testPassphrase); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v.Lock()

	// A second Vault over the same data dir starts locked, like a restarted
	// process would.
	fresh := New(dir)
	if err := fresh.Unlock("wrong passphrase"); err == nil {
		t.Fatal("Unlock with wrong passphrase succeeded")
	}
	if fresh.IsUnlocked() {
		t.Fatal("vault reports unlocked after a failed Unlock")
	}
	if err := fresh.Unlock(testPassphrase); err != nil {
		t.Fatalf("Unlock with the right passphrase: %v", err)
	}
	if !fresh.IsUnlocked() {
		t.Fatal("vault still locked after a good Unlock")
	}
}

func TestUnlockWithoutInit(t *testing.T) {
	v := New(t.TempDir())
	if err := v.Unlock(testPassphrase); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Unlock without Init: got %v, want ErrNotInitialized", err)
	}
}

func TestInitRefusesToClobber(t *testing.T) {
	v := initVault(t)
	if _, err := v.Init(testPassphrase); err == nil {
		t.Fatal("second Init succeeded; existing identity would be orphaned")
	}
}

func TestMultiRecipientEitherDeviceDecrypts(t *testing.T) {
	v := initVault(t)

	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate second identity: %v", err)
	}
	if err := v.AddRecipient("laptop", other.Recipient().String(), "second device"); err != nil {
		t.Fatalf("AddRecipient: %v", err)
	}
	plaintext := []byte("password = \"shared across devices\"")
	if err := v.Encrypt("shared", plaintext); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// The second device opens the same file with its own identity — the
	// recipients-model property the whole design rests on.
	data, err := os.ReadFile(filepath.Join(v.Dir(), "ssh_configs", "shared.age"))
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), other)
	if err != nil {
		t.Fatalf("second identity cannot decrypt: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("second identity read: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("second device got %q, want %q", got, plaintext)
	}
}

func TestInvalidProfileNamesRejected(t *testing.T) {
	v := initVault(t)
	for _, name := range []string{"", "../x", "a/b", "a b", ".hidden", "a\\b"} {
		if err := v.Encrypt(name, []byte("x")); err == nil {
			t.Errorf("Encrypt(%q) succeeded", name)
		}
		if _, err := v.Decrypt(name); err == nil {
			t.Errorf("Decrypt(%q) succeeded", name)
		}
	}
}

func TestCorruptCiphertextFailsClosed(t *testing.T) {
	v := initVault(t)
	if err := v.Encrypt("prod", []byte("password = \"x\"")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	path := filepath.Join(v.Dir(), "ssh_configs", "prod.age")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	// Flip bytes in the payload region (past the age header).
	for i := len(data) - 16; i < len(data); i++ {
		data[i] ^= 0xff
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("corrupt ciphertext: %v", err)
	}
	if _, err := v.Decrypt("prod"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt of corrupt file: got %v, want ErrDecrypt", err)
	}
}

func TestDecryptMissingProfile(t *testing.T) {
	v := initVault(t)
	if _, err := v.Decrypt("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Decrypt of missing profile: got %v, want fs.ErrNotExist", err)
	}
}

// TestPlaintextNeverTouchesDisk walks the whole vault directory after an
// Encrypt and asserts the secret canary appears in no file — the I1 invariant
// checked at the byte level, not assumed from the design.
func TestPlaintextNeverTouchesDisk(t *testing.T) {
	v := initVault(t)
	canary := "CANARY-p4ssw0rd-never-on-disk"
	if err := v.Encrypt("prod", []byte("password = \""+canary+"\"")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	err := filepath.WalkDir(v.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(canary)) {
			t.Errorf("plaintext canary found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk vault: %v", err)
	}
}

func TestLockDropsAccess(t *testing.T) {
	v := initVault(t)
	if err := v.Encrypt("prod", []byte("x")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	v.Lock()
	if v.IsUnlocked() {
		t.Fatal("IsUnlocked after Lock")
	}
	if _, err := v.Decrypt("prod"); !errors.Is(err, ErrLocked) {
		t.Fatalf("Decrypt after Lock: got %v, want ErrLocked", err)
	}
	// Read-only listing keeps working on a locked vault (design doc §四).
	if names, err := v.List(); err != nil || len(names) != 1 {
		t.Fatalf("List on locked vault: %v, %v", names, err)
	}
}

func TestRecipientTableManagement(t *testing.T) {
	v := initVault(t)
	recs, err := v.Recipients()
	if err != nil || len(recs) != 1 || recs[0].Name != "self" {
		t.Fatalf("initial recipients: %v, %v", recs, err)
	}
	other, _ := age.GenerateX25519Identity()
	if err := v.AddRecipient("laptop", other.Recipient().String(), ""); err != nil {
		t.Fatalf("AddRecipient: %v", err)
	}
	if err := v.AddRecipient("laptop", other.Recipient().String(), ""); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := v.AddRecipient("bogus", "not-a-key", ""); err == nil {
		t.Fatal("malformed recipient key accepted")
	}
	if err := v.RemoveRecipient("laptop"); err != nil {
		t.Fatalf("RemoveRecipient: %v", err)
	}
	if err := v.RemoveRecipient("laptop"); !errors.Is(err, ErrRecipientNotFound) {
		t.Fatalf("second RemoveRecipient: got %v, want ErrRecipientNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	v := initVault(t)
	if err := v.Encrypt("prod", []byte("x")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := v.Delete("prod"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := v.Delete("prod"); err != nil {
		t.Fatalf("Delete of missing profile must be a no-op: %v", err)
	}
	if names, _ := v.List(); len(names) != 0 {
		t.Fatalf("List after Delete: %v", names)
	}
}

// TestConcurrentEncryptList is the -race smoke test: parallel writers on
// distinct profiles plus concurrent readers must not trip the detector or
// corrupt the directory.
func TestConcurrentEncryptList(t *testing.T) {
	v := initVault(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("profile-%d", i)
			if err := v.Encrypt(name, []byte("password = \"x\"")); err != nil {
				t.Errorf("Encrypt %s: %v", name, err)
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, err := v.List(); err != nil {
				t.Errorf("List: %v", err)
			}
		}()
	}
	wg.Wait()
	names, err := v.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 8 {
		t.Fatalf("List = %v, want 8 profiles", names)
	}
}

// TestFilePermissions pins the 0600 invariant on every file the vault writes.
func TestFilePermissions(t *testing.T) {
	v := initVault(t)
	if err := v.Encrypt("prod", []byte("x")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	err := filepath.WalkDir(v.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("%s mode %o, want 0600", path, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk vault: %v", err)
	}
}

// TestStringsDoesNotAppear is a cheap guard against the classic mistake of
// logging the passphrase into a file during refactors.
func TestPassphraseNotPersisted(t *testing.T) {
	v := initVault(t)
	err := filepath.WalkDir(v.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), testPassphrase) {
			t.Errorf("passphrase found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk vault: %v", err)
	}
}

func TestResealAllAddsANewDevice(t *testing.T) {
	v := initVault(t)
	if err := v.Encrypt("prod", []byte("password = \"before-reseal\"")); err != nil {
		t.Fatal(err)
	}

	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.AddRecipient("laptop", other.Recipient().String(), ""); err != nil {
		t.Fatal(err)
	}
	// Before reseal, the new device cannot open the old ciphertext.
	before, _ := os.ReadFile(filepath.Join(v.Dir(), "ssh_configs", "prod.age"))
	if _, err := age.Decrypt(bytes.NewReader(before), other); err == nil {
		t.Fatal("new device opened a file sealed before it was a recipient")
	}
	n, err := v.ResealAll()
	if err != nil {
		t.Fatalf("ResealAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("resealed %d, want 1", n)
	}
	after, _ := os.ReadFile(filepath.Join(v.Dir(), "ssh_configs", "prod.age"))
	r, err := age.Decrypt(bytes.NewReader(after), other)
	if err != nil {
		t.Fatalf("new device cannot open a resealed file: %v", err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "password = \"before-reseal\"" {
		t.Fatalf("reseal lost content: %q", got)
	}
}

func TestResealAllNeedsUnlock(t *testing.T) {
	v := initVault(t)
	v.Lock()
	if _, err := v.ResealAll(); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked ResealAll: got %v, want ErrLocked", err)
	}
}
