package configstore

/* There is no index.
 *
 * A host is a directory: `ssh_configs/<name>/` holds its plaintext
 * `config.toml`, or exactly one `<generation>.enc` (plus that generation's
 * `.adk` when its key has a passphrase, and an `active` marker when its second
 * layer is on for a key without one). Name, object id, generation id and the
 * sealing key's id are all published by that directory and the envelope header,
 * so a global mapping would only be a second copy that could disagree.
 *
 * That is also what makes one unplugged key a local problem: with no shared
 * index there is no single key that every host's listing and saving depends on.
 *
 * The previous format kept all of this in `store.enc`, an envelope sealed by one
 * "index owner" key. `migrateFromLegacyIndex` reads it once, converts its
 * activation booleans into per-host markers, and deletes it.
 */

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

/* catalogRef is one committed host: its directory name, its stable object id and
 * the generation currently committed, plus the key that seals it.
 *
 * `keyID` is new: it comes from the envelope's public header, so a host can say
 * which key it needs without any index and without that key being plugged in.
 * `activated` is the effective second layer, recomputed from the host's own
 * `.adk` / `active` files rather than stored in a global index.
 */
type catalogRef struct {
	name         string
	objectID     ID
	generationID ID
	keyID        ID
	activated    bool
}

// hostDir returns one host's directory under ssh_configs.
func hostDir(dataDir, name string) string {
	return filepath.Join(dataDir, "ssh_configs", name)
}

/* hasCiphertext reports whether any host directory holds a committed generation.
 *
 * It is the one fact that separates "this store was never encrypted" from "its
 * keychain is gone": without an index there is nothing else on disk that says so.
 */
func hasCiphertext(dataDir string) bool {
	entries, err := os.ReadDir(hostsRoot(dataDir))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(hostsRoot(dataDir), entry.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".enc") {
				return true
			}
		}
	}
	return false
}

// hostsRoot returns the directory that holds one directory per host.
func hostsRoot(dataDir string) string {
	return filepath.Join(dataDir, "ssh_configs")
}

// objectPath returns the ciphertext file of one generation.
func (r *Repository) objectPath(ref catalogRef) string {
	return filepath.Join(hostDir(r.dataDir, ref.name), hex.EncodeToString(ref.generationID[:])+".enc")
}

// legacyObjectPath is the pre-directory layout: objects/<object>/<generation>.
func (r *Repository) legacyObjectPath(ref catalogRef) string {
	return filepath.Join(hostsRoot(r.dataDir), "objects",
		hex.EncodeToString(ref.objectID[:]), hex.EncodeToString(ref.generationID[:])+".enc")
}

/* discoverRefs enumerates committed hosts from disk, with no key material.
 *
 * The envelope header is public, so this needs only the store id: it can list
 * every host and say which key each one needs even when nothing is plugged in.
 * A directory that cannot be parsed as a committed host is skipped, not fatal —
 * a half-written directory must not take the other hosts down with it.
 */
func (r *Repository) discoverRefs() (map[string]catalogRef, error) {
	entries, err := os.ReadDir(hostsRoot(r.dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]catalogRef{}, nil
	}
	if err != nil {
		return nil, err
	}
	st := r.snap()
	if st.keyring == nil {
		return nil, ErrNotInitialized
	}
	storeID := st.keyring.StoreID()
	refs := make(map[string]catalogRef, len(entries))
	folded := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validateCatalogName(entry.Name()) != nil {
			continue
		}
		name := entry.Name()
		if previous, ok := folded[strings.ToLower(name)]; ok {
			return nil, errNameCollision(previous, name)
		}
		ref, found, err := r.scanHostDir(name, storeID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		folded[strings.ToLower(name)] = name
		refs[name] = ref
	}
	return refs, nil
}

/* scanHostDir reads one host directory and decides its committed generation.
 *
 * Exactly one `.enc` is the normal state. Two mean a crash between publishing
 * the new generation and removing the old one; the winner is the one whose
 * header matches the object id and which is newest by name order, because
 * generation ids are random, not ordered. A directory with more than one is
 * therefore NOT silently resolved: it is reported as ambiguous, since picking
 * one could discard a committed save.
 */
func (r *Repository) scanHostDir(name string, storeID ID) (catalogRef, bool, error) {
	dir := hostDir(r.dataDir, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return catalogRef{}, false, nil
	}
	type candidate struct {
		ref  catalogRef
		path string
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".enc") {
			continue
		}
		raw, err := readBoundedFile(filepath.Join(dir, entry.Name()),
			int64(envelopeHeaderSize+MaxProfilePlaintext+16))
		if err != nil {
			continue
		}
		objectID, generationID, ok := envelopeIdentity(raw, storeID, KindProfile)
		if !ok {
			continue
		}
		keyID, ok := envelopeKeyID(raw)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{
			ref:  catalogRef{name: name, objectID: objectID, generationID: generationID, keyID: keyID},
			path: filepath.Join(dir, entry.Name()),
		})
	}
	if len(candidates) == 0 {
		return catalogRef{}, false, nil
	}
	if len(candidates) > 1 {
		return catalogRef{}, false, errAmbiguousGenerations(name, len(candidates))
	}
	return candidates[0].ref, true, nil
}

/* loadRefs publishes the disk view and computes each host's second layer.
 *
 * Reading activation needs the keychain file (to know whether a key carries a
 * passphrase) but no key material: a passphrase host simply comes back inactive
 * until its passphrase is typed.
 */
func (r *Repository) loadRefs() error {
	refs, err := r.discoverRefs()
	if err != nil {
		return err
	}
	stored, err := r.readKeyringFile()
	if err != nil && !errors.Is(err, ErrNotInitialized) {
		return err
	}
	for name, ref := range refs {
		ref.activated = r.generationActivated(ref, stored)
		refs[name] = ref
	}
	r.mutate(func(next *repoSnapshot) { next.refs = refs })
	return nil
}

// listRefs returns committed names in lexical order, refreshing from disk first.
func (r *Repository) listRefs() ([]string, error) {
	if err := r.loadRefs(); err != nil {
		return nil, err
	}
	st := r.snap()
	names := make([]string, 0, len(st.refs))
	for name := range st.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func errNameCollision(a, b string) error {
	return &collisionError{a: a, b: b}
}

type collisionError struct{ a, b string }

func (e *collisionError) Error() string {
	return "configstore: profile directories " + e.a + " and " + e.b + " collide"
}

func (e *collisionError) Is(target error) bool { return target == ErrExists }

func errAmbiguousGenerations(name string, count int) error {
	return &ambiguousError{name: name, count: count}
}

type ambiguousError struct {
	name  string
	count int
}

func (e *ambiguousError) Error() string {
	return "configstore: profile " + e.name + " has " + itoa(e.count) + " committed generations"
}

func (e *ambiguousError) Is(target error) bool { return target == ErrFormat }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
