package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// remoteFixture is the TOML a new remote profile starts from. The server no
// longer owns this: the web UI supplies the template (see conn-form.js), so the
// fixture lives here, next to the only code that still needs one.
const remoteFixture = "kind = \"remote\"\n" +
	"# host = \"example.com\"\n" +
	"# user = \"root\"\n" +
	"# port = 22\n"

// writeRemoteSkeleton creates ssh_configs/<name>/config.toml the way a user's
// first save would, standing in for the removed InitRemoteSkeleton helper.
func writeRemoteSkeleton(t *testing.T, dataDir, name string) string {
	t.Helper()
	p := filepath.Join(dataDir, "ssh_configs", name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(remoteFixture), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStoreRemoteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "internal" {
		t.Fatalf("list: %#v", names)
	}
	in, err := s.Load("internal")
	if err != nil || in.Kind != KindInternal {
		t.Fatalf("internal: %+v err %v", in, err)
	}
	// Virtual internal must not create a disk directory.
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "internal")); !os.IsNotExist(err) {
		t.Fatalf("expected no on-disk internal dir, stat err=%v", err)
	}
	raw, err := os.ReadFile(writeRemoteSkeleton(t, dir, "prod"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := toml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["host"] = "h.example"
	m["user"] = "u"
	m["password"] = "p"
	out, _ := toml.Marshal(m)
	if err := s.Save("prod", out); err != nil {
		t.Fatal(err)
	}
	e, err := s.Load("prod")
	if err != nil || e.Host != "h.example" {
		t.Fatalf("load prod: %+v %v", e, err)
	}
	// A directory literally named "internal" is NOT the override: that would make
	// the loopback profile's stored settings depend on a path a user could create
	// by hand as a remote profile. The override lives in .internal/ and is skipped
	// by the list scan.
	if err := os.MkdirAll(filepath.Join(dir, "ssh_configs", "internal"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_configs", "internal", "config.toml"), []byte("kind = \"remote\"\nhost=\"x\"\nuser=\"u\"\npassword=\"p\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	names, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	var internals int
	for _, n := range names {
		if n == "internal" {
			internals++
		}
	}
	if internals != 1 {
		t.Fatalf("list should have exactly one internal, got %#v", names)
	}
	in, err = s.Load("internal")
	if err != nil || in.Kind != KindInternal {
		t.Fatalf("a disk directory named internal must not be mistaken for the override: %+v %v", in, err)
	}
	if in.DefaultApproval {
		t.Error("the internal profile picked up settings from an unrelated disk directory")
	}
	// Saving internal is how the loopback profile is configured; it writes the
	// override and the next Load must see it.
	if err := s.Save("internal", []byte("kind = \"internal\"\ndefault_approval = true\n")); err != nil {
		t.Fatalf("Save(internal) must be allowed (it writes the override): %v", err)
	}
	in, err = s.Load("internal")
	if err != nil {
		t.Fatal(err)
	}
	if !in.DefaultApproval {
		t.Error("the internal override did not take effect")
	}
	if in.Kind != KindInternal {
		t.Errorf("override changed kind to %q", in.Kind)
	}
	if in.Description == "" {
		t.Error("the override dropped the built-in description; absent keys must keep their default")
	}
}

func TestLoad_UnknownReturnsErrNotFound(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Load("no-such-config"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// A profile can make review the default for every session started from it, so
// "all writes to this host are reviewed" is a property of the connection and
// not a switch someone has to remember after each launch.
//
// The field must survive a TOML round trip and default to off for a profile
// that never mentioned it: a missing key meaning "on" would gate sessions whose
// owner never asked.
func TestDefaultApprovalRoundTripsAndDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	off, err := ParseAndValidate([]byte("kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if off.DefaultApproval {
		t.Error("a profile that never set default_approval must not gate: a missing key cannot mean on")
	}
	if EffectiveApproval(off) {
		t.Error("EffectiveApproval must report false for an unset field")
	}
	if EffectiveApproval(nil) {
		t.Error("a nil entry must not gate: a profile that failed to load cannot silently turn review on")
	}

	on, err := ParseAndValidate([]byte("kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\ndefault_approval = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !on.DefaultApproval {
		t.Fatal("default_approval = true did not survive parsing")
	}
	if !EffectiveApproval(on) {
		t.Error("EffectiveApproval must report true when the profile sets it")
	}

	// Marshal/unmarshal is the path that matters: the UI saves by marshalling an
	// Entry back to TOML, so a field lost there would be silently dropped on the
	// first edit.
	body, err := toml.Marshal(on)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseAndValidate(body)
	if err != nil {
		t.Fatal(err)
	}
	if !back.DefaultApproval {
		t.Error("default_approval was lost in a marshal/unmarshal round trip")
	}

	// And through the store, which is what a saved profile actually goes through.
	if err := s.Save("gated", body); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load("gated")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.DefaultApproval {
		t.Error("default_approval was lost through Save/Load")
	}

	// A field set to false must not appear in the TOML at all: a file a human
	// reads should not state a default.
	plain, err := toml.Marshal(off)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "default_approval") {
		t.Errorf("default_approval = false was written to TOML; it is the default and should be omitted:\n%s", plain)
	}
}
