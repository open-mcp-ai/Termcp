//go:build !darwin && !linux && !windows

package vault

import "errors"

// otherSecretStore is the empty backend for platforms without a credential
// CLI integration. The passphrase path is the whole unlock chain there.
type otherSecretStore struct{}

func newPlatformSecretStore() secretStore { return otherSecretStore{} }

func (otherSecretStore) Available() bool { return false }

var errNoStore = errors.New("no credential store on this platform")

func (otherSecretStore) Store(_ *Vault, _, _ string) error       { return errNoStore }
func (otherSecretStore) Load(_ *Vault, _ string) (string, error) { return "", errNoStore }
