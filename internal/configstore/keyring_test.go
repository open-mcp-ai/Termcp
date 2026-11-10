package configstore

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testStoreID(t *testing.T) ID {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	return id
}

func profileIdentity(store ID, name string) Identity {
	object := sha(name)
	return Identity{StoreID: store, ObjectID: object, GenerationID: object, Kind: KindProfile}
}

// sha maps a name to a stable id for tests, so one envelope can be sealed and later
// expected for the same profile name.
func sha(name string) ID {
	var id ID
	copy(id[:], name)
	return id
}

// THE PASSWORD IS THE KEY. A round trip is the whole feature: seal a profile, put the
// keyring on disk, and a later process with the right password reads it back.
func TestPasswordKeyringRoundTrip(t *testing.T) {
	store := testStoreID(t)
	k, raw, err := CreatePasswordKeyring(store, "correct horse battery", "correct horse battery")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer k.Destroy()

	identity := profileIdentity(store, "prod")
	env, err := k.Seal([]byte("password = \"hunter2\"\n"), identity)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	blob := env.Bytes()

	unlocked, err := UnlockPasswordKeyring(raw, "correct horse battery")
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	defer unlocked.Destroy()
	if unlocked.StoreID() != store {
		t.Fatalf("store id = %s, want %s", unlocked.StoreID(), store)
	}
	if unlocked.KeyID() != k.KeyID() {
		t.Fatalf("key id changed across a round trip: %s vs %s", unlocked.KeyID(), k.KeyID())
	}

	parsed, err := Parse(blob, identity)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plain, err := unlocked.Open(parsed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if want := "password = \"hunter2\"\n"; string(plain) != want {
		t.Fatalf("plaintext = %q, want %q", plain, want)
	}
}

// A WRONG PASSWORD MUST NOT OPEN ANYTHING, and the answer must not say which of
// "wrong password" and "tampered file" it was.
func TestWrongPasswordIsRejected(t *testing.T) {
	store := testStoreID(t)
	k, raw, err := CreatePasswordKeyring(store, "correct horse battery", "correct horse battery")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	k.Destroy()

	if _, err := UnlockPasswordKeyring(raw, "correct horse batterY"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("unlock with a wrong password = %v, want ErrAuthentication", err)
	}
}

// THE KDF INPUTS ARE AUTHENTICATED. Without them in the AAD, lowering the cost in the
// file would make the victim derive a weaker key from the same password, and raising
// it would turn unlocking into a denial of service.
func TestTamperedKeyringIsRejected(t *testing.T) {
	store := testStoreID(t)
	const password = "correct horse battery"
	_, raw, err := CreatePasswordKeyring(store, password, password)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	tamper := []struct {
		name  string
		apply func(*storedKeyring)
	}{
		{"lower the Argon2 cost", func(st *storedKeyring) { st.Argon2.Time = 1 }},
		{"lower the memory", func(st *storedKeyring) { st.Argon2.Memory = 8 * 1024 }},
		{"replace the salt", func(st *storedKeyring) { st.Salt[0] ^= 0xff }},
		{"replace the nonce", func(st *storedKeyring) { st.Nonce[0] ^= 0xff }},
		{"replace the verifier", func(st *storedKeyring) { st.Verifier[0] ^= 0xff }},
		{"replace the key id", func(st *storedKeyring) { st.KeyID[0] ^= 0xff }},
		{"replace the store id", func(st *storedKeyring) { st.StoreID = strings.Repeat("ab", 16) }},
	}
	for _, tc := range tamper {
		t.Run(tc.name, func(t *testing.T) {
			var st storedKeyring
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			tc.apply(&st)
			mutated, err := json.Marshal(st)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if _, err := UnlockPasswordKeyring(mutated, password); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("a tampered keyring was accepted: err = %v", err)
			}
		})
	}
}

// A MODIFIED FILE MUST NOT BUY COMPUTE. The parameters are bounded before the KDF
// runs, so an attacker who can write the file cannot make unlocking allocate 1 TiB.
func TestArgon2ParametersAreBoundedBeforeTheKDFRuns(t *testing.T) {
	store := testStoreID(t)
	const password = "correct horse battery"
	_, raw, err := CreatePasswordKeyring(store, password, password)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, p := range []Argon2Parameters{
		{Time: 0, Memory: 64 * 1024, Threads: 1},
		{Time: 1_000, Memory: 64 * 1024, Threads: 1},
		{Time: 3, Memory: 1024, Threads: 1},
		{Time: 3, Memory: 4 << 20, Threads: 1},
		{Time: 3, Memory: 64 * 1024, Threads: 0},
	} {
		var st storedKeyring
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		st.Argon2 = p
		mutated, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, err := UnlockPasswordKeyring(mutated, password); !errors.Is(err, ErrFormat) {
			t.Fatalf("parameters %+v = %v, want ErrFormat", p, err)
		}
	}
}

// CHANGING THE PASSWORD CHANGES THE MAIN KEY, so every envelope must be rewrapped —
// the body ciphertext is reused, which is the point of the DEK layer: a password
// change does not re-encrypt the configuration, and does not touch the disk bytes.
func TestRekeyRewrapsAndRetiresTheOldPassword(t *testing.T) {
	store := testStoreID(t)
	oldKey, oldRaw, err := CreatePasswordKeyring(store, "first password here", "first password here")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer oldKey.Destroy()

	identity := profileIdentity(store, "staging")
	env, err := oldKey.Seal([]byte("user = \"deploy\"\n"), identity)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	bodyBefore := env.Bytes()

	newKey, err := oldKey.Rekey("second password here", "second password here")
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	defer newKey.Destroy()
	if newKey.KeyID() == oldKey.KeyID() {
		t.Fatal("rekey kept the old key id")
	}

	rewrapped, err := env.Rewrap(oldKey.main, newKey.KeyID(), newKey.main)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	// The inherited DEK means the body ciphertext survives a password change.
	if !bytes.Equal(rewrapped.Bytes()[envelopeHeaderSize:], bodyBefore[envelopeHeaderSize:]) {
		t.Fatal("rewrap re-encrypted the body; the DEK layer exists so it does not")
	}
	plain, err := rewrapped.Open(newKey.main)
	if err != nil {
		t.Fatalf("open with the new key: %v", err)
	}
	if string(plain) != "user = \"deploy\"\n" {
		t.Fatalf("plaintext = %q", plain)
	}
	// The retired key must not open the rewrapped envelope.
	if _, err := rewrapped.Open(oldKey.main); err == nil {
		t.Fatal("the retired Main Key still opens the rewrapped envelope")
	}

	newRaw, err := newKey.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := UnlockPasswordKeyring(newRaw, "first password here"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("the old password unlocked the new keyring: %v", err)
	}
	if _, err := UnlockPasswordKeyring(oldRaw, "second password here"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("the new password unlocked the old keyring: %v", err)
	}
}

// NO KEY MATERIAL IN THE FILE. The Main Key is a function of the password and the
// salt; if it ever appeared on disk the file would be the whole secret.
func TestKeyringFileHoldsNoKeyMaterial(t *testing.T) {
	store := testStoreID(t)
	k, raw, err := CreatePasswordKeyring(store, "correct horse battery", "correct horse battery")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer k.Destroy()

	mainKey := k.main[:]
	if bytes.Contains(raw, mainKey) {
		t.Fatal("the keyring file contains the Main Key raw")
	}
	for _, enc := range [][]byte{
		[]byte(hex.EncodeToString(mainKey)),
		[]byte(base64.StdEncoding.EncodeToString(mainKey)),
	} {
		if bytes.Contains(raw, enc) {
			t.Fatalf("the keyring file contains the Main Key encoded: %q", enc)
		}
	}
	if bytes.Contains(raw, []byte("correct horse battery")) {
		t.Fatal("the keyring file contains the password")
	}
}

// The policy in the error message is the policy in the code.
func TestPasswordPolicy(t *testing.T) {
	store := testStoreID(t)
	for _, tc := range []struct {
		password string
		want     error
	}{
		{"", ErrPasswordRequired},
		{"short", ErrPasswordInvalid},
		{"1234567", ErrPasswordInvalid},
		{"12345678", nil},
		{strings.Repeat("a", maxPasswordBytes), nil},
		{strings.Repeat("a", maxPasswordBytes+1), ErrPasswordInvalid},
	} {
		_, _, err := CreatePasswordKeyring(store, tc.password, tc.password)
		if tc.want == nil {
			if err != nil {
				t.Errorf("password of len %d rejected: %v", len(tc.password), err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("password of len %d = %v, want %v", len(tc.password), err, tc.want)
		}
	}
	if _, _, err := CreatePasswordKeyring(store, "12345678", "1234567x"); !errors.Is(err, ErrPasswordConfirmation) {
		t.Fatalf("mismatched confirmation = %v, want ErrPasswordConfirmation", err)
	}
}

// An envelope sealed under another keyring's key id must be refused rather than
// decrypted: that id is how a half-finished rotation is detected.
func TestOpenRefusesAnEnvelopeFromAnotherKey(t *testing.T) {
	store := testStoreID(t)
	first, _, err := CreatePasswordKeyring(store, "first password here", "first password here")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	defer first.Destroy()
	second, _, err := CreatePasswordKeyring(store, "second password here", "second password here")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	defer second.Destroy()

	env, err := first.Seal([]byte("x = 1\n"), profileIdentity(store, "a"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := second.Open(env); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("open with a foreign keyring = %v, want ErrAuthentication", err)
	}
}
