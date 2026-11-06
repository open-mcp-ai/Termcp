package sshconfig

import (
	"errors"
	"strings"
	"testing"
)

// TestAMisspelledKeyIsRefusedByName pins issue #249.4.
//
// TOML drops keys the struct does not name, so a profile with `privat_key` parsed
// cleanly, produced a zero value, and failed much later at dial time as "unable to
// authenticate" — a message that describes the symptom and hides the edit. The
// refusal has to name the word the caller typed, because that word is their next
// action.
func TestAMisspelledKeyIsRefusedByName(t *testing.T) {
	_, err := ParseAndValidate([]byte(`kind = "remote"
host = "example.test"
user = "pi"
privat_key = "-----BEGIN OPENSSH PRIVATE KEY-----"
`))
	if err == nil {
		t.Fatal("a misspelled key was accepted, so its value was silently ignored")
	}
	if !strings.Contains(err.Error(), `"privat_key"`) {
		t.Errorf("the refusal does not name the key the caller wrote: %v", err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("a config the caller must edit is not ErrInvalid: %v", err)
	}
}

// A correct profile is not touched: the check is a bound, not a rewrite.
func TestEveryDeclaredKeyStillParses(t *testing.T) {
	e, err := ParseAndValidate([]byte(`kind = "remote"
description = "box"
host = "example.test"
port = 2222
user = "pi"
password = "pw"
trust_unknown_host = true
dial_timeout_seconds = 5

[jump]
host = "bastion.example"
user = "hop"
password = "hpw"
`))
	if err != nil {
		t.Fatalf("a profile using only declared keys was refused: %v", err)
	}
	if e.Port != 2222 || e.Jump == nil {
		t.Fatalf("the parse lost fields: port=%d jump=%v", e.Port, e.Jump)
	}
}
