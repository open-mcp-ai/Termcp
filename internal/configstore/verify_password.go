package configstore

import (
	"errors"
	"os"
	"path/filepath"
)

// VerifyDataDirPassword checks the password-derived keyring without acquiring
// the repository's writer lock. It is intended for diagnostics while a daemon
// is running and never changes persistent state.
func VerifyDataDirPassword(dataDir, password string) error {
	// readStoreFile resolves the logical keyring through local-state/state.json
	// in current layouts and falls back to the legacy physical key file. It uses
	// only read locks, so diagnostics work while the daemon owns the writer lock.
	raw, err := readStoreFile(filepath.Join(filepath.Clean(dataDir), "keys", "config.keys"), maxKeyringFile)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotInitialized
	}
	if err != nil {
		return err
	}
	defer clear(raw)
	stored, err := parseStoredKeyring(raw)
	if err != nil {
		return err
	}
	if stored.Password == nil {
		return ErrPasswordNotConfigured
	}
	secret := []byte(password)
	defer clear(secret)
	keyring, err := unlockPasswordKeyring(stored, secret)
	if keyring != nil {
		defer keyring.Destroy()
	}
	return err
}
