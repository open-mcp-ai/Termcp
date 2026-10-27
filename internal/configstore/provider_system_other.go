//go:build !darwin && !linux && !windows

package configstore

// SystemProvider reports unavailability on platforms without a reviewed native
// credential store. Container deployments use ENV or password recovery instead.
func SystemProvider() Provider {
	return unavailableSystemProvider{}
}

type unavailableSystemProvider struct{}

func (unavailableSystemProvider) providerName() string { return "system-unavailable" }

func (unavailableSystemProvider) newParameters() (providerParameters, error) {
	return providerParameters{}, ErrProviderUnavailable
}

func (unavailableSystemProvider) unwrapKey(providerParameters) (keyEncryptionKey, error) {
	return keyEncryptionKey{}, ErrProviderUnavailable
}

func (unavailableSystemProvider) validateParameters(providerParameters) error {
	return ErrProviderUnavailable
}
