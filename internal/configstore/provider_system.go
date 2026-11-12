package configstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os/exec"
	"time"
)

const systemService = "org.termcp.config-encryption.v1"

// credentialStore stores a provider KEK outside the data directory. References
// are random values and never contain the local login name or data path.
type credentialStore interface {
	store(reference, secret string) error
	load(reference string) (string, error)
}

type credentialProvider struct {
	name  string
	store credentialStore
}

func (p credentialProvider) providerName() string { return p.name }

func (p credentialProvider) newParameters() (providerParameters, error) {
	reference := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, reference); err != nil {
		return providerParameters{}, err
	}
	var key keyEncryptionKey
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return providerParameters{}, err
	}
	defer clear(key[:])
	encodedReference := hex.EncodeToString(reference)
	encodedSecret := base64.StdEncoding.EncodeToString(key[:])
	if err := p.store.store(encodedReference, encodedSecret); err != nil {
		return providerParameters{}, ErrProviderUnavailable
	}
	return providerParameters{Reference: encodedReference}, nil
}

func (p credentialProvider) validateParameters(parameters providerParameters) error {
	if len(parameters.Reference) != 32 || len(parameters.Salt) != 0 ||
		parameters.Argon2 != nil || len(parameters.Protected) != 0 {
		return ErrFormat
	}
	if _, err := hex.DecodeString(parameters.Reference); err != nil {
		return ErrFormat
	}
	return nil
}

func (p credentialProvider) unwrapKey(parameters providerParameters) (keyEncryptionKey, error) {
	var key keyEncryptionKey
	if err := p.validateParameters(parameters); err != nil {
		return key, err
	}
	secret, err := p.store.load(parameters.Reference)
	if err != nil {
		return key, ErrProviderUnavailable
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(secret)
	defer clear(decoded)
	if err != nil || len(decoded) != len(key) {
		return key, ErrProviderUnavailable
	}
	copy(key[:], decoded)
	return key, nil
}

// System commands receive secrets on stdin. Their stderr is discarded because
// desktop credential tools can echo values in diagnostics.
func credentialCommand(path string, args []string, input io.Reader) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = input
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	return cmd.Output()
}
