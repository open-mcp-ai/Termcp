package configstore

import (
	"errors"
	"runtime"
	"testing"
)

/* The keychain providers are thin shells around a credential store: the shell
 * decides what a slot looks like, and the store is whatever the platform offers.
 * The shell is the part that must be right on every platform, so it is tested
 * with a fake store here — a real Keychain is only available on a Mac and a real
 * Secret Service only behind a D-Bus session, and neither exists on the machine
 * that builds this repository's CI.
 *
 * What this proves: a slot written by a credential provider unwraps to the same
 * KEK, a reference is never the login name or a path, and a store that cannot
 * answer reports ErrProviderUnavailable rather than inventing a key.
 */
type fakeCredentialStore struct {
	secrets map[string]string
	fail    bool
}

func (f *fakeCredentialStore) store(reference, secret string) error {
	if f.fail {
		return errors.New("synthetic store failure")
	}
	if f.secrets == nil {
		f.secrets = map[string]string{}
	}
	f.secrets[reference] = secret
	return nil
}

func (f *fakeCredentialStore) load(reference string) (string, error) {
	if f.fail {
		return "", errors.New("synthetic store failure")
	}
	secret, ok := f.secrets[reference]
	if !ok {
		return "", errors.New("synthetic missing reference")
	}
	return secret, nil
}

func TestCredentialProviderRoundTripsThroughAStore(t *testing.T) {
	store := &fakeCredentialStore{}
	provider := credentialProvider{name: "synthetic", store: store}

	parameters, err := provider.newParameters()
	if err != nil {
		t.Fatalf("newParameters: %v", err)
	}
	if len(parameters.Reference) != 32 {
		t.Fatalf("reference = %q, want 32 hex characters", parameters.Reference)
	}
	if len(parameters.Protected) != 0 || len(parameters.Salt) != 0 || parameters.Argon2 != nil {
		t.Fatalf("credential slot carries unrelated fields: %+v", parameters)
	}

	key, err := provider.unwrapKey(parameters)
	if err != nil {
		t.Fatalf("unwrapKey: %v", err)
	}
	if isZeroBytes(key[:]) {
		t.Fatal("unwrapKey produced an all-zero KEK")
	}

	/* A different store cannot resolve the reference: that is the "this is not the
	   machine that wrote it" case, and it must be unavailable, not a wrong key. */
	other := credentialProvider{name: "synthetic", store: &fakeCredentialStore{}}
	if _, err := other.unwrapKey(parameters); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("foreign store error = %v, want ErrProviderUnavailable", err)
	}

	/* A store that cannot write must refuse to create a slot rather than write a
	   keyring nobody can open. */
	broken := credentialProvider{name: "synthetic", store: &fakeCredentialStore{fail: true}}
	if _, err := broken.newParameters(); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("broken store error = %v, want ErrProviderUnavailable", err)
	}
}

/* The real thing, where a real one can exist. Skipped elsewhere: a Linux CI
 * runner has no D-Bus session, and the Windows build has DPAPI instead. */
func TestSystemProviderRoundTripsOnThisHost(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no Keychain/Secret Service on " + runtime.GOOS)
	}
	provider := SystemProvider()
	parameters, err := provider.newParameters()
	if errors.Is(err, ErrProviderUnavailable) {
		t.Skip("this host has no usable credential store: " + provider.providerName())
	}
	if err != nil {
		t.Fatalf("newParameters: %v", err)
	}
	key, err := provider.unwrapKey(parameters)
	if err != nil {
		t.Fatalf("unwrapKey: %v", err)
	}
	if isZeroBytes(key[:]) {
		t.Fatal("system provider produced an all-zero KEK")
	}
}
