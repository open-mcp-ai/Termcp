//go:build darwin

package vault

import (
	"os"
	"strings"
)

// darwinSecretStore uses the login Keychain through /usr/bin/security.
// Storing runs security's interactive mode (-i) so the command — which
// carries the secret — travels on stdin, never in argv. The OS may show an
// access prompt the first time.
type darwinSecretStore struct{}

func newPlatformSecretStore() secretStore { return darwinSecretStore{} }

func (darwinSecretStore) Available() bool {
	// security ships with macOS at a fixed path; its absence means something
	// is deeply unusual, so a plain stat is the whole probe.
	_, err := os.Stat("/usr/bin/security")
	return err == nil
}

func (darwinSecretStore) Store(_ *Vault, ref, secret string) error {
	// -U updates an existing item with the same service/account pair, which
	// keeps a re-Init from failing on the leftover entry.
	command := "add-generic-password -U -s " + keychainService + " -a " + ref + " -w " + secret + "\n"
	out, err := credentialCommand("/usr/bin/security", []string{"-i"}, strings.NewReader(command))
	clear(out)
	if err != nil {
		return err
	}
	// security -i exits 0 even when the subcommand failed; read the item
	// back before letting the vault reference it.
	got, err := darwinSecretStore{}.Load(nil, ref)
	if err != nil || got != secret {
		return ErrKeychainUnavailable
	}
	return nil
}

func (darwinSecretStore) Load(_ *Vault, ref string) (string, error) {
	out, err := credentialCommand("/usr/bin/security",
		[]string{"find-generic-password", "-s", keychainService, "-a", ref, "-w"}, nil)
	defer clear(out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
