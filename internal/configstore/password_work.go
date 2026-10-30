package configstore

import (
	"bytes"
	"errors"
	"path/filepath"
)

// ErrKeyringChanged asks a caller to retry against current key metadata. A
// password proof made from an earlier snapshot must not silently commit.
var ErrKeyringChanged = errors.New("configstore: keyring changed during password operation")

func (r *Repository) passwordDeriver() passwordDeriver {
	if r.derivePassword != nil {
		return r.derivePassword
	}
	return deriveMain
}

// passwordWorkLocked is entered and returned with mu held. Only independent
// snapshots may be used in work; Close and ordinary traffic can run meanwhile.
func (r *Repository) passwordWorkLocked(expected []byte, work func() error) error {
	err := func() error {
		r.mu.Unlock()
		defer r.mu.Lock()
		return work()
	}()
	if err != nil {
		return err
	}
	if r.closed {
		return ErrClosed
	}
	current, err := readBoundedFile(filepath.Join(r.dataDir, "keys", "config.keys"), maxKeyringFile)
	if err != nil {
		return err
	}
	defer clear(current)
	if !bytes.Equal(current, expected) {
		return ErrKeyringChanged
	}
	return nil
}

// passwordSnapshotLocked owns its own secret map so a concurrent Close cannot
// zero memory still used by provider work outside the repository lock.
func (r *Repository) passwordSnapshotLocked() (*Keyring, error) {
	store, keys, active, err := r.keyring.snapshot()
	if err != nil {
		return nil, err
	}
	return &Keyring{storeID: store, mainKeys: keys, activeKeyID: active, password: r.keyring.password}, nil
}
