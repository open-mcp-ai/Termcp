package hostshare

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func secret(b byte) [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = b + byte(i)
	}
	return s
}

func TestSealOpensOnlyForTheRecipient(t *testing.T) {
	alice, bob := secret(1), secret(2)
	toml := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"ops\"\npassword = \"synthetic\"\n")

	blob, err := Seal(mustPub(bob), "prod", toml, PublicKey(alice))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !LooksLike(blob) {
		t.Fatalf("blob does not identify itself: %q", blob[:20])
	}
	name, got, err := Open(bob, blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if name != "prod" {
		t.Fatalf("name = %q", name)
	}
	if !bytes.Equal(got, toml) {
		t.Fatalf("toml = %q", got)
	}
	if _, _, err := Open(alice, blob); err != ErrDecrypt {
		t.Fatalf("another person opened it: %v", err)
	}
	if Sender(blob) != PublicKey(alice) {
		t.Fatal("sender label lost")
	}
}

/* A blob is relabelled by re-serialising the JSON with a different name. The
 * name is inside the AEAD's additional data, so that must not authenticate.
 */
func TestNameIsAuthenticated(t *testing.T) {
	bob := secret(9)
	blob, err := Seal(mustPub(bob), "prod", []byte("host = \"h\"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(blob, "eyJuIjoicHJvZCIs", "eyJuIjoiZXZpbCIs", 1)
	if tampered != blob {
		if _, _, err := Open(bob, tampered); err == nil {
			t.Fatal("a renamed blob opened")
		}
		return
	}
	// Fall back to the structural guarantee if the encoding differs: decode,
	// change the name, re-encode.
	card, err := parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	card.Name = "evil"
	raw := rebuild(t, card)
	if _, _, err := Open(bob, raw); err != ErrDecrypt {
		t.Fatalf("renamed blob = %v; want ErrDecrypt", err)
	}
}

func TestRejectsForeignText(t *testing.T) {
	for _, input := range []string{"", "hello", "termcp-host-v1:!!!!", "termcp-host-v1:" + strings.Repeat("A", 10)} {
		if _, _, err := Open(secret(1), input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestChatWrappingSurvives(t *testing.T) {
	bob := secret(4)
	blob, err := Seal(mustPub(bob), "prod", []byte("host = \"h\"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	// Chat inserts hard wraps and often a trailing newline.
	var wrapped strings.Builder
	for i := 0; i < len(blob); i += 60 {
		end := i + 60
		if end > len(blob) {
			end = len(blob)
		}
		wrapped.WriteString(blob[i:end])
		wrapped.WriteString("\r\n")
	}
	if _, _, err := Open(bob, wrapped.String()); err != nil {
		t.Fatalf("wrapped blob: %v", err)
	}
}

func TestPublicKeyRoundTrip(t *testing.T) {
	s := secret(7)
	pub, err := ParsePublicKey(PublicKey(s))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := Seal(pub, "prod", []byte("host = \"h\"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(s, blob); err != nil {
		t.Fatalf("re-parsed public key: %v", err)
	}
	if _, err := ParsePublicKey("not-base64!"); err == nil {
		t.Fatal("garbage public key accepted")
	}
}

func rebuild(t *testing.T, c card) string {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw)
}

func mustPub(s [32]byte) [32]byte {
	pub, err := ParsePublicKey(PublicKey(s))
	if err != nil {
		panic(err)
	}
	return pub
}
