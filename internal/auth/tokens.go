package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// This file implements scoped tokens (design doc scoped-tokens.md §4):
// opaque random tokens resolved against a server-side table — the GitHub
// fine-grained PAT shape. termcp is a single-instance stateful service
// already, so a table costs no architecture: revocation is a row edit that
// takes effect on the next request, and there is no signing-key rotation to
// maintain.
//
// The token string carries its table id (`tcptok_<id>.<secret>`) so Resolve
// finds the row without scanning; the secret half is verified against the
// row's salted verifier in constant time. The separator is a dot because the
// base64url alphabet never produces one — splitting id from secret cannot
// depend on the random bytes' content. The plaintext exists exactly twice:
// in the Create response and in the client's hands — the table stores only
// the verifier.

// Principal is what every authentication path produces (scoped-tokens.md
// §4.2): downstream code — admission, logging, the approval policy — only
// ever sees this, never a token shape.
type Principal struct {
	ID   string // token id, or "operator"/"mcp" for the static credentials
	Kind Kind
	// Tools whitelists capability ids ("shell.input", "file.write"); empty
	// means unrestricted. Sessions restricts the sessions/entries the token
	// may touch; empty means all.
	Tools    []string
	Sessions []string
	// ApprovalRequired forces every write through the review queue, whatever
	// the session's own review mode says.
	ApprovalRequired bool
}

// ScopeFree reports whether the principal carries no per-operation or
// per-session restriction (the static credentials, and scoped tokens created
// without lists).
func (p *Principal) ScopeFree() bool {
	return len(p.Tools) == 0 && len(p.Sessions) == 0
}

// TokenRecord is one row of the tokens table. The verifier is a salted hash
// (auth.Hash's format); the plaintext token is never stored. JSON tags keep
// the REST API in the same snake_case as every other endpoint.
type TokenRecord struct {
	ID               string    `toml:"id" json:"id"`
	Verifier         string    `toml:"verifier" json:"-"`
	Kind             string    `toml:"kind" json:"kind"` // "agent"; "human" reserved for future use
	Tools            []string  `toml:"tools,omitempty" json:"tools,omitempty"`
	Sessions         []string  `toml:"sessions,omitempty" json:"sessions,omitempty"`
	ApprovalRequired bool      `toml:"approval_required,omitempty" json:"approval_required,omitempty"`
	ExpiresAt        time.Time `toml:"expires_at,omitempty" json:"expires_at,omitempty"`
	CreatedBy        string    `toml:"created_by" json:"created_by"`
	CreatedAt        time.Time `toml:"created_at" json:"created_at"`
	RevokedAt        time.Time `toml:"revoked_at,omitempty" json:"revoked_at,omitempty"`
}

// tokenFileVersion guards the on-disk shape.
const tokenFileVersion = 1

type tokenFile struct {
	Version int           `toml:"version"`
	Tokens  []TokenRecord `toml:"token"`
}

// Token errors. ErrTokenUnknown covers "no such id" and "bad secret" alike —
// telling them apart would let an attacker enumerate ids.
var (
	ErrTokenUnknown  = errors.New("unknown or invalid token")
	ErrTokenExpired  = errors.New("token expired")
	ErrTokenRevoked  = errors.New("token revoked")
	ErrTokenNotFound = errors.New("token id not found")
)

const tokenPrefix = "tcptok_"

// TokenStore persists the tokens table at data-dir/tokens.toml (0600).
type TokenStore struct {
	path string
	mu   sync.Mutex
}

// OpenTokenStore loads (or lazily creates on first write) the table.
func OpenTokenStore(dataDir string) *TokenStore {
	return &TokenStore{path: filepath.Join(dataDir, "tokens.toml")}
}

// Create mints a scoped token. The plaintext token is returned exactly once,
// here; the store keeps only its verifier. ttl=0 means no expiry.
func (s *TokenStore) Create(kind string, tools, sessions []string, approvalRequired bool, ttl time.Duration, createdBy string) (string, *TokenRecord, error) {
	if kind != string(KindAgent) && kind != string(KindOperator) {
		return "", nil, fmt.Errorf("kind must be %q or %q", KindAgent, KindOperator)
	}
	idRaw := make([]byte, 8)
	secret := make([]byte, 24)
	if _, err := rand.Read(idRaw); err != nil {
		return "", nil, err
	}
	if _, err := rand.Read(secret); err != nil {
		return "", nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(idRaw)
	secretStr := base64.RawURLEncoding.EncodeToString(secret)
	verifier, err := Hash(secretStr)
	if err != nil {
		return "", nil, err
	}
	rec := TokenRecord{
		ID:               id,
		Verifier:         verifier,
		Kind:             kind,
		Tools:            tools,
		Sessions:         sessions,
		ApprovalRequired: approvalRequired,
		CreatedBy:        createdBy,
		CreatedAt:        time.Now().UTC(),
	}
	if ttl > 0 {
		rec.ExpiresAt = rec.CreatedAt.Add(ttl)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadLocked()
	if err != nil {
		return "", nil, err
	}
	f.Tokens = append(f.Tokens, rec)
	if err := s.saveLocked(f); err != nil {
		return "", nil, err
	}
	return tokenPrefix + id + "." + secretStr, &rec, nil
}

// Resolve verifies a presented token and returns its Principal. Expired and
// revoked tokens fail closed with their own errors (the caller answers 401
// either way; the distinction is for logs).
func (s *TokenStore) Resolve(presented string) (*Principal, error) {
	id, secret, ok := strings.Cut(strings.TrimPrefix(presented, tokenPrefix), ".")
	if !ok || !strings.HasPrefix(presented, tokenPrefix) {
		return nil, ErrTokenUnknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	for _, rec := range f.Tokens {
		if subtle.ConstantTimeCompare([]byte(rec.ID), []byte(id)) != 1 {
			continue
		}
		salt, digest, err := parseHash(rec.Verifier)
		if err != nil {
			return nil, fmt.Errorf("token %q: stored verifier unreadable", id)
		}
		if subtle.ConstantTimeCompare(digestOf(salt, secret), digest) != 1 {
			return nil, ErrTokenUnknown
		}
		if !rec.RevokedAt.IsZero() {
			return nil, ErrTokenRevoked
		}
		if !rec.ExpiresAt.IsZero() && !time.Now().Before(rec.ExpiresAt) {
			return nil, ErrTokenExpired
		}
		return &Principal{
			ID:               rec.ID,
			Kind:             Kind(rec.Kind),
			Tools:            rec.Tools,
			Sessions:         rec.Sessions,
			ApprovalRequired: rec.ApprovalRequired,
		}, nil
	}
	return nil, ErrTokenUnknown
}

// List returns the table without verifiers redacted — the verifier is a
// salted hash and safe to display, but callers only ever need metadata, so
// List drops it anyway: one less place a credential-shaped string travels.
func (s *TokenStore) List() ([]TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]TokenRecord, 0, len(f.Tokens))
	for _, rec := range f.Tokens {
		rec.Verifier = ""
		out = append(out, rec)
	}
	return out, nil
}

// Revoke marks a token dead, effective on the next request.
func (s *TokenStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range f.Tokens {
		if f.Tokens[i].ID == id {
			if f.Tokens[i].RevokedAt.IsZero() {
				f.Tokens[i].RevokedAt = time.Now().UTC()
				return s.saveLocked(f)
			}
			return nil // already revoked: idempotent
		}
	}
	return fmt.Errorf("%w: %q", ErrTokenNotFound, id)
}

func (s *TokenStore) loadLocked() (*tokenFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &tokenFile{Version: tokenFileVersion}, nil
		}
		return nil, err
	}
	var f tokenFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("tokens.toml: %w", err)
	}
	return &f, nil
}

func (s *TokenStore) saveLocked(f *tokenFile) error {
	f.Version = tokenFileVersion
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tmp-tokens-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := toml.NewEncoder(tmp).Encode(f); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, s.path)
}
