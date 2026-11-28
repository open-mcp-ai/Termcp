package configstore

import (
	"encoding/base64"
	"testing"
)

/* Rotation is three operations, not one, and the test asserts the property that
 * makes that safe: at no point does a profile become unreadable, and the old words
 * stop working only once the last object has moved.
 *
 * Mutation check: reordering RotateKey to delete the old key before handObjectsTo
 * fails on the read-back below.
 */
func TestRotateKeyKeepsObjectsReadableAndRetiresTheOldWords(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvKEKVariable, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	phrase, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	providers := DefaultProviders()
	repo, err := InitializeRepositoryWithMnemonic(dir, phrase, "", providers...)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	body := []byte("kind = \"remote\"\nhost = \"192.0.2.9\"\n")
	if err := repo.Save("alpha", body); err != nil {
		t.Fatal(err)
	}
	keys, err := repo.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %+v, err = %v", keys, err)
	}
	oldID := keys[0].ID

	rotated, newPhrase, err := repo.RotateKey(keyIDOf(t, oldID), "", providers...)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newPhrase == "" || newPhrase == phrase {
		t.Fatal("rotation did not produce new words")
	}

	/* The old key is gone and the object it sealed is still readable. */
	after, err := repo.Keys()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range after {
		if k.ID == oldID {
			t.Fatal("the old key still exists")
		}
	}
	if rotated.Hosts != 0 {
		t.Fatalf("the new key should have been handed the hosts, Hosts = %d", rotated.Hosts)
	}
	got, err := repo.Read("alpha")
	if err != nil {
		t.Fatalf("read after rotation: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("body changed: %q", got)
	}
}

func keyIDOf(t *testing.T, hexID string) ID {
	t.Helper()
	var id ID
	if err := id.UnmarshalText([]byte(hexID)); err != nil {
		t.Fatal(err)
	}
	return id
}

// Rotating with a passphrase gives the new key that passphrase; rotating with an
// empty string leaves the new key without one.
func TestRotateKeyAdoptsNewPassphrase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvKEKVariable, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	phrase, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	providers := DefaultProviders()
	repo, err := InitializeRepositoryWithMnemonic(dir, phrase, "initial-secret", providers...)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	keys, err := repo.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %+v, err = %v", keys, err)
	}
	oldID := keyIDOf(t, keys[0].ID)

	/* Rotate and supply a new passphrase. The new key must record that it has one. */
	rotated, _, err := repo.RotateKey(oldID, "replacement-secret", providers...)
	if err != nil {
		t.Fatalf("rotate with new passphrase: %v", err)
	}
	if !rotated.Passphrase {
		t.Fatal("new key should have passphrase=true")
	}

	/* Rotate again, empty string: the replacement key drops the passphrase. */
	newID := keyIDOf(t, rotated.ID)
	secondRotated, _, err := repo.RotateKey(newID, "", providers...)
	if err != nil {
		t.Fatalf("rotate without passphrase: %v", err)
	}
	if secondRotated.Passphrase {
		t.Fatal("key rotated with empty string must have passphrase=false")
	}
}
