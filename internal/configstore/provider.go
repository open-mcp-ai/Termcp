package configstore

import (
	"errors"
	"os"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// EnvKEKVariable names the environment variable containing an optional
	// base64-encoded 32-byte key-encryption key used for automated loading.
	EnvKEKVariable = "TERMCP_CONFIG_KEK"

	defaultArgon2Time    uint32 = 3
	defaultArgon2Memory  uint32 = 64 * 1024
	defaultArgon2Threads uint8  = 1
)

var (
	ErrProviderUnavailable = errors.New("configstore: key provider unavailable")
)

type keyEncryptionKey [chacha20poly1305.KeySize]byte

// Argon2Parameters records the computational cost of one Argon2id derivation.
// It is the on-disk shape `.adk` uses (§4.5) and the bounds below are the format's:
// a file naming a cheaper or dearer cost is refused before any KDF runs.
type Argon2Parameters struct {
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
}

func defaultArgon2Parameters() Argon2Parameters {
	return Argon2Parameters{
		Time:    defaultArgon2Time,
		Memory:  defaultArgon2Memory,
		Threads: defaultArgon2Threads,
	}
}

func (p Argon2Parameters) validate() error {
	if p.Time < 1 || p.Time > 10 ||
		p.Memory < 8*1024 || p.Memory > 256*1024 ||
		p.Threads < 1 || p.Threads > 8 {
		return ErrFormat
	}
	return nil
}

type providerParameters struct {
	Salt      []byte            `json:"salt,omitempty"`
	Argon2    *Argon2Parameters `json:"argon2id,omitempty"`
	Reference string            `json:"reference,omitempty"`
	Protected []byte            `json:"protected,omitempty"`
}

// Provider wraps and unwraps Main Keys with a key-encryption key.
type Provider interface {
	providerName() string
	newParameters() (providerParameters, error)
	validateParameters(parameters providerParameters) error
	unwrapKey(parameters providerParameters) (keyEncryptionKey, error)
}

type envProvider struct {
	variable string
}

// NewEnvProvider reads a KEK from variable. When variable is empty, it defaults
// to EnvKEKVariable.
func NewEnvProvider(variable string) Provider {
	if variable == "" {
		variable = EnvKEKVariable
	}
	return envProvider{variable: variable}
}

func (envProvider) providerName() string { return "env" }

func (p envProvider) newParameters() (providerParameters, error) {
	if v, ok := os.LookupEnv(p.variable); !ok || v == "" {
		return providerParameters{}, ErrProviderUnavailable
	}
	return providerParameters{}, nil
}

func (envProvider) validateParameters(parameters providerParameters) error {
	if !parameters.empty() {
		return ErrFormat
	}
	return nil
}

func (p envProvider) unwrapKey(parameters providerParameters) (keyEncryptionKey, error) {
	var key keyEncryptionKey
	if err := p.validateParameters(parameters); err != nil {
		return key, err
	}
	encoded, ok := os.LookupEnv(p.variable)
	if !ok || encoded == "" {
		return key, ErrProviderUnavailable
	}
	decoded, err := decodeKEK([]byte(encoded))
	if err != nil {
		return key, err
	}
	defer clear(decoded)
	copy(key[:], decoded)
	return key, nil
}

func (p providerParameters) empty() bool {
	return len(p.Salt) == 0 && p.Argon2 == nil && p.Reference == "" && len(p.Protected) == 0
}

// credentialProvider implementation moved to provider_system.go
