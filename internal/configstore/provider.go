package configstore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// EnvKEKVariable is the explicit container/non-interactive KEK source.
	EnvKEKVariable = "TERMCP_CONFIG_KEK"

	defaultArgon2Time    = 3
	defaultArgon2Memory  = 64 * 1024
	defaultArgon2Threads = 1
)

var (
	// ErrProviderUnavailable reports that no caller supplied the provider needed
	// by a slot, or that the provider has no usable backing secret.
	ErrProviderUnavailable = errors.New("configstore: key provider unavailable")
	// ErrPasswordConfirmation reports mismatched initial password inputs.
	ErrPasswordConfirmation = errors.New("configstore: password confirmation does not match")
)

type keyEncryptionKey [chacha20poly1305.KeySize]byte

// Argon2Parameters are persisted with password slots. Memory is in KiB and is
// validated before the KDF runs so a modified file cannot request unbounded work.
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
		return fmt.Errorf("%w: Argon2 parameters outside supported limits", ErrFormat)
	}
	return nil
}

type providerParameters struct {
	Salt      []byte            `json:"salt,omitempty"`
	Argon2    *Argon2Parameters `json:"argon2id,omitempty"`
	Reference string            `json:"reference,omitempty"`
	Protected []byte            `json:"protected,omitempty"`
}

// Provider supplies a Provider KEK. Its methods are intentionally private:
// callers can use the reviewed ENV, password, and system implementations but
// cannot inject an arbitrary decryption path through the public API.
type Provider interface {
	providerName() string
	newParameters() (providerParameters, error)
	unwrapKey(providerParameters) (keyEncryptionKey, error)
	validateParameters(providerParameters) error
}

type envProvider struct {
	variable string
}

// NewEnvProvider returns an ENV provider. An empty variable selects
// TERMCP_CONFIG_KEK.
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

// PasswordProvider derives a KEK from a recovery password with Argon2id.
type PasswordProvider struct {
	mu        sync.RWMutex
	password  []byte
	cost      Argon2Parameters
	destroyed bool
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
	return &PasswordProvider{
		password: bytes.Clone(password),
		cost:     cost,
	}, nil
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
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.destroyed {
		return providerParameters{}, ErrProviderUnavailable
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return providerParameters{}, err
	}
	cost := p.cost
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
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.destroyed {
		return key, ErrProviderUnavailable
	}
	derived := argon2.IDKey(
		p.password,
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

// Destroy clears this provider's retained password copy. Go does not promise
// that other process copies are completely erased.
func (p *PasswordProvider) Destroy() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.password)
	p.password = nil
	p.destroyed = true
}

func (p providerParameters) empty() bool {
	return len(p.Salt) == 0 && p.Argon2 == nil && p.Reference == "" && len(p.Protected) == 0
}
