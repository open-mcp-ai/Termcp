package configstore

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// decodeKEK reads the canonical base64 encoding of a 32-byte key, tolerating the
// trailing newline an editor or `kubectl create secret --from-file` leaves behind.
func decodeKEK(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	decoded, err := base64.StdEncoding.Strict().DecodeString(string(trimmed))
	if err != nil || len(decoded) != len(keyEncryptionKey{}) {
		clear(decoded)
		return nil, fmt.Errorf("%w: expected a base64-encoded 32-byte KEK", ErrProviderUnavailable)
	}
	return decoded, nil
}

// DefaultProviders is the unlock chain, in the order the repository tries it:
//
//  1. the operating system's credential store, which follows the machine and the
//     logged-in account — the desktop case, and the reason a restart needs no
//     password at all;
//  2. a mounted secret file, which is how a container gets the same behaviour
//     without putting a key in its environment;
//  3. the environment variable, kept as the last resort it always was.
//
// Every entry may report ErrProviderUnavailable; that is not an error, it is the
// absence of that path on this host. When all three are unavailable the store
// opens locked and waits for the password, which stays the recovery route.
func DefaultProviders() []Provider {
	return []Provider{
		SystemProvider(),
		NewFileProvider(""),
		NewEnvProvider(""),
	}
}

/* SystemAvailable reports whether this host has the native credential store the
 * SystemProvider talks to, without touching it: asking for parameters would
 * write a throwaway secret into the user's keychain, which a capability check
 * has no business doing. */
func SystemAvailable() bool {
	switch runtime.GOOS {
	case "windows":
		return true // DPAPI is always present on Windows
	case "darwin":
		_, err := exec.LookPath("security")
		return err == nil
	case "linux":
		_, err := exec.LookPath("secret-tool")
		return err == nil
	}
	return false
}

// DiscardDataDirKeys removes the keyring and its catalog, returning the data
// directory to a plaintext store. It deletes the catalog first: a leftover
// catalog without keys is the one state the opener refuses outright, while keys
// without a catalog are inert.
func DiscardDataDirKeys(dataDir string) error {
	for _, path := range []string{repositoryCatalogPath(dataDir), filepath.Join(dataDir, "keys", "config.keys")} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return syncDir(filepath.Join(dataDir, "keys"))
}
