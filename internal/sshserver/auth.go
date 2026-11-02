package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"time"

	sshstd "golang.org/x/crypto/ssh"
)

func (s *Server) passwordOK(user, password string) bool {
	v, ok := s.pending.LoadAndDelete(user)
	if !ok {
		return false
	}
	expPass, _ := v.(string)
	if len(password) != len(expPass) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(expPass)) == 1
}

func (s *Server) mintCreds() (user, pass string, err error) {
	userRand := make([]byte, 10)
	if _, err := rand.Read(userRand); err != nil {
		return "", "", err
	}
	passRand := make([]byte, 32)
	if _, err := rand.Read(passRand); err != nil {
		return "", "", err
	}
	user = "t" + hex.EncodeToString(userRand)
	pass = base64.RawURLEncoding.EncodeToString(passRand)
	return user, pass, nil
}

// MintClientConfig registers a new one-time username/password and returns a dial config.
// The entry is removed on the first successful SSH password authentication (single use).
// Call only after Start().
func (s *Server) MintClientConfig() (*sshstd.ClientConfig, error) {
	user, pass, err := s.mintCreds()
	if err != nil {
		return nil, err
	}
	s.pending.Store(user, pass)
	return &sshstd.ClientConfig{
		User:            user,
		Auth:            []sshstd.AuthMethod{sshstd.Password(pass)},
		HostKeyCallback: sshstd.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}, nil
}

// RevokeClientConfig drops a pending one-time credential minted by
// MintClientConfig. Call it when the returned config will never complete a
// successful authentication (dial refused, handshake error), otherwise the
// entry would stay in the pending map for the lifetime of the process — a
// memory leak on every failed session start. Revoking a credential that was
// already consumed is a no-op.
func (s *Server) RevokeClientConfig(user string) {
	if user == "" {
		return
	}
	s.pending.Delete(user)
}

// generateHostKeyPEM mints the ephemeral host key the in-process server presents.
//
// ed25519 rather than RSA-2048 because the key type is the largest single cost of
// a loopback handshake. The client is obliged to verify the server's KEX signature
// (x/crypto/ssh handshake.go), which is not skippable from this side: measured
// ~845us for RSA-2048 against ~59us for ed25519, and end-to-end that is 1.55ms per
// connection down to 0.72ms. Minting is ~23ms for RSA per process start and
// microseconds for ed25519.
//
// The key is ephemeral, generated per process, and never verified against anything
// -- clients authenticate with a one-time password, not against this key -- so RSA's
// longer key adds cost here and no trust.
func generateHostKeyPEM() ([]byte, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	block := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: b,
	}
	return pem.EncodeToMemory(block), nil
}
