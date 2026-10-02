package sshserver

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"time"

	sshstd "golang.org/x/crypto/ssh"
)

func (s *Server) passwordOK(user, password string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expPass, ok := s.pending[user]
	if !ok {
		return false
	}
	if len(password) != len(expPass) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(expPass)) != 1 {
		return false
	}
	delete(s.pending, user)
	return true
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
	s.mu.Lock()
	s.pending[user] = pass
	s.mu.Unlock()
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
	s.mu.Lock()
	delete(s.pending, user)
	s.mu.Unlock()
}

func generateHostKeyPEM() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	b := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: b,
	}
	return pem.EncodeToMemory(block), nil
}
