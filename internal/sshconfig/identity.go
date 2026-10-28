package sshconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

const identityFileName = "identity"

// InternalIdentity belongs to the virtual built-in profile. Its defaults never
// create a directory just to persist an identity.
const InternalIdentity = "00000000-0000-4000-8000-000000000001"

var (
	ErrInvalidIdentity   = errors.New("invalid profile identity")
	ErrDuplicateIdentity = errors.New("duplicate profile identity")
)

func newProfileIdentity() (string, error) {
	for {
		id, err := uuid.NewRandom()
		if err != nil {
			return "", err
		}
		if id.String() != InternalIdentity {
			return id.String(), nil
		}
	}
}

func validProfileIdentity(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func identityDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: profile directory is not a real directory", ErrInvalidIdentity)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(path) {
		return fmt.Errorf("%w: profile directory traverses a symbolic link", ErrInvalidIdentity)
	}
	return nil
}

func readProfileIdentity(dir string) (string, error) {
	if err := identityDirectory(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, identityFileName)
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Size() > 64 {
		return "", ErrInvalidIdentity
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return "", ErrInvalidIdentity
	}
	data, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(string(data), "\n")
	value = strings.TrimSuffix(value, "\r")
	if !validProfileIdentity(value) {
		return "", ErrInvalidIdentity
	}
	return value, nil
}

// ensureProfileIdentity never replaces an existing identity. A fully written
// temporary file is linked into place so concurrent Store instances cannot
// observe an empty identity or overwrite the winner with another UUID.
func ensureProfileIdentity(dir, expected string) (string, error) {
	required := expected
	if id, err := readProfileIdentity(dir); err == nil {
		if expected != "" && expected != id {
			return "", ErrInvalidIdentity
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if expected == "" {
		var err error
		expected, err = newProfileIdentity()
		if err != nil {
			return "", err
		}
	}
	if !validProfileIdentity(expected) {
		return "", ErrInvalidIdentity
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := identityDirectory(dir); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := io.WriteString(tmp, expected+"\n"); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), filepath.Join(dir, identityFileName)); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		id, readErr := readProfileIdentity(dir)
		if readErr != nil {
			return "", readErr
		}
		if required != "" && required != id {
			return "", ErrInvalidIdentity
		}
		// With no expected value, another creator's valid UUID wins.
		return id, nil
	}
	if runtime.GOOS != "windows" {
		parent, err := os.Open(dir)
		if err != nil {
			return "", err
		}
		err = parent.Sync()
		_ = parent.Close()
		if err != nil {
			return "", err
		}
	}
	return expected, nil
}

func hasEncryptedProfile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) != 36 || !strings.HasSuffix(name, ".enc") {
			continue
		}
		valid := true
		for _, char := range name[:32] {
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
				valid = false
			}
		}
		if valid {
			return true
		}
	}
	return false
}

func (s *Store) invalidateIdentitiesLocked() {
	s.identitiesLoaded = false
}

func (s *Store) refreshIdentitiesLocked(force bool) error {
	var stamp time.Time
	if info, err := os.Stat(s.root()); err == nil {
		stamp = info.ModTime()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !force && s.identitiesLoaded && stamp.Equal(s.identityStamp) {
		return nil
	}
	byName := map[string]string{"internal": InternalIdentity}
	byID := map[string]string{InternalIdentity: "internal"}
	encrypted := make(map[string]bool)
	knownEncrypted := s.encrypted != nil && !s.encrypted.Status().Locked
	if knownEncrypted {
		names, err := s.encrypted.List()
		if err != nil {
			return err
		}
		for _, name := range names {
			encrypted[strings.ToLower(name)] = true
		}
	}
	entries, err := os.ReadDir(s.root())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name != strings.TrimSpace(name) || ValidateName(name) != nil {
			continue
		}
		dir := filepath.Join(s.root(), name)
		plain, err := regularFileExists(filepath.Join(dir, "config.toml"))
		if err != nil {
			return err
		}
		if IsInternalName(name) {
			plain = false // legacy internal/ TOML is deliberately ignored
		}
		cipher := encrypted[strings.ToLower(name)] || s.encrypted != nil && !knownEncrypted && hasEncryptedProfile(dir)
		if !plain && !cipher {
			continue
		}
		expected := ""
		if IsInternalName(name) {
			name, expected = "internal", InternalIdentity
		}
		id, err := ensureProfileIdentity(dir, expected)
		if err != nil {
			return fmt.Errorf("profile %q identity: %w", name, err)
		}
		if old, duplicate := byID[id]; duplicate && old != name || id == InternalIdentity && !IsInternalName(name) {
			return fmt.Errorf("%w: %q and %q", ErrDuplicateIdentity, byID[id], name)
		}
		if old, duplicate := byName[strings.ToLower(name)]; duplicate && !IsInternalName(name) {
			return fmt.Errorf("profile name collision: %q (%s)", name, old)
		}
		byName[strings.ToLower(name)], byID[id] = id, name
	}
	if exists, err := regularFileExists(s.internalOverridePath()); err != nil {
		return err
	} else if exists {
		dir := filepath.Dir(s.internalOverridePath())
		if _, err := ensureProfileIdentity(dir, InternalIdentity); err != nil {
			return err
		}
	}
	for name, id := range s.temporaryIdentities {
		if _, exists := s.temporary[name]; !exists {
			continue
		}
		if other, duplicate := byID[id]; duplicate && other != name {
			return ErrDuplicateIdentity
		}
		byName[strings.ToLower(name)], byID[id] = id, name
	}
	s.identityByName, s.nameByIdentity = byName, byID
	s.identityStamp, s.identitiesLoaded = stamp, true
	return nil
}

// RefreshIdentities scans profile directories, backfills missing identity
// sidecars, and rejects malformed or duplicate identities without rewriting TOML.
func (s *Store) RefreshIdentities() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshIdentitiesLocked(true)
}

func (s *Store) Identity(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshIdentitiesLocked(false); err != nil {
		return "", err
	}
	if id, found := s.identityByName[strings.ToLower(strings.TrimSpace(name))]; found {
		return id, nil
	}
	return "", ErrNotFound
}

func (s *Store) NameByIdentity(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshIdentitiesLocked(false); err != nil {
		return "", err
	}
	if name, found := s.nameByIdentity[id]; found {
		return name, nil
	}
	return "", ErrNotFound
}

// ResolveReference keeps names as compatible input while identities take
// precedence for already-bound references. An unknown identity never falls
// back to a reused name.
func (s *Store) ResolveReference(name, identity string) (string, string, error) {
	if identity != "" {
		resolved, err := s.NameByIdentity(identity)
		return resolved, identity, err
	}
	id, err := s.Identity(name)
	if err != nil {
		return name, "", err
	}
	resolved, err := s.NameByIdentity(id)
	return resolved, id, err
}

// ResolveSessionReference also permits ad-hoc legacy labels that are not backed
// by a stored profile. Explicit identity references remain strict.
func (s *Store) ResolveSessionReference(name, identity string) (string, string, error) {
	resolved, id, err := s.ResolveReference(name, identity)
	if identity == "" && errors.Is(err, ErrNotFound) {
		return name, "", nil
	}
	return resolved, id, err
}

func removeProfileIdentity(dir string) error {
	if err := os.Remove(filepath.Join(dir, identityFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(dir) // remove only an empty directory
	return nil
}
