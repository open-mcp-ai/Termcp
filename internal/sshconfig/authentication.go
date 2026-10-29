package sshconfig

import (
	"errors"
	"github.com/open-mcp-ai/termcp/internal/configstore"
)

// PrepareAuthentication shares the profile repository and Main Key. Creating
// a keyring for authentication does not opt plaintext profiles into encryption.
func (s *Store) PrepareAuthentication(password, previous string, automatic bool, providers ...configstore.Provider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.encrypted == nil {
		var selected []configstore.Provider
		if automatic {
			selected = providers
		}
		repo, err := configstore.InitializeRepositoryWithPassword(s.dataDir, password, password, selected...)
		if err != nil {
			if !errors.Is(err, configstore.ErrAlreadyInitialized) && !errors.Is(err, configstore.ErrDurability) {
				return err
			}
			repo, err = configstore.OpenRepositoryForRecovery(s.dataDir, providers...)
			if err != nil {
				return err
			}
			s.encrypted = repo
		} else {
			s.encrypted = repo
			s.encryptNew = repo.EncryptNew()
			return nil
		}
	}
	repo := s.encrypted
	if repo.Status().Locked {
		candidate := password
		if previous != "" {
			candidate = previous
		}
		if err := repo.UnlockPassword(candidate); err != nil {
			return err
		}
	}
	if !repo.Status().PasswordConfigured {
		if err := repo.ChangePassword("", password, password, providers...); err != nil {
			return err
		}
	} else if err := repo.VerifyPassword(password); err != nil {
		if previous == "" {
			return configstore.ErrAuthentication
		}
		if err := repo.ChangePassword(previous, password, password, providers...); err != nil {
			return err
		}
	}
	if repo.Status().RotationPending {
		if err := repo.ChangePassword(password, "", "", providers...); err != nil {
			return err
		}
	}
	if automatic {
		if err := repo.RebindPasswordProvider(providers...); err != nil {
			return err
		}
	}
	s.encryptNew = repo.EncryptNew()
	s.invalidateIdentitiesLocked()
	return nil
}

func (s *Store) UnlockAuthentication(password string, providers ...configstore.Provider) error {
	s.mu.Lock()
	changed := false
	defer func() {
		s.mu.Unlock()
		if changed {
			s.notifyChange()
		}
	}()
	if s.encrypted == nil {
		return configstore.ErrNotInitialized
	}
	if s.encrypted.Status().Locked {
		if err := s.encrypted.UnlockPassword(password); err != nil {
			return err
		}
		changed = true
	}
	if err := s.encrypted.VerifyPassword(password); err != nil {
		return err
	}
	if s.encrypted.Status().RotationPending {
		if err := s.encrypted.ChangePassword(password, "", "", providers...); err != nil {
			return err
		}
		changed = true
	}
	s.encryptNew = s.encrypted.EncryptNew()
	s.invalidateIdentitiesLocked()
	return nil
}

func (s *Store) ReadAuthentication() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.encrypted == nil {
		return nil, configstore.ErrNotInitialized
	}
	return s.encrypted.ReadAuthentication()
}

func (s *Store) WriteAuthentication(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.encrypted == nil {
		return configstore.ErrNotInitialized
	}
	return s.encrypted.WriteAuthentication(data)
}

func (s *Store) AuthenticationLocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encrypted == nil || s.encrypted.Status().Locked
}

func (s *Store) AuthenticationInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encrypted != nil
}
