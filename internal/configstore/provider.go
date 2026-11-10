package configstore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// EnvKEKVariable names the environment variable containing an optional
	// base64-encoded 32-byte key-encryption key used for automated unlocking.
	EnvKEKVariable = "TERMCP_CONFIG_KEK"

	defaultArgon2Time    uint32 = 3
	defaultArgon2Memory  uint32 = 64 * 1024
	defaultArgon2Threads uint8  = 1
)

var (
	ErrProviderUnavailable  = errors.New("configstore: key provider unavailable")
	ErrPasswordConfirmation = errors.New("configstore: password confirmation does not match")
)

type keyEncryptionKey [chacha20poly1305.KeySize]byte

// Argon2Parameters records the computational cost required to derive a KEK.
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

func (envProvider) newParameters() (providerParameters, error) {
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
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	defer clear(decoded)
	if err != nil || len(decoded) != len(key) {
		return key, fmt.Errorf("%w: expected a base64-encoded 32-byte KEK", ErrProviderUnavailable)
	}
	copy(key[:], decoded)
	return key, nil
}

type passwordState struct {
	password []byte
	cost     Argon2Parameters
}

// PasswordProvider derives a KEK from a recovery password with Argon2id.
// It is safe for concurrent use without a mutex: its live bytes are held in an
// atomic pointer swapped to nil by Destroy.
type PasswordProvider struct {
	state atomic.Pointer[passwordState]
}

// NewPasswordProvider copies password. A zero cost selects the production
// defaults. Use Destroy when the caller no longer needs this provider.
func NewPasswordProvider(password []byte, cost Argon2Parameters) (*PasswordProvider, error) {
	if len(password) == 0 || len(password) > 1<<20 {
		return nil, fmt.Errorf("%w: password length outside supported limits", ErrProviderUnavailable)
	}
	if cost == (Argon2Parameters{}) {
		cost = defaultArgon2Parameters()
	}
	if err := cost.validate(); err != nil {
		return nil, err
	}
	p := &PasswordProvider{}
	p.state.Store(&passwordState{
		password: bytes.Clone(password),
		cost:     cost,
	})
	return p, nil
}

// NewConfirmedPasswordProvider checks confirmation before retaining a password.
func NewConfirmedPasswordProvider(password, confirmation []byte, cost Argon2Parameters) (*PasswordProvider, error) {
	if len(password) == 0 || !bytes.Equal(password, confirmation) {
		return nil, ErrPasswordConfirmation
	}
	return NewPasswordProvider(password, cost)
}

func (*PasswordProvider) providerName() string { return "password" }

func (p *PasswordProvider) newParameters() (providerParameters, error) {
	st := p.state.Load()
	if st == nil {
		return providerParameters{}, ErrProviderUnavailable
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return providerParameters{}, err
	}
	cost := st.cost
	return providerParameters{Salt: salt, Argon2: &cost}, nil
}

func (p *PasswordProvider) validateParameters(parameters providerParameters) error {
	if len(parameters.Salt) != 16 || parameters.Argon2 == nil ||
		parameters.Reference != "" || len(parameters.Protected) != 0 {
		return ErrFormat
	}
	return parameters.Argon2.validate()
}

func (p *PasswordProvider) unwrapKey(parameters providerParameters) (keyEncryptionKey, error) {
	var key keyEncryptionKey
	if err := p.validateParameters(parameters); err != nil {
		return key, err
	}
	st := p.state.Load()
	if st == nil {
		return key, ErrProviderUnavailable
	}
	derived := argon2.IDKey(
		st.password,
		parameters.Salt,
		parameters.Argon2.Time,
		parameters.Argon2.Memory,
		parameters.Argon2.Threads,
		uint32(len(key)),
	)
	copy(key[:], derived)
	clear(derived)
	return key, nil
}

// Destroy revokes this provider: no later derivation can use its password, and
// callers report ErrProviderUnavailable.
//
// ponytail: the retained bytes are dropped, not overwritten — see Keyring.Destroy
// for why an in-place clear cannot be made race-free without the lock this
// replaces. The password reaches disk nowhere, which is what the tests check.
func (p *PasswordProvider) Destroy() {
	p.state.Store(nil)
}

func (p providerParameters) empty() bool {
	return len(p.Salt) == 0 && p.Argon2 == nil && p.Reference == "" && len(p.Protected) == 0
}

// credentialProvider represents a platform keychain slot for keyring parsing.
// System keychain access is optional; validation only requires the structural
// shape to be parsable.
type credentialProvider struct {
	name string
}

func (p credentialProvider) providerName() string { return p.name }

func (credentialProvider) newParameters() (providerParameters, error) {
	return providerParameters{}, ErrProviderUnavailable
}

func (credentialProvider) unwrapKey(providerParameters) (keyEncryptionKey, error) {
	return keyEncryptionKey{}, ErrProviderUnavailable
}

func (credentialProvider) validateParameters(parameters providerParameters) error {
	if len(parameters.Reference) == 0 || len(parameters.Reference) > 1024 ||
		len(parameters.Salt) != 0 || parameters.Argon2 != nil || len(parameters.Protected) != 0 {
		return ErrFormat
	}
	return nil
}
