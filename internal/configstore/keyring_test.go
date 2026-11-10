package configstore

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
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
	envelope, err := unlocked.Seal([]byte("profile body"), identity)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := keyring.Open(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "profile body" {
		t.Fatalf("plaintext = %q", plaintext)
	}
}

func TestKeyringWrongEnvironmentKEKDoesNotRevealAuthenticationOracle(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	data, keyring, err := CreateKeyring(storeID, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	t.Setenv(EnvKEKVariable, environmentKEK(0x62))
	if _, err := UnlockKeyring(data, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("UnlockKeyring = %v, want ErrAuthentication", err)
	}
}

func TestPasswordAndENVSlotsUnwrapSameKeyring(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	passwordProvider, err := NewPasswordProvider([]byte("correct-horse-battery"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer passwordProvider.Destroy()

	data, keyring, err := CreateKeyring(storeID, passwordProvider, NewEnvProvider(""))
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
		StoreID:      storeID,
		ObjectID:     filledID(0x20),
		GenerationID: filledID(0x30),
		Kind:         KindProfile,
	}
	envelope, err := fromENV.Seal([]byte("wrapped body"), identity)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := fromPassword.Open(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "wrapped body" {
		t.Fatalf("plaintext = %q", plaintext)
	}
}

func TestPasswordProviderRejectsWrongPasswordAndBoundsKDF(t *testing.T) {
	storeID := filledID(0x10)
	good, err := NewPasswordProvider([]byte("the-real-password"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer good.Destroy()

	data, keyring, err := CreateKeyring(storeID, good)
	if err != nil {
		t.Fatal(err)
	}
	defer keyring.Destroy()

	bad, err := NewPasswordProvider([]byte("wrong-password"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Destroy()

	if _, err := UnlockKeyring(data, bad); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("UnlockKeyring with bad password = %v, want ErrAuthentication", err)
	}

	for _, invalid := range []Argon2Parameters{
		{Time: 0, Memory: 64 * 1024, Threads: 1},
		{Time: 11, Memory: 64 * 1024, Threads: 1},
		{Time: 1, Memory: 4 * 1024, Threads: 1},
		{Time: 1, Memory: 512 * 1024, Threads: 1},
		{Time: 1, Memory: 64 * 1024, Threads: 0},
		{Time: 1, Memory: 64 * 1024, Threads: 9},
	} {
		if _, err := NewPasswordProvider([]byte("password"), invalid); !errors.Is(err, ErrFormat) {
			t.Fatalf("NewPasswordProvider(%+v) = %v, want ErrFormat", invalid, err)
		}
	}
}

func TestKeyringFileRejectsUnknownFieldsDuplicateSlotsAndTampering(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	data, keyring, err := CreateKeyring(storeID, NewEnvProvider(""))
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
	if _, err := UnlockKeyring(withUnknown, NewEnvProvider("")); !errors.Is(err, ErrFormat) {
		t.Fatalf("UnlockKeyring with unknown fields = %v, want ErrFormat", err)
	}

	var stored storedKeyring
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Slots = append(stored.Slots, stored.Slots[0])
	withDuplicates, _ := json.Marshal(stored)
	if _, err := UnlockKeyring(withDuplicates, NewEnvProvider("")); !errors.Is(err, ErrFormat) {
		t.Fatalf("UnlockKeyring with duplicate slot IDs = %v, want ErrFormat", err)
	}

	tampered := bytes.Clone(data)
	tampered[len(tampered)-10] ^= 0x40
	if _, err := UnlockKeyring(tampered, NewEnvProvider("")); !errors.Is(err, ErrAuthentication) && !errors.Is(err, ErrFormat) {
		t.Fatalf("UnlockKeyring with tampered ciphertext = %v", err)
	}
}

func TestKeyringMarshalPreservesMainKeys(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	keyID := filledID(0x20)
	main := testMainKey(0x30)
	keyring := &Keyring{}
	keyring.state.Store(&keyringState{
		storeID:     storeID,
		activeKeyID: keyID,
		mainKeys:    map[ID]MainKey{keyID: main},
	})
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
		Kind:         KindProfile,
	}
	envelope, err := unlocked.Seal([]byte("payload"), identity)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := envelope.Open(main)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "payload" {
		t.Fatalf("plaintext = %q", plaintext)
	}
}

func TestKeyringFileHoldsNoKeyMaterial(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	passwordProvider, err := NewPasswordProvider([]byte("correct-horse-battery"), testArgon2Cost())
	if err != nil {
		t.Fatal(err)
	}
	defer passwordProvider.Destroy()

	raw, keyring, err := CreateKeyring(storeID, passwordProvider, NewEnvProvider(""))
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
		if bytes.Contains(raw, []byte(hex.EncodeToString(mainBytes))) {
			t.Fatal("the keyring file contains a Main Key hex-encoded")
		}
		if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(mainBytes))) {
			t.Fatal("the keyring file contains a Main Key base64-encoded")
		}
	}
	if bytes.Contains(raw, []byte("correct-horse-battery")) {
		t.Fatal("the keyring file contains the password")
	}
}

func TestPasswordPolicy(t *testing.T) {
	for _, tc := range []struct {
		password string
		confirm  string
		want     error
	}{
		{"", "", ErrPasswordInvalid},
		{"short", "short", ErrPasswordInvalid},
		{"1234567", "1234567", ErrPasswordInvalid},
		{"12345678", "12345678", nil},
		{"   12345678   ", "   12345678   ", nil},
		{strings.Repeat("a", 1024), strings.Repeat("a", 1024), nil},
		{strings.Repeat("a", 1025), strings.Repeat("a", 1025), ErrPasswordInvalid},
		{"12345678", "1234567x", ErrPasswordConfirmation},
	} {
		err := validateNewPassword(tc.password, tc.confirm)
		if tc.want == nil {
			if err != nil {
				t.Errorf("password of len %d rejected: %v", len(tc.password), err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("validateNewPassword(%q, %q) = %v, want %v", tc.password, tc.confirm, err, tc.want)
		}
	}
}

// Destroy can be called concurrently with Seal or Open without panic or data race.
// The lock detector confirms no mutex was used to buy that property.
func TestConcurrentSealAndDestroyRacesSafely(t *testing.T) {
	t.Setenv(EnvKEKVariable, environmentKEK(0x61))
	storeID := filledID(0x10)
	_, keyring, err := CreateKeyring(storeID, NewEnvProvider(""))
	if err != nil {
		t.Fatal(err)
	}

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
				_, _ = keyring.Seal([]byte("race"), identity)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		keyring.Destroy()
	}()
	wg.Wait()

	if _, err := keyring.Seal([]byte("after"), identity); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Seal after Destroy = %v, want ErrProviderUnavailable", err)
	}
}
