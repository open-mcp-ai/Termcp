package configstore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func environmentKEK(value byte) string {
	key := bytes.Repeat([]byte{value}, 32)
	return base64.StdEncoding.EncodeToString(key)
}

func testArgon2Cost() Argon2Parameters {
	return Argon2Parameters{Time: 1, Memory: 8 * 1024, Threads: 1}
}

func TestKeyringENVRoundTripAndProfileEnvelope(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	data, keyring, err := CreateKeyring(storeID, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()
	if bytes.Contains(data, []byte(environmentKEK(0x61))) {
		t.Fatal("keyring file contains its Provider KEK")
	}

	unlocked, err := UnlockKeyring(data, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer unlocked.Destroy()
	identity := Identity{
		StoreID:      storeID,
		ObjectID:     filledID(0x20),
		GenerationID: filledID(0x30),
		Kind:         KindProfile,
	}
	sealed, err := unlocked.Seal([]byte("host = \"example\"\n"), identity)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(sealed.Bytes(), identity)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := unlocked.Open(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != "host = \"example\"\n" {
		t.Fatalf("opened = %q", opened)
	}
	clear(opened)

	keyring.Destroy()
	if _, err := keyring.Open(parsed); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("open after Destroy error = %v", err)
	}
}

func TestKeyringWrongEnvironmentKEKDoesNotRevealAuthenticationOracle(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	data, keyring, err := CreateKeyring(filledID(0x10), NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()
	t.Setenv(EnvKEKVariable, environmentKEK(0x62))
	if _, err := UnlockKeyring(data, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong KEK error = %v", err)
	}
	t.Setenv(EnvKEKVariable, "")
	if _, err := UnlockKeyring(data, NewEnvProvider("")); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("missing KEK error = %v", err)
	}
}

func TestPasswordAndENVSlotsUnwrapSameKeyring(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	passwordProvider, err := NewPasswordProvider([]byte("synthetic recovery password"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer passwordProvider.Destroy()
	data, keyring, err := CreateKeyring(
		filledID(0x10),
		NewEnvProvider(""),
		passwordProvider,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	fromENV, err := UnlockKeyring(data, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer fromENV.Destroy()
	fromPassword, err := UnlockKeyring(data, passwordProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer fromPassword.Destroy()

	identity := Identity{
		StoreID:      filledID(0x10),
		ObjectID:     filledID(0x20),
		GenerationID: filledID(0x30),
		Kind:         KindCatalog,
	}
	sealed, err := fromENV.Seal([]byte(`{"profiles":[]}`), identity)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(sealed.Bytes(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fromPassword.Open(parsed); err != nil {
		t.Fatalf("password slot unwrapped a different Main Key: %v", err)
	}
}

func TestPasswordProviderRejectsWrongPasswordAndBoundsKDF(t *testing.T) {
	provider, err := NewPasswordProvider([]byte("correct synthetic password"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	data, keyring, err := CreateKeyring(filledID(0x10), provider)
	provider.Destroy()
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	wrong, err := NewPasswordProvider([]byte("wrong synthetic password"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Destroy()
	if _, err := UnlockKeyring(data, wrong); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong password error = %v", err)
	}

	var stored storedKeyring
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Slots[0].Parameters.Argon2.Memory = 300 * 1024
	bad, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnlockKeyring(bad, wrong); !errors.Is(err, ErrFormat) {
		t.Fatalf("out-of-bounds KDF error = %v", err)
	}
}

func TestKeyringFileRejectsUnknownFieldsDuplicateSlotsAndTampering(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	data, keyring, err := CreateKeyring(filledID(0x10), NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	withUnknown := bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"unexpected":true`), 1)
	if _, err := UnlockKeyring(withUnknown, NewEnvProvider("")); !errors.Is(err, ErrFormat) {
		t.Fatalf("unknown field error = %v", err)
	}

	var stored storedKeyring
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	duplicate := stored
	duplicate.Slots = append(duplicate.Slots, stored.Slots[0])
	duplicateData, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnlockKeyring(duplicateData, NewEnvProvider("")); !errors.Is(err, ErrFormat) {
		t.Fatalf("duplicate slot error = %v", err)
	}

	for _, offset := range []int{0, 1} {
		changed := stored
		changed.Slots = append([]storedKeySlot(nil), stored.Slots...)
		switch offset {
		case 0:
			changed.Slots[0].ID[0] ^= 0x80
		case 1:
			changed.Slots[0].Wrapped[0] ^= 0x80
		}
		changedData, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UnlockKeyring(changedData, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) {
			t.Fatalf("tamper case %d error = %v", offset, err)
		}
	}
}

func TestKeyringMarshalPreservesMainKeys(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	keyID := filledID(0x20)
	main := testMainKey(0x30)
	keyring := &Keyring{
		storeID:     storeID,
		activeKeyID: keyID,
		mainKeys:    map[ID]MainKey{keyID: main},
	}
	data, err := keyring.Marshal(NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	unlocked, err := UnlockKeyring(data, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer unlocked.Destroy()

	identity := Identity{
		StoreID:      storeID,
		ObjectID:     filledID(0x40),
		GenerationID: filledID(0x50),
		Kind:         KindMigration,
	}
	sealed, err := Seal([]byte("migration"), identity, keyID, main)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(sealed.Bytes(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unlocked.Open(parsed); err != nil {
		t.Fatalf("Marshal changed the Main Key: %v", err)
	}
}
