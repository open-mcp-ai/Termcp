package configstore

// Repository coordination, lock-free:
//
//   - state is the published, immutable view every reader loads. A reader never
//     waits.
//   - writeTok is a 1-slot channel token: exactly one writer holds it, across the
//     disk work that publishes a host's new generation. That is the sanctioned
//     primitive for per-resource serialization across I/O, and it replaces a
//     mutex whose critical section contained file writes.
//
// There is no catalog generation or index owner any more. The snapshot carries
// the last disk view (refs) and the preferences; each host's committed state is
// its own directory, so nothing here has to be rewritten when one host changes.

type repoSnapshot struct {
	encryptNew bool
	// masked is the global address/username masking switch. It is a display-layer
	// preference, readable with nothing plugged in.
	masked bool
	refs   map[string]catalogRef
	closed bool

	keyring *Keyring
	// loadErr reports why the keychain could not be used. It is nil when the
	// keychain is simply absent, because that means "no encryption", not "broken".
	loadErr error
}

func (r *Repository) snap() *repoSnapshot {
	if r == nil {
		return &repoSnapshot{closed: true}
	}
	st := r.state.Load()
	if st == nil {
		return &repoSnapshot{closed: true}
	}
	return st
}

// acquireWrite takes the writer token and returns the release function.
func (r *Repository) acquireWrite() func() {
	r.writeTok <- struct{}{}
	return func() { <-r.writeTok }
}

// mutate publishes a copy of the current snapshot with fn applied. Only a
// holder of the writer token may call it, so two writers cannot interleave.
func (r *Repository) mutate(fn func(next *repoSnapshot)) {
	next := *r.snap()
	fn(&next)
	r.state.Store(&next)
}

// newRepository returns a repository whose writer token and published view are
// ready. Every constructor goes through it.
func newRepository(dataDir string) *Repository {
	r := &Repository{
		dataDir:  dataDir,
		writeTok: make(chan struct{}, 1),
	}
	r.state.Store(&repoSnapshot{refs: make(map[string]catalogRef)})
	return r
}

// storeKeyring publishes the keyring a constructor or a layout change produced.
func (r *Repository) storeKeyring(keyring *Keyring) {
	r.mutate(func(next *repoSnapshot) {
		next.keyring = keyring
		next.loadErr = nil
	})
}
