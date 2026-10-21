package vault

import (
	"errors"
	"sync"
	"testing"
)

// fakeStore is an in-memory secretStore double. The orchestration logic
// (ref file, fallback order, disable switch) is platform-independent, so the
// tests exercise it against this double; the real CLIs are covered by the
// availability probe tests below and by CI on their own OS.
type fakeStore struct {
	mu    sync.Mutex
	items map[string]string
	up    bool
}

func newFakeStore() *fakeStore { return &fakeStore{items: map[string]string{}, up: true} }

func (f *fakeStore) Available() bool { return f.up }

func (f *fakeStore) Store(_ *Vault, ref, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return errors.New("store is down")
	}
	f.items[ref] = secret
	return nil
}

func (f *fakeStore) Load(_ *Vault, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.items[ref]
	if !ok {
		return "", errors.New("item not found")
	}
	return s, nil
}

// withFakeStore swaps the platform store for the test's duration. Tests in
// this package run sequentially, so the restore in Cleanup is safe.
func withFakeStore(t *testing.T, s secretStore) {
	t.Helper()
	prev := platformSecretStore
	platformSecretStore = s
	t.Cleanup(func() { platformSecretStore = prev })
}

func TestInitMirrorsIdentityIntoKeychain(t *testing.T) {
	fastScrypt(t)
	fake := newFakeStore()
	withFakeStore(t, fake)

	v := New(t.TempDir())
	if _, err := v.Init(testPassphrase); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !v.CanAutoUnlock() {
		t.Fatal("CanAutoUnlock false after Init with a working store")
	}
	if len(fake.items) != 1 {
		t.Fatalf("keychain holds %d items, want 1", len(fake.items))
	}
}

func TestUnlockAutoRoundTrip(t *testing.T) {
	fastScrypt(t)
	fake := newFakeStore()
	withFakeStore(t, fake)

	dir := t.TempDir()
	v := New(dir)
	if _, err := v.Init(testPassphrase); err != nil {
		t.Fatalf("Init: %v", err)
	}
	want := []byte("password = \"from-auto-unlock\"")
	if err := v.Encrypt("prod", want); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// A restarted process: fresh Vault, locked, no passphrase needed.
	fresh := New(dir)
	if fresh.IsUnlocked() {
		t.Fatal("fresh vault unexpectedly unlocked")
	}
	if err := fresh.UnlockAuto(); err != nil {
		t.Fatalf("UnlockAuto: %v", err)
	}
	got, err := fresh.Decrypt("prod")
	if err != nil {
		t.Fatalf("Decrypt after UnlockAuto: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round trip mismatch: got %q", got)
	}
}

func TestKeychainDisabledByEnv(t *testing.T) {
	fastScrypt(t)
	fake := newFakeStore()
	withFakeStore(t, fake)
	t.Setenv(noKeychainEnv, "1")

	dir := t.TempDir()
	v := New(dir)
	if _, err := v.Init(testPassphrase); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if v.CanAutoUnlock() {
		t.Fatal("CanAutoUnlock true with the env kill switch set")
	}
	if len(fake.items) != 0 {
		t.Fatal("keychain received an item while disabled")
	}
	v.Lock()
	if err := v.UnlockAuto(); !errors.Is(err, ErrKeychainUnavailable) {
		t.Fatalf("UnlockAuto while disabled: got %v, want ErrKeychainUnavailable", err)
	}
	// The passphrase path is untouched by the keychain switch.
	if err := v.Unlock(testPassphrase); err != nil {
		t.Fatalf("passphrase Unlock: %v", err)
	}
}

func TestKeychainDownFallsBackToPassphrase(t *testing.T) {
	fastScrypt(t)
	fake := newFakeStore()
	fake.up = false // a present-but-unreachable store, like headless D-Bus
	withFakeStore(t, fake)

	dir := t.TempDir()
	v := New(dir)
	// Init must not fail: keychain is a convenience, not a requirement.
	if _, err := v.Init(testPassphrase); err != nil {
		t.Fatalf("Init with store down: %v", err)
	}
	if v.CanAutoUnlock() {
		t.Fatal("CanAutoUnlock true with the store down")
	}
	v.Lock()
	if err := v.UnlockAuto(); !errors.Is(err, ErrKeychainUnavailable) {
		t.Fatalf("UnlockAuto with store down: got %v, want ErrKeychainUnavailable", err)
	}
	if err := v.Unlock(testPassphrase); err != nil {
		t.Fatalf("passphrase Unlock: %v", err)
	}
}

// TestRealStoreProbeNeverPanics runs the platform store's Available probe
// once. It asserts nothing about the answer — CI runners range from full
// desktops to headless containers — only that the probe is safe to call.
func TestRealStoreProbeNeverPanics(t *testing.T) {
	_ = platformSecretStore.Available()
}
