//go:build windows

package configstore

import (
	"crypto/rand"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SystemProvider binds a KEK to the current Windows account with DPAPI.
func SystemProvider() Provider {
	return dpapiProvider{}
}

type dpapiProvider struct{}

func (dpapiProvider) providerName() string { return "dpapi" }

func (dpapiProvider) newParameters() (providerParameters, error) {
	var key keyEncryptionKey
	if _, err := rand.Read(key[:]); err != nil {
		return providerParameters{}, err
	}
	defer clear(key[:])
	input := windows.DataBlob{Size: uint32(len(key)), Data: &key[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(
		&input,
		nil,
		nil,
		0,
		nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&output,
	); err != nil {
		return providerParameters{}, ErrProviderUnavailable
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return providerParameters{
		Protected: append([]byte(nil), unsafe.Slice(output.Data, output.Size)...),
	}, nil
}

func (dpapiProvider) validateParameters(parameters providerParameters) error {
	if len(parameters.Protected) == 0 || len(parameters.Protected) > 16<<10 ||
		len(parameters.Salt) != 0 || parameters.Argon2 != nil || parameters.Reference != "" {
		return ErrFormat
	}
	return nil
}

func (dpapiProvider) unwrapKey(parameters providerParameters) (keyEncryptionKey, error) {
	var key keyEncryptionKey
	if err := (dpapiProvider{}).validateParameters(parameters); err != nil {
		return key, err
	}
	input := windows.DataBlob{Size: uint32(len(parameters.Protected)), Data: &parameters.Protected[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(
		&input,
		nil,
		nil,
		0,
		nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&output,
	); err != nil {
		return key, ErrProviderUnavailable
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	decoded := unsafe.Slice(output.Data, output.Size)
	defer clear(decoded)
	if len(decoded) != len(key) {
		return key, ErrProviderUnavailable
	}
	copy(key[:], decoded)
	return key, nil
}
