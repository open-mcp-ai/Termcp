//go:build windows

package vault

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsSecretStore protects the identity with DPAPI bound to the current
// Windows account and keeps the resulting blob as identity.dpapi inside the
// vault directory. The blob is ciphertext — DPAPI needs the user's logon
// credentials to open it — so a file on disk is not a downgrade from the
// keychain files on other platforms; the ref argument is unused.
//
// DPAPI is called directly (golang.org/x/sys/windows) rather than through
// PowerShell: one syscall, no process spawn, nothing on a command line.
type windowsSecretStore struct{}

const dpapiBlobFile = "identity.dpapi"

func newPlatformSecretStore() secretStore { return windowsSecretStore{} }

func (windowsSecretStore) Available() bool { return true }

func (windowsSecretStore) Store(v *Vault, _, secret string) error {
	blob, err := protectDPAPI([]byte(secret))
	if err != nil {
		return err
	}
	return atomicWrite0600(filepath.Join(v.Dir(), dpapiBlobFile), blob)
}

func (windowsSecretStore) Load(v *Vault, _ string) (string, error) {
	blob, err := os.ReadFile(filepath.Join(v.Dir(), dpapiBlobFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrKeychainUnavailable
		}
		return "", err
	}
	plain, err := unprotectDPAPI(blob)
	if err != nil {
		// Wrong Windows account or a copied blob: same answer either way.
		return "", ErrKeychainUnavailable
	}
	return string(plain), nil
}

func protectDPAPI(secret []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(secret)), Data: &secret[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}

func unprotectDPAPI(blob []byte) ([]byte, error) {
	if len(blob) == 0 || len(blob) > 16<<10 {
		return nil, errors.New("invalid DPAPI blob size")
	}
	input := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}
