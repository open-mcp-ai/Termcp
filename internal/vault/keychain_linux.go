//go:build linux

package vault

import (
	"os/exec"
	"strings"
)

// linuxSecretStore uses libsecret's secret-tool, the CLI front of the
// desktop Secret Service (gnome-keyring, KWallet). On a headless box the
// binary or the D-Bus service is missing and Available says so — the vault
// then stays on the passphrase path, which is the designed fallback.
type linuxSecretStore struct{}

func newPlatformSecretStore() secretStore { return linuxSecretStore{} }

func (l linuxSecretStore) Available() bool {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return false
	}
	// A present binary is not enough on a headless system: probe the service
	// with a read of an attribute that will simply not match.
	_, err := credentialCommand("secret-tool",
		[]string{"lookup", "service", keychainService, "vault", "availability-probe"}, nil)
	return err == nil
}

func (linuxSecretStore) Store(_ *Vault, ref, secret string) error {
	out, err := credentialCommand("secret-tool",
		[]string{"store", "--label=Termcp vault identity", "service", keychainService, "vault", ref},
		strings.NewReader(secret))
	clear(out)
	return err
}

func (linuxSecretStore) Load(_ *Vault, ref string) (string, error) {
	out, err := credentialCommand("secret-tool",
		[]string{"lookup", "service", keychainService, "vault", ref}, nil)
	defer clear(out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
