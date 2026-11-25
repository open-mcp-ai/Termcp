package configstore

import (
	"os"
)

// EnvKEKFileVariable names an optional path whose contents are the same
// base64-encoded 32-byte KEK that EnvKEKVariable carries. Containers should
// prefer it: a mounted secret file does not show up in `docker inspect`, does
// not leak into every child process's environment, and is not printed by a
// crash reporter that dumps env.
const EnvKEKFileVariable = "TERMCP_CONFIG_KEK_FILE"

// NewFileProvider reads a KEK from the file at path. An empty path means
// EnvKEKFileVariable, so a container can mount a secret and name it once.
// A missing file or unreadable bytes report ErrProviderUnavailable, which is
// what "this machine cannot load a key by itself" looks like to the caller.
func NewFileProvider(path string) Provider {
	if path == "" {
		path = os.Getenv(EnvKEKFileVariable)
	}
	return fileProvider{path: path}
}

type fileProvider struct {
	path string
}

func (fileProvider) providerName() string { return "file" }

func (p fileProvider) newParameters() (providerParameters, error) {
	/* Report absence here rather than at unwrap time: the keyring asks every
	   provider in the chain for parameters before writing a single slot, so a
	   provider that answers "sure" and then fails turns a working chain into a
	   failed enable operation. */
	if p.path == "" {
		return providerParameters{}, ErrProviderUnavailable
	}
	if _, err := os.Stat(p.path); err != nil {
		return providerParameters{}, ErrProviderUnavailable
	}
	return providerParameters{}, nil
}

func (fileProvider) validateParameters(parameters providerParameters) error {
	if !parameters.empty() {
		return ErrFormat
	}
	return nil
}

func (p fileProvider) unwrapKey(parameters providerParameters) (keyEncryptionKey, error) {
	var key keyEncryptionKey
	if err := p.validateParameters(parameters); err != nil {
		return key, err
	}
	if p.path == "" {
		return key, ErrProviderUnavailable
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return key, ErrProviderUnavailable
	}
	defer clear(raw)
	decoded, err := decodeKEK(raw)
	if err != nil {
		return key, err
	}
	defer clear(decoded)
	copy(key[:], decoded)
	return key, nil
}
