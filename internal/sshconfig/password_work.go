package sshconfig

import "github.com/open-mcp-ai/termcp/internal/configstore"

// passwordRepository serializes long key operations without holding Store.mu.
// work may create a repository; adoption is checked against Close and the
// original repository identity before publishing its metadata to the Store.
func (s *Store) passwordRepository(work func(*configstore.Repository) (*configstore.Repository, bool, error)) error {
	s.passwordMu.Lock()
	notify := false
	defer func() {
		s.passwordMu.Unlock()
		if notify {
			s.notifyChange()
		}
	}()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return configstore.ErrClosed
	}
	original := s.encrypted
	s.mu.Unlock()
	repo, changed, err := work(original)
	s.mu.Lock()
	if s.closed || s.encrypted != original {
		s.mu.Unlock()
		if repo != nil && repo != original {
			_ = repo.Close()
		}
		return configstore.ErrClosed
	}
	if repo != nil {
		s.encrypted = repo
		state := repo.Status()
		if !state.Closed && !state.Locked {
			s.encryptNew = repo.EncryptNew()
		}
	}
	s.invalidateIdentitiesLocked()
	s.mu.Unlock()
	notify = changed
	return err
}
