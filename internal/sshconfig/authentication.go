package sshconfig

import (
	"errors"
	"github.com/open-mcp-ai/termcp/internal/configstore"
)

// PrepareAuthentication shares the profile repository and Main Key. Creating
// a keyring for authentication does not opt plaintext profiles into encryption.
func (s *Store) PrepareAuthentication(password, previous string, automatic bool, providers ...configstore.Provider) error {
	configPassword := password
	if previous != "" {
		configPassword = previous
	}
	return s.passwordRepository(func(repo *configstore.Repository) (*configstore.Repository, bool, error) {
		if repo == nil {
			var selected []configstore.Provider
			if automatic {
				selected = providers
			}
			candidate, err := configstore.InitializeRepositoryWithPassword(s.dataDir, configPassword, configPassword, selected...)
			if err == nil {
				return candidate, true, nil
			}
			if !errors.Is(err, configstore.ErrAlreadyInitialized) && !errors.Is(err, configstore.ErrDurability) {
				return nil, false, err
			}
			repo, err = configstore.OpenRepositoryForRecovery(s.dataDir, providers...)
			if err != nil {
				return nil, false, err
			}
		}
		changed := false
		if repo.Status().Locked {
			if err := repo.UnlockPassword(configPassword); err != nil {
				return repo, false, err
			}
			changed = true
		} else if repo.Status().PasswordConfigured {
			if err := repo.VerifyPassword(configPassword); err != nil {
				return repo, false, err
			}
		}
		if !repo.Status().PasswordConfigured {
			if err := repo.ChangePassword("", configPassword, configPassword, providers...); err != nil {
				return repo, changed, err
			}
			changed = true
		}
		if repo.Status().RotationPending {
			if err := repo.ChangePassword(configPassword, "", "", providers...); err != nil {
				return repo, changed, err
			}
			changed = true
		}
		if automatic {
			if err := repo.RebindPasswordProvider(providers...); err != nil {
				return repo, changed, err
			}
			changed = true
		}
		return repo, changed, nil
	})
}

func (s *Store) UnlockAuthentication(password string, providers ...configstore.Provider) error {
	return s.passwordRepository(func(repo *configstore.Repository) (*configstore.Repository, bool, error) {
		if repo == nil {
			return nil, false, configstore.ErrNotInitialized
		}
		changed := false
		if repo.Status().Locked {
			if err := repo.UnlockPassword(password); err != nil {
				return repo, false, err
			}
			changed = true
		} else if err := repo.VerifyPassword(password); err != nil {
			return repo, false, err
		}
		if repo.Status().RotationPending {
			if err := repo.ChangePassword(password, "", "", providers...); err != nil {
				return repo, changed, err
			}
			changed = true
		}
		return repo, changed, nil
	})
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
	if s.closed || s.encrypted == nil {
		return true
	}
	state := s.encrypted.Status()
	return state.Closed || state.Locked || state.RotationPending
}

func (s *Store) AuthenticationInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encrypted != nil
}

func (s *Store) ReadLoginCredential() ([]byte, error) {
	s.mu.Lock()
	repo := s.encrypted
	s.mu.Unlock()
	if repo == nil {
		return nil, nil
	}
	return repo.ReadLoginCredential()
}
func (s *Store) WriteLoginCredential(expected, next []byte) error {
	s.mu.Lock()
	repo := s.encrypted
	s.mu.Unlock()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	return repo.WriteLoginCredential(expected, next)
}
func (s *Store) WriteAuthenticationAndLogin(data, login []byte) error {
	s.mu.Lock()
	repo := s.encrypted
	s.mu.Unlock()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	return repo.WriteAuthenticationAndLogin(data, login)
}
