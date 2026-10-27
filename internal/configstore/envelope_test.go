package configstore

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func testIdentity(kind ObjectKind) Identity {
	return Identity{
		StoreID:      filledID(0x11),
		ObjectID:     filledID(0x22),
		GenerationID: filledID(0x33),
		Kind:         kind,
	}
}

func filledID(value byte) (id ID) {
	for i := range id {
		id[i] = value
	}
	return id
}

func testMainKey(value byte) (key MainKey) {
	for i := range key {
		key[i] = value
	}
	return key
}

func TestEnvelopeRoundTripAndFreshDEKPerSave(t *testing.T) {
	identity := testIdentity(KindProfile)
	main := testMainKey(0x44)
	plaintext := []byte("host = \"example\"\nuser = \"operator\"\n")

	first, err := Seal(plaintext, identity, filledID(0x55), main)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Seal(plaintext, identity, filledID(0x55), main)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Bytes()[envelopeHeaderSize:], second.Bytes()[envelopeHeaderSize:]) {
		t.Fatal("saving identical plaintext reused the body ciphertext")
	}
	for _, sealed := range []*Envelope{first, second} {
		parsed, err := Parse(sealed.Bytes(), identity)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := parsed.Open(main)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(opened, plaintext) {
			t.Fatalf("opened = %q, want %q", opened, plaintext)
		}
		clear(opened)
	}
}

func TestEnvelopeIdentityAndAuthenticationFailClosed(t *testing.T) {
	identity := testIdentity(KindProfile)
	main := testMainKey(0x44)
	sealed, err := Seal([]byte("host = \"example\"\n"), identity, filledID(0x55), main)
	if err != nil {
		t.Fatal(err)
	}

	wrongStore := identity
	wrongStore.StoreID = filledID(0x99)
	if _, err := Parse(sealed.Bytes(), wrongStore); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong Store ID error = %v", err)
	}
	wrongKind := identity
	wrongKind.Kind = KindCatalog
	if _, err := Parse(sealed.Bytes(), wrongKind); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong kind error = %v", err)
	}

	parsed, err := Parse(sealed.Bytes(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.Open(testMainKey(0x54)); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong Main Key error = %v", err)
	}
}

func TestEnvelopeTamperingRequiresAuthentication(t *testing.T) {
	identity := testIdentity(KindProfile)
	main := testMainKey(0x44)
	sealed, err := Seal([]byte("host = \"example\"\n"), identity, filledID(0x55), main)
	if err != nil {
		t.Fatal(err)
	}
	original := sealed.Bytes()
	cases := []struct {
		name   string
		offset int
	}{
		{"data nonce", 61},
		{"Main Key ID", 89},
		{"wrap nonce", 105},
		{"EDK", 129},
		{"body", envelopeHeaderSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := bytes.Clone(original)
			changed[tc.offset] ^= 0x80
			parsed, err := Parse(changed, identity)
			if err != nil {
				t.Fatalf("public header parse failed before authentication: %v", err)
			}
			if _, err := parsed.Open(main); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("Open error = %v, want authentication failure", err)
			}
		})
	}
}

func TestEnvelopeRejectsMalformedFramingBeforeAllocation(t *testing.T) {
	identity := testIdentity(KindProfile)
	main := testMainKey(0x44)
	sealed, err := Seal([]byte("host = \"example\"\n"), identity, filledID(0x55), main)
	if err != nil {
		t.Fatal(err)
	}
	raw := sealed.Bytes()

	if _, err := Parse(raw[:len(raw)-1], identity); !errors.Is(err, ErrFormat) {
		t.Fatalf("truncated error = %v", err)
	}
	if _, err := Parse(append(bytes.Clone(raw), 0), identity); !errors.Is(err, ErrFormat) {
		t.Fatalf("trailing byte error = %v", err)
	}
	badVersion := bytes.Clone(raw)
	binary.LittleEndian.PutUint16(badVersion[8:10], 2)
	if _, err := Parse(badVersion, identity); !errors.Is(err, ErrVersion) {
		t.Fatalf("version error = %v", err)
	}
	badAlgorithm := bytes.Clone(raw)
	binary.LittleEndian.PutUint16(badAlgorithm[10:12], 99)
	if _, err := Parse(badAlgorithm, identity); !errors.Is(err, ErrFormat) {
		t.Fatalf("algorithm error = %v", err)
	}
	badLength := bytes.Clone(raw)
	binary.LittleEndian.PutUint32(badLength[85:89], MaxProfilePlaintext+17)
	if _, err := Parse(badLength, identity); !errors.Is(err, ErrFormat) {
		t.Fatalf("length error = %v", err)
	}

	oversized := make([]byte, envelopeHeaderSize+MaxProfilePlaintext+chacha20poly1305.Overhead+1)
	if _, err := Parse(oversized, identity); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized error = %v", err)
	}
	if _, err := Seal(make([]byte, MaxProfilePlaintext+1), identity, filledID(0x55), main); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized seal error = %v", err)
	}
}

func TestEnvelopeAcceptsTheDocumentedPlaintextLimit(t *testing.T) {
	identity := testIdentity(KindProfile)
	main := testMainKey(0x44)
	plaintext := bytes.Repeat([]byte{'x'}, MaxProfilePlaintext)
	sealed, err := Seal(plaintext, identity, filledID(0x55), main)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(sealed.Bytes(), identity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parsed.Open(main)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("maximum-size profile did not round-trip")
	}
}

func TestEnvelopeRewrapChangesOnlyWrapper(t *testing.T) {
	identity := testIdentity(KindProfile)
	oldMain := testMainKey(0x44)
	newMain := testMainKey(0x66)
	newKeyID := filledID(0x77)
	sealed, err := Seal([]byte("host = \"example\"\n"), identity, filledID(0x55), oldMain)
	if err != nil {
		t.Fatal(err)
	}
	before := sealed.Bytes()
	rewrapped, err := sealed.Rewrap(oldMain, newKeyID, newMain)
	if err != nil {
		t.Fatal(err)
	}
	after := rewrapped.Bytes()
	if !bytes.Equal(before[:dataHeaderSize], after[:dataHeaderSize]) {
		t.Fatal("rewrap changed the stable data header")
	}
	if !bytes.Equal(before[envelopeHeaderSize:], after[envelopeHeaderSize:]) {
		t.Fatal("rewrap changed the body ciphertext")
	}
	if got := rewrapped.WrapKeyID(); got != newKeyID {
		t.Fatalf("WrapKeyID = %x, want %x", got, newKeyID)
	}
	if _, err := rewrapped.Open(newMain); err != nil {
		t.Fatal(err)
	}
	if _, err := rewrapped.Open(oldMain); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("old key error = %v", err)
	}
}

func TestEnvelopeV1Golden(t *testing.T) {
	identity := testIdentity(KindProfile)
	main := testMainKey(0x33)
	random := bytes.NewReader(deterministicBytes(80))
	sealed, err := sealWithRandom(
		[]byte("host = \"example\"\nuser = \"operator\"\n"),
		identity,
		filledID(0x44),
		main,
		random,
	)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/envelope-v1.golden")
	if err != nil {
		t.Fatal(err)
	}
	got := []byte(strings.TrimSpace(hex.EncodeToString(sealed.Bytes())))
	want = bytes.TrimSpace(want)
	if !bytes.Equal(got, want) {
		t.Fatalf("golden envelope changed\n got: %s\nwant: %s", got, want)
	}
}

func deterministicBytes(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}
