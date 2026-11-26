package configstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* The activation layer, tested against the rows of the design's acceptance matrix
 * (§12) that this package owns:
 *
 *   - only the key plugged in, host not activated
 *   - only the passphrase, key not plugged in
 *   - A's `.adk` spliced onto B's body
 *   - one activation across two keys that both carry a passphrase
 *   - an activation request missing one key's passphrase
 *   - restart: `plugged` holds, passphrase hosts fall back to INACTIVE
 */

const testPassphrase = "synthetic passphrase for tests"

// passphraseRepository returns a repository whose only key carries a passphrase,
// plus that key's id and a host sealed under it.
func passphraseRepository(t *testing.T, kek byte, host, body string) (*Repository, ID, string) {
	t.Helper()
	t.Setenv(EnvKEKVariable, environmentKEK(kek))
	dir := t.TempDir()
	phrase := testPhrase(t)
	storeID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	// The repository is created from a keychain whose single key has a passphrase,
	// which is what makes its hosts need the second layer.
	seed, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := CreateKeyring(storeID, phrase, testPassphrase, "test-key", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	_ = seed
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "config.keys"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	keys, err := repo.Keys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keychain = %+v, %v", keys, err)
	}
	if !keys[0].Passphrase {
		t.Fatal("the key lost its passphrase flag")
	}
	var keyID ID
	if err := keyID.UnmarshalText([]byte(keys[0].ID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWithOptions(host, []byte(body), SaveOptions{KeyID: keyID, Passphrase: testPassphrase}); err != nil {
		t.Fatalf("creating a passphrase host: %v", err)
	}
	return repo, keyID, dir
}

/* Only the key is plugged in. The DEK is unwrapped, but the body key needs the
 * activation key, so the host is not readable — and the answer says "activate",
 * not "load a key". (§12 "只插钥匙、不激活")
 */
func TestOnlyTheKeyPluggedInIsNotEnough(t *testing.T) {
	repo, keyID, _ := passphraseRepository(t, 0xb1, "alpha", "host = \"a.example\"\n")
	// A fresh open has no activation keys at all: the passphrase was never on disk.
	if err := repo.Deactivate([]string{"alpha"}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := repo.Read("alpha"); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("reading a host that is not activated = %v; want ErrNotActivated", err)
	}
	// The DEK is reachable — the key is plugged in — but it is not the body key.
	ref := repo.snap().refs["alpha"]
	raw, err := repo.readObjectRaw(ref)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Parse(raw, repo.profileIdentity(ref))
	if err != nil {
		t.Fatal(err)
	}
	main, ok := repo.snap().keyring.key(keyID)
	if !ok {
		t.Fatal("the sealing key is not plugged in")
	}
	// Opening with the DEK and the no-passphrase stand-in is exactly what an
	// attacker holding the key file alone can do. It must fail.
	if _, err := envelope.Open(main, noActivationKey); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("the DEK alone opened an activated body: %v", err)
	}
	// With the right second layer it opens.
	if _, err := repo.Activate([]string{"alpha"}, map[ID]string{keyID: testPassphrase}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if data, err := repo.Read("alpha"); err != nil || !strings.Contains(string(data), "a.example") {
		t.Fatalf("read after activating: %q, %v", data, err)
	}
}

/* Re-saving a host under a key with no passphrase has to leave it activated. The
 * marker is written for the NEW generation and the OLD generation is cleaned up
 * right after it, and that cleanup is where this broke: the marker is one file per
 * host, so removing "the previous generation's marker" deleted the state the save
 * had just recorded. The only symptom was a later restore or activation failing
 * with host_not_activated.
 *
 * Mutation check: remove the marker in removeObject and the read below fails.
 */
func TestResaveKeepsAPassphraselessHostActivated(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0xb2))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	body := []byte("host = \"a.example\"\n")
	if err := repo.Save("alpha", body); err != nil {
		t.Fatal(err)
	}
	if ref := repo.snap().refs["alpha"]; !ref.activated {
		t.Fatal("a host saved for the first time is expected to be activated")
	}
	keyID, ok := repo.snap().keyring.anySealableKey()
	if !ok {
		t.Fatal("no key plugged in")
	}
	if err := repo.SaveWithKey("alpha", append(body, []byte("user = \"u\"\n")...), keyID); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if ref := repo.snap().refs["alpha"]; !ref.activated {
		t.Fatal("re-saving a host under a key with no passphrase dropped its activation")
	}
	if _, err := repo.Read("alpha"); err != nil {
		t.Fatalf("read after re-saving: %v", err)
	}
	/* And the previous generation must really be gone: a store that kept both files
	   would still read, but it would also have two committed generations. */
	entries, err := os.ReadDir(filepath.Join(dir, "ssh_configs", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	generations := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".enc") {
			generations++
		}
	}
	if generations != 1 {
		t.Fatalf("committed generations in the host directory = %d; want 1", generations)
	}
}

/* Only the passphrase, with the key not plugged in. The activation key can be
 * computed from the `.adk`, but the DEK cannot be unwrapped, so the body is out of
 * reach — the other half of the AND. (§12 "只有口令、钥匙未插入")
 */
func TestOnlyThePassphraseIsNotEnough(t *testing.T) {
	repo, keyID, _ := passphraseRepository(t, 0xb2, "alpha", "host = \"b.example\"\n")
	ref := repo.snap().refs["alpha"]
	record, err := repo.readActivation(ref)
	if err != nil {
		t.Fatalf("reading the activation record: %v", err)
	}
	activationKey, err := DeriveActivationKey(testPassphrase, record.salt, record.parameters)
	if err != nil {
		t.Fatalf("deriving the activation key: %v", err)
	}
	if err := repo.UnplugKey(keyID); err != nil {
		t.Fatalf("unplug: %v", err)
	}
	// The passphrase still derives the same activation key, and it verifies against
	// the record — that is not enough to read anything.
	raw, err := repo.readObjectRaw(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := record.verify(activationKey, raw[envelopeHeaderSize:]); err != nil {
		t.Fatalf("the derived key does not verify against its own record: %v", err)
	}
	if _, err := repo.Read("alpha"); !errors.Is(err, ErrKeyNotLoaded) {
		t.Fatalf("reading with the key unplugged = %v; want ErrKeyNotLoaded", err)
	}
	envelope, err := Parse(raw, repo.profileIdentity(ref))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envelope.Open(testMainKey(0x01), activationKey); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("the activation key alone opened the body: %v", err)
	}
}

/* Splicing A's `.adk` onto B's body must fail hard: the AAD binds the header, the
 * object and generation ids, and the body digest. (§12 "把 A 的 .adk 拼到 B 的正文上")
 */
func TestActivationRecordCannotBeSplicedOntoAnotherBody(t *testing.T) {
	repo, keyID, _ := passphraseRepository(t, 0xb3, "alpha", "host = \"a.example\"\n")
	if err := repo.SaveWithOptions("beta", []byte("host = \"b.example\"\n"), SaveOptions{KeyID: keyID, Passphrase: testPassphrase}); err != nil {
		t.Fatalf("second host: %v", err)
	}
	// The two records name different objects, so the identity check already refuses
	// the splice before any KDF runs.
	alpha, beta := repo.snap().refs["alpha"], repo.snap().refs["beta"]
	alphaRecord, err := repo.readActivation(alpha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseActivation(alphaRecord.Bytes(), repo.profileIdentity(beta)); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("A's record parsed as B's = %v; want ErrIdentityMismatch", err)
	}
	// Even with matching identities, the body digest in the AAD refuses it: bind the
	// same record to A's ciphertext, then verify it against B's.
	betaRaw, err := repo.readObjectRaw(beta)
	if err != nil {
		t.Fatal(err)
	}
	activationKey, err := DeriveActivationKey(testPassphrase, alphaRecord.salt, alphaRecord.parameters)
	if err != nil {
		t.Fatal(err)
	}
	if err := alphaRecord.verify(activationKey, betaRaw[envelopeHeaderSize:]); !errors.Is(err, ErrPassphraseWrong) {
		t.Fatalf("A's record verified against B's body = %v; want a hard failure", err)
	}
}

/* One activation request, two keys that both carry a passphrase: one passphrase
 * each, and every host under one key gets its own activation key because each has
 * its own salt. (§12 "一次激活跨两把带口令的钥匙")
 */
func TestOneActivationAcrossTwoPassphraseKeys(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0xb4))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	first, err := repo.CreateKeyWithLabel(testPhrase(t), testPassphrase, "first", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.CreateKeyWithLabel(testPhrase(t), "another synthetic passphrase", "second", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	var firstID, secondID ID
	if err := firstID.UnmarshalText([]byte(first.ID)); err != nil {
		t.Fatal(err)
	}
	if err := secondID.UnmarshalText([]byte(second.ID)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a1", "a2"} {
		if err := repo.SaveWithOptions(name, []byte("host = \""+name+".example\"\n"), SaveOptions{KeyID: firstID, Passphrase: testPassphrase}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SaveWithOptions("b1", []byte("host = \"b1.example\"\n"), SaveOptions{KeyID: secondID, Passphrase: "another synthetic passphrase"}); err != nil {
		t.Fatal(err)
	}
	// Deactivate everything so the batch has real work to do.
	if err := repo.Deactivate([]string{"a1", "a2", "b1"}); err != nil {
		t.Fatal(err)
	}

	results, err := repo.Activate([]string{"a1", "a2", "b1"}, map[ID]string{
		firstID:  testPassphrase,
		secondID: "another synthetic passphrase",
	})
	if err != nil {
		t.Fatalf("one activation across two keys: %v", err)
	}
	for _, name := range []string{"a1", "a2", "b1"} {
		if err, present := results[name]; present && err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := repo.Read(name); err != nil {
			t.Fatalf("reading %s after the batch: %v", name, err)
		}
	}
	// Same key, two hosts, two different activation keys: one passphrase, one salt
	// per host.
	a1Key, ok := repo.snap().keyring.activationKey(firstID, repo.snap().refs["a1"].objectID)
	if !ok {
		t.Fatal("a1 has no activation key")
	}
	a2Key, ok := repo.snap().keyring.activationKey(firstID, repo.snap().refs["a2"].objectID)
	if !ok {
		t.Fatal("a2 has no activation key")
	}
	if a1Key == a2Key {
		t.Fatal("two hosts under one key share an activation key; the salts must differ")
	}
}

/* A request that forgets one key's passphrase refuses the WHOLE batch and names the
 * keys by short id. Nothing is activated, not even the hosts whose passphrase was
 * supplied. (§12 "请求漏了某把钥匙的口令")
 */
func TestActivationMissingAPassphraseRefusesTheBatch(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0xb5))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	covered, err := repo.CreateKeyWithLabel(testPhrase(t), testPassphrase, "covered", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	forgotten, err := repo.CreateKeyWithLabel(testPhrase(t), "unasked synthetic passphrase", "forgotten", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	var coveredID, forgottenID ID
	if err := coveredID.UnmarshalText([]byte(covered.ID)); err != nil {
		t.Fatal(err)
	}
	if err := forgottenID.UnmarshalText([]byte(forgotten.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWithOptions("good", []byte("host = \"good.example\"\n"), SaveOptions{KeyID: coveredID, Passphrase: testPassphrase}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWithOptions("bad", []byte("host = \"bad.example\"\n"), SaveOptions{KeyID: forgottenID, Passphrase: "unasked synthetic passphrase"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Deactivate([]string{"good", "bad"}); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Activate([]string{"good", "bad"}, map[ID]string{coveredID: testPassphrase})
	if !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("activating without one key's passphrase = %v; want ErrPassphraseRequired", err)
	}
	if !strings.Contains(err.Error(), shortID(forgottenID)) {
		t.Fatalf("error does not name the missing key %s: %v", shortID(forgottenID), err)
	}
	if strings.Contains(err.Error(), shortID(coveredID)) {
		t.Fatalf("error names a key whose passphrase WAS supplied: %v", err)
	}
	// Nothing changed: the host whose passphrase was supplied is still inactive.
	for _, name := range []string{"good", "bad"} {
		if ref := repo.snap().refs[name]; ref.activated {
			t.Fatalf("%s was activated by a refused batch", name)
		}
		if _, err := repo.Read(name); !errors.Is(err, ErrNotActivated) {
			t.Fatalf("reading %s after a refused batch = %v", name, err)
		}
	}
	// A wrong passphrase refuses the batch too, and names the key rather than a host.
	_, err = repo.Activate([]string{"good", "bad"}, map[ID]string{
		coveredID:   "not the passphrase",
		forgottenID: "unasked synthetic passphrase",
	})
	if !errors.Is(err, ErrPassphraseWrong) {
		t.Fatalf("activating with a wrong passphrase = %v; want ErrPassphraseWrong", err)
	}
	if strings.Contains(err.Error(), "good") || strings.Contains(err.Error(), "bad") {
		t.Fatalf("a passphrase error named a host: %v", err)
	}
	if !strings.Contains(err.Error(), shortID(coveredID)) {
		t.Fatalf("a passphrase error did not name the key: %v", err)
	}
}

/* Restart keeps `plugged` and drops the passphrase layer, while a key without a
 * passphrase keeps its hosts ACTIVE. (§12 "重启后 plugged 保持 / 有口令主机回落到未激活")
 */
func TestRestartKeepsPluggedAndDropsThePassphraseLayer(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0xb6))
	dir := t.TempDir()
	repo, err := InitializeRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	open, err := repo.CreateKeyWithLabel(testPhrase(t), "", "open", NewEnvProvider(""))
	if err != nil {
		t.Fatalf("key without a passphrase: %v", err)
	}
	guarded, err := repo.CreateKeyWithLabel(testPhrase(t), testPassphrase, "guarded", NewEnvProvider(""))
	if err != nil {
		t.Fatalf("key with a passphrase: %v", err)
	}
	var openID, guardedID ID
	if err := openID.UnmarshalText([]byte(open.ID)); err != nil {
		t.Fatal(err)
	}
	if err := guardedID.UnmarshalText([]byte(guarded.ID)); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWithKey("plainlayer", []byte("host = \"plain.example\"\n"), openID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWithOptions("guarded", []byte("host = \"guarded.example\"\n"), SaveOptions{KeyID: guardedID, Passphrase: testPassphrase}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Read("guarded"); err != nil {
		t.Fatalf("the passphrase host is unreadable right after creation: %v", err)
	}
	// The no-passphrase key's host is readable while its boolean is set; the catalog
	// records it, so a restart restores it.
	if keys, err := repo.Keys(); err != nil || len(keys) < 2 {
		t.Fatalf("keychain = %+v, %v", keys, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenRepository(dir, NewEnvProvider(""))
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	keys, err := reopened.Keys()
	if err != nil {
		t.Fatal(err)
	}
	plugged := map[string]bool{}
	for _, key := range keys {
		plugged[key.ID] = key.Plugged
	}
	if !plugged[open.ID] || !plugged[guarded.ID] {
		t.Fatalf("plugged state did not survive the restart: %+v", keys)
	}
	if status := reopened.Status(); status.LoadedKeys < 2 {
		t.Fatalf("%d keys came back plugged in; want at least the two this test created", status.LoadedKeys)
	}
	/* The host whose key has no passphrase comes back ACTIVE from the catalog
	   boolean; the one whose key has a passphrase comes back INACTIVE. */
	if data, err := reopened.Read("plainlayer"); err != nil || len(data) == 0 {
		t.Fatalf("a no-passphrase host after the restart: %q, %v", data, err)
	}
	if _, err := reopened.Read("guarded"); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("a passphrase host after the restart = %v; want ErrNotActivated", err)
	}
	if _, err := reopened.Activate([]string{"guarded"}, map[ID]string{guardedID: testPassphrase}); err != nil {
		t.Fatalf("reactivating after the restart: %v", err)
	}
	if _, err := reopened.Read("guarded"); err != nil {
		t.Fatalf("reading after reactivating: %v", err)
	}
}

/* The `.adk` is exactly 158 bytes, at the documented offsets, and its KDF bounds are
 * refused BEFORE any derivation. (§4.5)
 */
func TestActivationFileIsFixedSizeAndBoundedBeforeDeriving(t *testing.T) {
	identity := Identity{
		StoreID:      filledID(0x01),
		ObjectID:     filledID(0x02),
		GenerationID: filledID(0x03),
		Kind:         KindProfile,
	}
	var salt [16]byte
	copy(salt[:], bytes.Repeat([]byte{0x04}, 16))
	parameters := defaultActivationParameters()
	var activationKey [32]byte
	copy(activationKey[:], bytes.Repeat([]byte{0x05}, 32))
	record, err := newActivation(identity, parameters, salt, activationKey)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0x06}, 64)
	if err := record.bind(activationKey, body); err != nil {
		t.Fatal(err)
	}
	raw := record.Bytes()
	if len(raw) != activationFileSize {
		t.Fatalf("activation record is %d bytes; want %d", len(raw), activationFileSize)
	}
	if string(raw[:8]) != "TCPACT01" {
		t.Fatalf("magic = %q", raw[:8])
	}
	if got := raw[12]; got != byte(KindProfile) {
		t.Fatalf("kind = %d", got)
	}
	if !bytes.Equal(raw[activationSaltOffset:activationSaltOffset+16], salt[:]) {
		t.Fatal("salt is not at offset 70")
	}
	parsed, err := parseActivation(raw, identity)
	if err != nil {
		t.Fatalf("parsing its own bytes: %v", err)
	}
	if err := parsed.verify(activationKey, body); err != nil {
		t.Fatalf("verifying its own record: %v", err)
	}
	if _, err := parseActivation(raw[:activationFileSize-1], identity); !errors.Is(err, ErrFormat) {
		t.Fatalf("a 157-byte record parsed: %v", err)
	}
	if _, err := parseActivation(append(bytes.Clone(raw), 0), identity); !errors.Is(err, ErrFormat) {
		t.Fatalf("a 159-byte record parsed: %v", err)
	}
	for _, parameters := range []ActivationParameters{
		{Time: 0, Memory: 64 * 1024, Threads: 1},
		{Time: 11, Memory: 64 * 1024, Threads: 1},
		{Time: 1, Memory: 4 * 1024, Threads: 1},
		{Time: 1, Memory: 512 * 1024, Threads: 1},
		{Time: 1, Memory: 64 * 1024, Threads: 0},
		{Time: 1, Memory: 64 * 1024, Threads: 9},
	} {
		costed, err := newActivation(identity, parameters, salt, activationKey)
		if err == nil {
			// A cost outside the bounds must be refused at parse time even if the
			// writer produced it.
			bad := costed.Bytes()
			if _, err := parseActivation(bad, identity); !errors.Is(err, ErrFormat) {
				t.Fatalf("parsing cost %+v = %v; want ErrFormat", parameters, err)
			}
		}
		if _, err := DeriveActivationKey("synthetic", salt, parameters); err == nil {
			t.Fatalf("DeriveActivationKey accepted cost %+v", parameters)
		}
	}
}
