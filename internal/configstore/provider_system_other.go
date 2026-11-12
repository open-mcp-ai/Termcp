//go:build !darwin && !linux && !windows

package configstore

// SystemProvider reports unavailable on non-Windows platforms until native
// keychains (Keychain, Secret Service) are ported and verified.
func SystemProvider() Provider {
	return unavailableSystemProvider{}
}

type unavailableSystemProvider struct{}

func (unavailableSystemProvider) providerName() string { return "system-unavailable" }

func (unavailableSystemProvider) newParameters() (providerParameters, error) {
	return providerParameters{}, ErrProviderUnavailable
}

func (unavailableSystemProvider) validateParameters(parameters providerParameters) error {
	return ErrProviderUnavailable
}

func (unavailableSystemProvider) unwrapKey(parameters providerParameters) (keyEncryptionKey, error) {
	return keyEncryptionKey{}, ErrProviderUnavailable
}
