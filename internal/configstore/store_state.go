package configstore

// Repository coordination, lock-free:
//
//   - state is the published, immutable view every reader loads. A reader never
//     waits.
//   - writeTok is a 1-slot channel token: exactly one writer holds it, and it is
//     held across the disk work that publishes a new catalog generation. That is
//     the sanctioned primitive for per-resource serialization across I/O, and it
//     replaces a mutex whose critical section contained file writes.
//
// Every field the old mutex protected lives inside repoSnapshot: the catalog
// view (revision, generation, preference, refs, closed) and the key state
// (keyring, password derivation, unlock error, store id, automatic-unlock
// flag). Nothing in a snapshot is mutated after publication; a writer builds the
// next one and stores it.

type repoSnapshot struct {
	// Catalog view.
	catalogGeneration ID
	revision          uint64
	encryptNew        bool
	refs              map[string]catalogRef
	closed            bool

	// Key state.
	keyring         *Keyring
	password        *storedPasswordDerivation
	unlockErr       error
	lockedStoreID   ID
	automaticUnlock bool
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
// ready. Every constructor goes through it, including the short-lived probes
// recovery and password unlocking use to read the catalog with a candidate key.
func newRepository(dataDir string) *Repository {
	r := &Repository{
		dataDir:  dataDir,
		writeTok: make(chan struct{}, 1),
	}
	r.state.Store(&repoSnapshot{refs: make(map[string]catalogRef)})
	return r
}

// publishRefs publishes the catalog view a writer just committed. The catalog
// generation comes from the live snapshot because commitCatalog has already
// advanced it; revision and the encrypt preference come from the writer.
func (r *Repository) publishRefs(cur *repoSnapshot, refs map[string]catalogRef, encryptNew bool) {
	r.mutate(func(next *repoSnapshot) {
		next.revision = cur.revision + 1
		next.encryptNew = encryptNew
		next.refs = refs
	})
}

// setRevision advances the published revision without touching the catalog
// generation: commitCatalog has just published the generation this revision
// belongs to.
func (r *Repository) setRevision(revision uint64) {
	r.mutate(func(next *repoSnapshot) {
		next.revision = revision
	})
}

// storeKeyring publishes the keyring a constructor just created.
func (r *Repository) storeKeyring(keyring *Keyring, storeID ID) {
	r.mutate(func(next *repoSnapshot) {
		next.keyring = keyring
		next.lockedStoreID = storeID
	})
}

func (r *Repository) publish(next *repoSnapshot) {
	r.state.Store(next)
}
