package configstore

import (
	"crypto/rand"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

/* The second layer's repository half: activating and deactivating hosts, and the
 * `.adk` sidecar that records which generation was activated with which
 * parameters.
 *
 * Activation is a batch operation because a person picks hosts, not keys. The batch
 * is all-or-nothing: a selection whose keys are not all plugged in, or whose
 * passphrases do not cover every key that needs one, changes nothing at all. That
 * is what makes "5 hosts, 2 passphrases, one click" trustworthy — there is no state
 * in which half the selection was activated and the other half was not.
 */

// activationPath is the `.adk` beside the generation it belongs to: same directory,
// same base name, different suffix.
func (r *Repository) activationPath(ref catalogRef) string {
	return filepath.Join(
		r.dataDir,
		"ssh_configs",
		ref.name,
		strings.TrimSuffix(filepath.Base(r.objectPath(ref)), ".enc")+".adk",
	)
}

func (r *Repository) profileIdentity(ref catalogRef) Identity {
	return Identity{
		StoreID:      r.snap().keyring.StoreID(),
		ObjectID:     ref.objectID,
		GenerationID: ref.generationID,
		Kind:         KindProfile,
	}
}

// readActivation loads one generation's `.adk`. A missing file is the honest
// answer "this host needs no passphrase", so the error is the ordinary os one.
func (r *Repository) readActivation(ref catalogRef) (activationRecord, error) {
	return ReadActivation(r.activationPath(ref), r.profileIdentity(ref))
}

// activationForSave decides a new generation's second layer.
//
// Three cases, and the difference between them is the whole point of two layers:
//
//   - the caller supplied a passphrase: derive from a fresh salt, record the host as
//     activated, and write a `.adk` bound to the new body;
//   - the key carries a passphrase and this process already holds the host's
//     activation key: keep it, and re-record the `.adk` against the new body.
//     Re-saving does not ask for the passphrase again, and it does not silently drop
//     the second layer;
//   - the key has no passphrase: the second layer is the catalog boolean, a save by
//     a person sets it, and there is no `.adk` because there is no secret.
func (r *Repository) activationForSave(ref catalogRef, keyID ID, key storedKey, options SaveOptions) ([32]byte, bool, activationRecord, error) {
	var none [32]byte
	var record activationRecord
	if _, plugged := r.snap().keyring.key(keyID); !plugged {
		return none, false, record, ErrKeyNotLoaded
	}
	if options.Passphrase != "" {
		parameters := defaultActivationParameters()
		if err := parameters.validate(); err != nil {
			return none, false, record, err
		}
		salt, err := newActivationSalt()
		if err != nil {
			return none, false, record, err
		}
		activationKey, err := DeriveActivationKey(options.Passphrase, salt, parameters)
		if err != nil {
			return none, false, record, err
		}
		record, err = newActivation(r.profileIdentity(ref), parameters, salt, activationKey)
		if err != nil {
			clear(activationKey[:])
			return none, false, record, err
		}
		return activationKey, true, record, nil
	}
	if !keyHasPassphrase(key) {
		/* No secret to gate on: the boolean is the layer, and a save by a person
		   creates the host activated (§2.2). */
		return none, true, record, nil
	}
	existing, ok := r.snap().keyring.activationKey(keyID, ref.objectID)
	if !ok {
		/* The host needs a passphrase and this process does not hold its activation
		   key. Writing the generation anyway would produce a body nobody can open —
		   the downgrade §8.2 forbids — so the save fails instead. */
		return none, false, record, ErrPassphraseRequired
	}
	/* Reuse needs the ORIGINAL salt and cost, or the `.adk` written here would name
	   parameters that do not reproduce the key this body is sealed with. They live
	   in the previous generation's file, which is the only place they exist. */
	previousRef, known := r.snap().refs[ref.name]
	if known && !previousRef.generationID.IsZero() && previousRef.generationID != ref.generationID {
		previous, err := r.readActivation(previousRef)
		if err == nil {
			record, err = newActivation(r.profileIdentity(ref), previous.parameters, previous.salt, existing)
			if err != nil {
				return none, false, record, err
			}
			return existing, true, record, nil
		}
	}
	/* No previous `.adk` to copy from (a brand-new host under a passphrase key whose
	   activation key is somehow already held): mint a fresh salt for the same key. */
	salt, err := newActivationSalt()
	if err != nil {
		return none, false, record, err
	}
	parameters := defaultActivationParameters()
	record, err = newActivation(r.profileIdentity(ref), parameters, salt, existing)
	if err != nil {
		return none, false, record, err
	}
	return existing, true, record, nil
}

/* verifyPassphrase proves a passphrase against something this key already sealed.
 *
 * A key that carries a passphrase keeps no verifier for it, and that is deliberate:
 * a stored verifier is an offline oracle for guessing. The sealed hosts are the
 * verifier instead — each has its own salt, and opening its `.adk` proves the
 * passphrase without writing anything. A key with no sealed host yet has nothing to
 * compare against, and the first sealing defines the passphrase.
 *
 * Called by a save that is establishing a NEW sealing (VerifyPassphrase); changing a
 * passphrase goes the other way, away from every record on disk.
 */
func (r *Repository) verifyPassphrase(keyID ID, passphrase string) error {
	cur := r.snap()
	checked := 0
	for _, name := range r.hostsForKey(keyID, 0) {
		ref, ok := cur.refs[name]
		if !ok {
			continue
		}
		record, err := r.readActivation(ref)
		if err != nil {
			/* A host with no record cannot prove anything; the next one may. */
			continue
		}
		raw, err := r.readObjectRaw(ref)
		if err != nil {
			continue
		}
		checked++
		activationKey, err := DeriveActivationKey(passphrase, record.salt, record.parameters)
		if err != nil {
			return err
		}
		err = record.verify(activationKey, raw[envelopeHeaderSize:])
		clear(activationKey[:])
		if err == nil {
			return nil
		}
	}
	if checked == 0 {
		return nil
	}
	return fmt.Errorf("%w: key %s", ErrPassphraseWrong, shortID(keyID))
}

func newActivationSalt() ([16]byte, error) {
	var salt [16]byte
	if _, err := io.ReadFull(rand.Reader, salt[:]); err != nil {
		return salt, fmt.Errorf("generate activation salt: %w", err)
	}
	return salt, nil
}

// writeActivation binds the record to the body ciphertext just written, so the
// marker cannot be moved onto another generation, object, or host.
func (r *Repository) writeActivation(ref catalogRef, record activationRecord, activationKey [32]byte) error {
	raw, err := r.readObjectRaw(ref)
	if err != nil {
		return err
	}
	if err := record.bind(activationKey, raw[envelopeHeaderSize:]); err != nil {
		return err
	}
	return WriteActivation(r.activationPath(ref), record)
}

/* Activate turns on the second layer for a selection of hosts.
 *
 * It is one batch and it is all-or-nothing (§8.5). The order below is the contract:
 *
 *  1. Decide every host first. A host whose key is not plugged in refuses the whole
 *     batch and changes nothing.
 *  2. Require a passphrase for every key in the selection that has one. A missing
 *     one refuses the batch and names the keys, so the caller knows what to type.
 *  3. Derive each host's activation key from ITS OWN salt — M hosts under one key
 *     are M derivations and M different keys.
 *  4. Verify each against its `.adk`. A failure refuses the batch and names the KEY,
 *     never the host: one passphrase belongs to a whole key, so naming a host would
 *     be a claim about something that is not what failed.
 *
 * Keys without a passphrase need nothing: for them activation is the per-host
 * `active` marker, and a mixed selection is allowed.
 */
func (r *Repository) Activate(names []string, passphrases map[ID]string) (map[string]error, error) {
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return nil, err
	}
	if err := r.loadRefs(); err != nil {
		return nil, err
	}
	cur := r.snap()
	type target struct {
		name      string
		ref       catalogRef
		key       storedKey
		keyID     ID
		needsPass bool
		record    activationRecord
	}
	targets := make([]target, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if err := validateCatalogName(name); err != nil {
			return nil, err
		}
		if _, duplicate := seen[strings.ToLower(name)]; duplicate {
			continue
		}
		seen[strings.ToLower(name)] = struct{}{}
		ref, ok := cur.refs[name]
		if !ok {
			return nil, fmt.Errorf("%q: %w", name, ErrNotFound)
		}
		raw, err := r.readObjectRaw(ref)
		if err != nil {
			return nil, err
		}
		keyID, ok := envelopeKeyID(raw)
		if !ok {
			return nil, ErrFormat
		}
		if _, plugged := cur.keyring.key(keyID); !plugged {
			/* Step 1: a host nobody can read is not a host to activate. Refusing
			   here — before any KDF — is also what keeps a wrong passphrase from
			   being reported for a host whose key was never there. */
			return nil, fmt.Errorf("%w: %s", ErrKeyNotLoaded, name)
		}
		entry := target{name: name, ref: ref, keyID: keyID, key: storedKeyForKey(r, keyID)}
		if keyHasPassphrase(entry.key) {
			record, err := r.readActivation(ref)
			if err != nil {
				/* A key that says it has a passphrase must have a record to verify
				   against; a missing one is a damaged generation, not a host to
				   activate on trust. */
				return nil, fmt.Errorf("%q: %w", name, err)
			}
			entry.needsPass = true
			entry.record = record
		}
		targets = append(targets, entry)
	}
	if len(targets) == 0 {
		return map[string]error{}, nil
	}

	/* Step 2: every passphrase-bearing key in the selection must be covered. The
	   message lists the keys by short id, because that is what the panel shows and
	   what the caller can act on. */
	missing := make([]string, 0)
	required := make(map[ID]struct{})
	for _, entry := range targets {
		if !entry.needsPass {
			continue
		}
		if _, given := passphrases[entry.keyID]; given {
			continue
		}
		if _, already := required[entry.keyID]; already {
			continue
		}
		required[entry.keyID] = struct{}{}
		missing = append(missing, shortID(entry.keyID))
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%w: %s", ErrPassphraseRequired, strings.Join(missing, ", "))
	}

	/* Step 3 and 4: derive and verify every host before publishing anything, so a
	   failure leaves the store exactly as it was. */
	derived := make(map[string][32]byte, len(targets))
	for _, entry := range targets {
		if !entry.needsPass {
			continue
		}
		activationKey, err := DeriveActivationKey(passphrases[entry.keyID], entry.record.salt, entry.record.parameters)
		if err != nil {
			return nil, err
		}
		raw, err := r.readObjectRaw(entry.ref)
		if err != nil {
			clear(activationKey[:])
			return nil, err
		}
		if err := entry.record.verify(activationKey, raw[envelopeHeaderSize:]); err != nil {
			clear(activationKey[:])
			return nil, fmt.Errorf("%w: key %s", ErrPassphraseWrong, shortID(entry.keyID))
		}
		derived[entry.name] = activationKey
	}

	/* Every host was decided and verified above, so publishing cannot fail on a
	   wrong passphrase. A host whose key has no passphrase gets its own marker;
	   one with a passphrase needs nothing on disk, only the key in memory. */
	next := cloneRefs(cur.refs)
	for _, entry := range targets {
		ref := next[entry.name]
		if !entry.needsPass {
			ref.keyID = entry.keyID
			if err := r.writeActiveMarker(ref); err != nil {
				return nil, err
			}
		}
		ref.activated = true
		next[entry.name] = ref
	}
	r.mutate(func(s *repoSnapshot) { s.refs = next })
	results := make(map[string]error, len(targets))
	for _, entry := range targets {
		activationKey, ok := derived[entry.name]
		if !ok {
			/* A key without a passphrase: nothing to hold, the boolean is the layer. */
			results[entry.name] = nil
			continue
		}
		r.snap().keyring.withActivationKey(entry.keyID, entry.ref.objectID, activationKey)
		clear(activationKey[:])
		results[entry.name] = nil
	}
	return results, nil
}

/* Deactivate drops the second layer for a selection. It touches no ciphertext and
 * derives nothing: the body key is computed per read, so forgetting the activation
 * key (or clearing the boolean) is the whole operation (§8.5 step 7).
 */
func (r *Repository) Deactivate(names []string) error {
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return err
	}
	if err := r.loadRefs(); err != nil {
		return err
	}
	cur := r.snap()
	next := cloneRefs(cur.refs)
	dropped := make(map[ID]struct{}, len(names))
	changed := false
	for _, name := range names {
		if err := validateCatalogName(name); err != nil {
			return err
		}
		ref, ok := next[name]
		if !ok {
			return fmt.Errorf("%q: %w", name, ErrNotFound)
		}
		if !ref.activated {
			continue
		}
		ref.activated = false
		if err := r.clearActiveMarker(ref); err != nil {
			return err
		}
		next[name] = ref
		dropped[ref.objectID] = struct{}{}
		changed = true
	}
	if !changed {
		return nil
	}
	r.mutate(func(s *repoSnapshot) { s.refs = next })
	cur.keyring.withoutActivationKeys(dropped)
	return nil
}

func shortID(id ID) string {
	text := fmt.Sprintf("%x", id[:])
	if len(text) > 8 {
		return text[:8]
	}
	return text
}

/* HostState is one committed profile's non-secret storage facts: which key seals
 * it, whether its second layer is on right now, whether it needs a passphrase at
 * all, and whether its key is gone. The panel renders exactly this, and the read
 * path answers from the same three facts — one spelling per concept.
 */
type HostState struct {
	// KeyID names the Main Key that sealed this generation.
	KeyID ID
	// Activated is the effective second-layer state: for a key without a passphrase
	// it is the host's `active` marker, for one with a passphrase it is "the
	// activation key is in memory".
	Activated bool
	// NeedsPassphrase reports that activating this host requires its key's
	// passphrase.
	NeedsPassphrase bool
	// Orphaned reports ciphertext whose key is no longer in the keychain: it can
	// never be read again.
	Orphaned bool
	// Plugged reports that the sealing key's material is in memory.
	Plugged bool
}

// HostState answers for one committed profile. A name that is not committed (a
// plaintext host, or one whose ciphertext no index lists) is ErrNotFound.
func (r *Repository) HostState(name string) (HostState, error) {
	var state HostState
	st := r.snap()
	if st.keyring == nil {
		return state, ErrNotInitialized
	}
	ref, ok := st.refs[name]
	if !ok {
		return state, ErrNotFound
	}
	raw, err := r.readObjectRaw(ref)
	if err != nil {
		return state, err
	}
	keyID, ok := envelopeKeyID(raw)
	if !ok {
		return state, ErrFormat
	}
	state.KeyID = keyID
	_, state.Plugged = st.keyring.key(keyID)
	stored, err := r.readKeyringFile()
	if err != nil {
		return state, err
	}
	key, known := storedKeyByID(stored, keyID)
	if !known {
		state.Orphaned = true
		return state, nil
	}
	state.NeedsPassphrase = keyHasPassphrase(key)
	if !state.NeedsPassphrase {
		state.Activated = ref.activated
		return state, nil
	}
	_, state.Activated = st.keyring.activationKey(keyID, ref.objectID)
	return state, nil
}
