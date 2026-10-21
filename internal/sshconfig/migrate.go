package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/open-mcp-ai/termcp/internal/vault"
)

// MigrateFailure is one profile that could not be migrated, named so the
// caller can report it and continue with the rest.
type MigrateFailure struct {
	Name string
	Err  error
}

// MigrateResult reports what a MigrateToVault run did.
type MigrateResult struct {
	Migrated []string         // profiles sealed and scrubbed this run
	Failed   []MigrateFailure // per-profile failures; the run continues past them
}

// MigrateToVault seals every plaintext remote profile into the vault and
// scrubs the TOML left behind. It needs an unlocked vault and refuses with
// vault.ErrLocked otherwise — a migration written by a caller that cannot
// read back what it sealed is how data gets lost.
//
// Each profile is verified before its plaintext is removed: the freshly
// sealed ciphertext is decrypted and byte-compared against the original, so a
// seal bug cannot destroy the only copy. A profile that fails any step keeps
// its plaintext and lands in Failed.
//
// The operation is idempotent (a re-run finds no plaintext and reports an
// empty Migrated) and crash-safe (a rerun after a crash between seal and
// scrub re-seals, re-verifies, and finishes the scrub).
func (s *Store) MigrateToVault() (*MigrateResult, error) {
	s.mu.Lock()
	vt := s.vault
	s.mu.Unlock()
	if vt == nil || !vt.Exists() {
		return nil, fmt.Errorf("no vault: run `termcp vault init` first")
	}
	if !vt.IsUnlocked() {
		return nil, vault.ErrLocked
	}
	res := &MigrateResult{}
	entries, err := os.ReadDir(s.root())
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".internal" {
			continue
		}
		name := e.Name()
		if ValidateName(name) != nil || IsInternalName(name) {
			continue
		}
		tomlPath := filepath.Join(s.root(), name, "config.toml")
		data, err := os.ReadFile(tomlPath)
		if err != nil {
			continue // no plaintext file: already vaulted, temporary, or not a profile dir
		}
		if _, err := ParseAndValidate(data); err != nil {
			res.Failed = append(res.Failed, MigrateFailure{name, fmt.Errorf("validate: %w", err)})
			continue
		}
		if err := vt.Encrypt(name, data); err != nil {
			res.Failed = append(res.Failed, MigrateFailure{name, err})
			continue
		}
		roundTrip, err := vt.Decrypt(name)
		if err != nil || string(roundTrip) != string(data) {
			res.Failed = append(res.Failed, MigrateFailure{name, fmt.Errorf("sealed copy failed verification; plaintext kept")})
			continue
		}
		if err := os.Remove(tomlPath); err != nil {
			res.Failed = append(res.Failed, MigrateFailure{name, fmt.Errorf("scrub plaintext: %w", err)})
			continue
		}
		_ = os.Remove(filepath.Dir(tomlPath))
		res.Migrated = append(res.Migrated, name)
	}
	sort.Strings(res.Migrated)
	return res, nil
}
