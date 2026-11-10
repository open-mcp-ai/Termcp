package sshconfig

import (
	"github.com/open-mcp-ai/termcp/internal/configstore"
)

// storeState is the published, immutable view of the encryption side of a
// Store: the repository holding ciphertext and the preference for how a brand
// new profile is first persisted.
//
// Reads load the pointer; the operations that change it (enable, initialize,
// unlock, rotate) build the next value and store it. No lock: the repository
// itself already serializes its own writers, and a reader here only needs a
// coherent pair of fields.
type storeState struct {
	repo       *configstore.Repository
	encryptNew bool
}

func (s *Store) stateSnapshot() *storeState {
	if st := s.state.Load(); st != nil {
		return st
	}
	return &storeState{}
}

// repositorySnapshot returns the repository in use, or nil when encryption has
// never been enabled in this process.
func (s *Store) repositorySnapshot() *configstore.Repository {
	return s.stateSnapshot().repo
}

func (s *Store) setEncryption(repo *configstore.Repository, encryptNew bool) {
	s.state.Store(&storeState{repo: repo, encryptNew: encryptNew})
}

// setEncryptNew updates the preference, keeping the current repository.
func (s *Store) setEncryptPreference(encryptNew bool) {
	cur := s.stateSnapshot()
	s.state.Store(&storeState{repo: cur.repo, encryptNew: encryptNew})
}
