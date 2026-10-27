package configstore

// Platform-neutral slot validators let keyring parsing reject malformed provider
// parameters before deriving or touching a system credential store.

type credentialProviderValidator struct {
	name string
}

func (p credentialProviderValidator) providerName() string { return p.name }

func (credentialProviderValidator) newParameters() (providerParameters, error) {
	return providerParameters{}, ErrProviderUnavailable
}

func (credentialProviderValidator) unwrapKey(providerParameters) (keyEncryptionKey, error) {
	return keyEncryptionKey{}, ErrProviderUnavailable
}

func (p credentialProviderValidator) validateParameters(parameters providerParameters) error {
	return credentialProvider{name: p.name}.validateParameters(parameters)
}

type dpapiProviderValidator struct{}

func (dpapiProviderValidator) providerName() string { return "dpapi" }

func (dpapiProviderValidator) newParameters() (providerParameters, error) {
	return providerParameters{}, ErrProviderUnavailable
}

func (dpapiProviderValidator) unwrapKey(providerParameters) (keyEncryptionKey, error) {
	return keyEncryptionKey{}, ErrProviderUnavailable
}

func (dpapiProviderValidator) validateParameters(parameters providerParameters) error {
	if len(parameters.Protected) == 0 || len(parameters.Protected) > 16<<10 ||
		len(parameters.Salt) != 0 || parameters.Argon2 != nil || parameters.Reference != "" {
		return ErrFormat
	}
	return nil
}

func dpapiProviderForValidation() Provider {
	return dpapiProviderValidator{}
}
