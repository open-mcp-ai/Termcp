package configstore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func environmentKEK(value byte) string {
	key := bytes.Repeat([]byte{value}, 32)
	return base64.StdEncoding.EncodeToString(key)
}

// testStoreID is the salt every derivation in these tests uses. It is synthetic:
// nothing here depends on a real machine.
func testStoreID() ID { return filledID(0x10) }

func testPhrase(t *testing.T) string {
	t.Helper()
	phrase, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	return phrase
}

func TestKeyringSealsTheMnemonicNotTheMainKey(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := testStoreID()
	phrase := testPhrase(t)
	data, keyring, err := CreateKeyring(storeID, phrase, "", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()
	if bytes.Contains(data, []byte(environmentKEK(0x61))) {
		t.Fatal("keyring file contains its Provider KEK")
	}
	/* The words must not appear either: a slot is a sealed copy, and a file that
	   held them in clear would make the wrapper meaningless. */
	normalized, err := NormalizeMnemonic(phrase)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(normalized)) {
		t.Fatal("keyring file contains the recovery phrase in clear")
	}
	/* Nor the Main Key it derives. */
	main, err := deriveMainKey(storeID, normalized)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, main[:]) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(main[:]))) {
		t.Fatal("keyring file contains the Main Key")
	}

	loaded, err := LoadKeyring(data, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Destroy()
	identity := Identity{
		StoreID:      storeID,
		ObjectID:     filledID(0x20),
		GenerationID: filledID(0x30),
		Kind:         KindProfile,
	}
	envelope, err := loaded.SealWith([]byte("profile body"), identity, onlyKeyID(t, loaded), noActivationKey)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := keyring.Open(envelope, noActivationKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "profile body" {
		t.Fatalf("plaintext = %q", plaintext)
	}
}

/* The Main Key is DERIVED, so the same words in the same store are the same key —
 * that is what makes the exported phrase and this machine the same ability — and
 * the same words in another store are a different one.
 */
func TestMainKeyIsDerivedFromTheMnemonicAndTheStore(t *testing.T) {
	phrase := testPhrase(t)
	normalized, err := NormalizeMnemonic(phrase)
	if err != nil {
		t.Fatal(err)
	}
	first, err := deriveMainKey(testStoreID(), normalized)
	if err != nil {
		t.Fatal(err)
	}
	again, err := deriveMainKey(testStoreID(), strings.ToUpper(strings.ReplaceAll(phrase, " ", "  ")))
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("casing and whitespace changed the derived Main Key")
	}
	other, err := deriveMainKey(filledID(0x11), normalized)
	if err != nil {
		t.Fatal(err)
	}
	if first == other {
		t.Fatal("the same phrase in another store derived the same Main Key")
	}
	if _, err := deriveMainKey(testStoreID(), "not a phrase at all"); !errors.Is(err, ErrMnemonicInvalid) {
		t.Fatalf("deriving from an invalid phrase = %v; want ErrMnemonicInvalid", err)
	}
}

func TestKeyringWrongEnvironmentKEKDoesNotRevealAuthenticationOracle(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	data, keyring, err := CreateKeyring(testStoreID(), testPhrase(t), "", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	t.Setenv(EnvKEKVariable, environmentKEK(0x62))
	if _, err := LoadKeyring(data, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("LoadKeyring = %v, want ErrAuthentication", err)
	}
}

/* The plugged bit is what survives a restart. A key marked unplugged stays out of
 * memory with every provider able to open it, and a key marked plugged comes back
 * with none of them asked.
 */
func TestKeychainFileRecordedPluggedState(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x63))
	data, keyring, err := CreateKeyring(testStoreID(), testPhrase(t), "", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	loaded, err := LoadKeyring(data, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.keysCount() != 1 {
		t.Fatalf("a freshly created keychain has %d pluggable keys; want 1", loaded.keysCount())
	}
	loaded.Destroy()

	var stored storedKeyring
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Keys[0].Plugged {
		t.Fatal("a new key is not recorded as plugged")
	}
	stored.Keys[0].Plugged = false
	unplugged, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadKeyring(unplugged, NewEnvProvider(""))
	if err != nil && !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("an unplugged keychain failed to load: %v", err)
	}
	defer reloaded.Destroy()
	if reloaded.keysCount() != 0 {
		t.Fatal("an unplugged key's material was put in memory anyway")
	}
	/* The words still open it: unplugging is a decision, not a loss. */
	if _, _, err := unsealKey(mustParse(t, unplugged), storedKeyIDOf(t, unplugged), NewEnvProvider("")); err != nil {
		t.Fatalf("the words no longer open an unplugged key: %v", err)
	}
}

func mustParse(t *testing.T, data []byte) storedKeyring {
	t.Helper()
	stored, err := parseStoredKeyring(data)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// storedKeyIDOf returns the id of the keychain's single key. With no index owner
// there is no field naming a key, so tests read the key itself.
func storedKeyIDOf(t *testing.T, data []byte) ID {
	t.Helper()
	stored := mustParse(t, data)
	if len(stored.Keys) == 0 {
		t.Fatal("keychain holds no key")
	}
	var id ID
	copy(id[:], stored.Keys[0].ID)
	return id
}

// onlyKeyID returns the id of the one key this keyring holds in memory.
func onlyKeyID(t *testing.T, keyring *Keyring) ID {
	t.Helper()
	st := keyring.state.Load()
	if st == nil || len(st.mainKeys) != 1 {
		t.Fatalf("expected exactly one plugged key, got %+v", st)
	}
	for id := range st.mainKeys {
		return id
	}
	return ID{}
}

func TestKeyringFileRejectsUnknownFieldsDuplicateSlotsAndTampering(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x64))
	data, keyring, err := CreateKeyring(testStoreID(), testPhrase(t), "", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["extra"] = "forbidden"
	withUnknown, _ := json.Marshal(raw)
	if _, err := LoadKeyring(withUnknown, NewEnvProvider("")); !errors.Is(err, ErrFormat) {
		t.Fatalf("LoadKeyring with unknown fields = %v, want ErrFormat", err)
	}

	var stored storedKeyring
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Keys[0].Slots = append(stored.Keys[0].Slots, stored.Keys[0].Slots[0])
	withDuplicates, _ := json.Marshal(stored)
	if _, err := LoadKeyring(withDuplicates, NewEnvProvider("")); !errors.Is(err, ErrFormat) {
		t.Fatalf("LoadKeyring with duplicate slot IDs = %v, want ErrFormat", err)
	}

	tampered := bytes.Clone(data)
	tampered[len(tampered)-10] ^= 0x40
	if _, err := LoadKeyring(tampered, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) && !errors.Is(err, ErrFormat) {
		t.Fatalf("LoadKeyring with tampered ciphertext = %v", err)
	}
}

/* A slot seals its key under the AAD of that key, key id included. Moving a slot
 * — same slot id, same provider — under another key's header must not
 * authenticate: otherwise the wrapper made for one key would open the material of
 * the key that seals real hosts.
 *
 * Mutation check: drop the key id from keyringSlotAAD and the moved slot opens the
 * second key.
 */
func TestKeyringSlotCannotBeMovedToAnotherKey(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x65))
	data, keyring, err := CreateKeyring(testStoreID(), testPhrase(t), "", NewEnvProvider(""), NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()
	var stored storedKeyring
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Keys[0].Slots) != 2 {
		t.Fatalf("first key has %d slots; want two", len(stored.Keys[0].Slots))
	}
	second, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	stored.Keys = append(stored.Keys, storedKey{
		ID:    append([]byte(nil), second[:]...),
		Slots: []storedKeySlot{stored.Keys[0].Slots[1]},
	})
	stored.Keys[0].Slots = stored.Keys[0].Slots[:1]
	spliced, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadKeyring(spliced, NewEnvProvider(""))
	if loaded == nil {
		t.Fatalf("the spliced keychain did not parse: %v", err)
	}
	defer loaded.Destroy()
	if material, ok := loaded.key(second); ok {
		clear(material[:])
		t.Fatal("a slot sealed for one key loaded another key's material")
	}
	if _, ok := loaded.key(onlyKeyID(t, keyring)); !ok {
		t.Fatal("the untouched slot stopped working")
	}
}

func TestKeyringFileHoldsNoKeyMaterial(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x66))
	phrase := testPhrase(t)
	raw, keyring, err := CreateKeyring(testStoreID(), phrase, "", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	st := keyring.state.Load()
	if st == nil {
		t.Fatal("keyring state is nil")
	}
	for _, main := range st.mainKeys {
		mainBytes := main[:]
		if bytes.Contains(raw, mainBytes) {
			t.Fatal("the keyring file contains a Main Key raw")
		}
		if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(mainBytes))) {
			t.Fatal("the keyring file contains a Main Key base64-encoded")
		}
	}
}

// Destroy can be called concurrently with SealWith or Open without panic or data
// race. The lock detector confirms no mutex was used to buy that property.
func TestConcurrentSealAndDestroyRacesSafely(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x67))
	storeID := testStoreID()
	_, keyring, err := CreateKeyring(storeID, testPhrase(t), "", NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	keyID := onlyKeyID(t, keyring)
	identity := Identity{
		StoreID:      storeID,
		ObjectID:     filledID(0x20),
		GenerationID: filledID(0x30),
		Kind:         KindProfile,
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = keyring.SealWith([]byte("race"), identity, keyID, noActivationKey)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		keyring.Destroy()
	}()
	wg.Wait()

	if _, err := keyring.SealWith([]byte("after"), identity, keyID, noActivationKey); !errors.Is(err, ErrClosed) {
		t.Fatalf("SealWith after Destroy = %v, want ErrClosed", err)
	}
}
