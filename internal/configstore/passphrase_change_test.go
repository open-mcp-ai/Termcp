package configstore

import (
	"errors"
	"testing"
)

/* Changing a passphrase rotates the second layer only: the words, the key's id and
 * its address stay, and every host sealed by that key is re-sealed with a new salt.
 * These tests are about what survives that, because "same key, different
 * passphrase" is exactly the case where a wrong answer looks like a working one:
 * the old passphrase still opening the hosts, or the new one opening only the
 * hosts that happened to be re-sealed first.
 */

func TestChangingThePassphraseKeepsTheKeyAndReSealsEveryHost(t *testing.T) {
	const next = "a different synthetic passphrase"
	repo, keyID, dir := passphraseRepository(t, 0xc1, "alpha", "host = \"a.example\"\n")
	if err := repo.SaveWithOptions("beta", []byte("host = \"b.example\"\n"),
		SaveOptions{KeyID: keyID, Passphrase: testPassphrase}); err != nil {
		t.Fatalf("second host: %v", err)
	}
	before, err := repo.Keys()
	if err != nil || len(before) != 1 {
		t.Fatalf("keychain = %+v, %v", before, err)
	}

	if err := repo.ChangePassphrase(keyID, testPassphrase, next); err != nil {
		t.Fatalf("change the passphrase: %v", err)
	}
	after, err := repo.Keys()
	if err != nil || len(after) != 1 {
		t.Fatalf("keychain after = %+v, %v", after, err)
	}
	if after[0].ID != before[0].ID || after[0].PublicKey != before[0].PublicKey {
		t.Fatalf("the key changed identity: %q/%q became %q/%q",
			before[0].ID, before[0].PublicKey, after[0].ID, after[0].PublicKey)
	}
	if !after[0].Passphrase {
		t.Fatal("the key lost its passphrase flag, so its hosts read as un-gated")
	}

	/* Reopen: the point is what is on disk, not what this process happens to hold. */
	_ = repo.Close()
	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Read("alpha"); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("a restarted host read without activating: %v", err)
	}
	if _, err := reopened.Activate([]string{"alpha", "beta"}, map[ID]string{keyID: next}); err != nil {
		t.Fatalf("activate with the new passphrase: %v", err)
	}
	for name, want := range map[string]string{
		"alpha": "host = \"a.example\"\n",
		"beta":  "host = \"b.example\"\n",
	} {
		got, err := reopened.Read(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestTheOldPassphraseStopsWorking(t *testing.T) {
	const next = "a different synthetic passphrase"
	repo, keyID, dir := passphraseRepository(t, 0xc2, "alpha", "host = \"a.example\"\n")
	if err := repo.ChangePassphrase(keyID, testPassphrase, next); err != nil {
		t.Fatalf("change the passphrase: %v", err)
	}
	_ = repo.Close()
	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Activate([]string{"alpha"}, map[ID]string{keyID: testPassphrase}); !errors.Is(err, ErrPassphraseWrong) {
		t.Fatalf("the old passphrase still activates the host: %v", err)
	}
}

/* A wrong passphrase is the only failure that can be caught before any write, and
 * catching it there is what makes the action safe to get wrong. */
func TestAWrongPassphraseChangesNothing(t *testing.T) {
	repo, keyID, dir := passphraseRepository(t, 0xc3, "alpha", "host = \"a.example\"\n")
	if err := repo.ChangePassphrase(keyID, "not the passphrase", "another synthetic one"); !errors.Is(err, ErrPassphraseWrong) {
		t.Fatalf("a wrong passphrase was accepted: %v", err)
	}
	_ = repo.Close()
	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Activate([]string{"alpha"}, map[ID]string{keyID: testPassphrase}); err != nil {
		t.Fatalf("the unchanged passphrase no longer works: %v", err)
	}
	if _, err := reopened.Read("alpha"); err != nil {
		t.Fatal(err)
	}
}

/* Removing the passphrase is the same rotation with an empty replacement: the hosts
 * become gated by the keychain's boolean again, which means an `active` marker
 * instead of an `.adk`, and no passphrase at all after a restart. */
func TestRemovingThePassphraseLeavesTheHostsReadable(t *testing.T) {
	repo, keyID, dir := passphraseRepository(t, 0xc4, "alpha", "host = \"a.example\"\n")
	if err := repo.ChangePassphrase(keyID, testPassphrase, ""); err != nil {
		t.Fatalf("remove the passphrase: %v", err)
	}
	keys, err := repo.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keychain = %+v, %v", keys, err)
	}
	if keys[0].Passphrase {
		t.Fatal("the key still says its hosts need a passphrase")
	}
	_ = repo.Close()
	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.Read("alpha")
	if err != nil {
		t.Fatalf("a host with no passphrase is not readable after a restart: %v", err)
	}
	if string(got) != "host = \"a.example\"\n" {
		t.Errorf("alpha = %q", got)
	}
}
