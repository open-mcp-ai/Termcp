package keys

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// THE FILE IS CREATED WITH THE MODE THAT IS THE AUTHORIZATION, so the positive
// case and the mode are asserted together: a key that exists but is readable by
// everyone is the failure this package exists to prevent, and asserting only
// "the file exists" would pass on it.
func TestLoadCreatesAKeyOnlyTheOwnerCanRead(t *testing.T) {
	dir := t.TempDir()
	root, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if root.Kid == "" || len(root.Public) == 0 || len(root.private) == 0 {
		t.Fatalf("incomplete key: %+v", root)
	}
	if root.Path != filepath.Join(dir, "access", RootName) {
		t.Errorf("path = %q, want the default under the data dir", root.Path)
	}

	info, err := os.Stat(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Fatalf("key mode = %04o; group/other must have no access", mode)
		}
	}

	// A second load must return the same material, not a new key. Regenerating on
	// every start would make every previously issued token unverifiable.
	again, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.Kid != root.Kid {
		t.Fatalf("a reload produced a different key (%s vs %s)", again.Kid, root.Kid)
	}
}

// A LOOSE MODE IS REFUSED ON READ, not only prevented on create.
//
// The file outlives this process and chmod happens elsewhere, so a check that
// only runs when creating would leave an existing 0644 key trusted forever. This
// is the case that matters: an operator (or a backup restore) making the key
// readable is exactly how the whole system quietly becomes open.
func TestLoadRefusesAKeyAnyAccountCouldRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not exist here; the check is skipped on this platform by design")
	}
	dir := t.TempDir()
	root, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root.Path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Addr{Dir: dir}.Load()
	if err == nil {
		t.Fatal("a world-readable root key must be refused")
	}
	// The message has to say what to do. "permission denied" would send an
	// operator looking at the daemon's account instead of at the key.
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}
}

// THE INLINE-ENV SPELLING IS REFUSED, and by name.
//
// It is a different variable on purpose: an operator who sets it made a visible
// choice, and the answer must explain why the bytes are not accepted here rather
// than acting as if the file were missing.
func TestLoadRefusesKeyMaterialFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvInlineKey, "whatever")
	_, err := Addr{Dir: dir}.Load()
	if err == nil {
		t.Fatal("an inline key in the environment must be refused")
	}
	if !strings.Contains(err.Error(), EnvKeyFile) {
		t.Errorf("the refusal does not name the path variable to use instead: %v", err)
	}
}

// No data directory, no override, no environment: refuse rather than guess.
// A default derived from the working directory would make the key's identity
// depend on where the binary was launched.
func TestPathRefusesToGuess(t *testing.T) {
	t.Setenv(EnvKeyFile, "")
	if _, err := (Addr{}).Path(); err == nil {
		t.Fatal("an address with nothing set must refuse to resolve")
	}
}

// The documented precedence: explicit file > environment > default.
func TestPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "from-env.key")
	t.Setenv(EnvKeyFile, envPath)

	got, err := Addr{Dir: dir}.Path()
	if err != nil {
		t.Fatal(err)
	}
	if got != envPath {
		t.Errorf("environment should beat the default: %q", got)
	}

	explicit := filepath.Join(dir, "explicit.key")
	got, err = Addr{Dir: dir, File: explicit}.Path()
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Errorf("the explicit path should beat the environment: %q", got)
	}
}

// A KID THAT DISAGREES WITH ITS MATERIAL IS A HAND-EDIT OR A MIX-UP.
//
// The kid is what the daemon maps to a permission template, so a file allowed to
// claim any kid could point a token at a template the material was never meant
// for. It is checked against the hash rather than trusted.
func TestLoadRefusesAMismatchedKid(t *testing.T) {
	dir := t.TempDir()
	root, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `kid = "`+root.Kid+`"`, `kid = "0000000000000000"`, 1)
	if edited == string(raw) {
		t.Fatal("the test did not actually edit the kid")
	}
	if err := os.WriteFile(root.Path, []byte(edited), fileMode); err != nil {
		t.Fatal(err)
	}

	if _, err := (Addr{Dir: dir}).Load(); err == nil {
		t.Fatal("a kid that does not match the key material must be refused")
	}
}

// Rotate is the only revocation a file-based key has, so it must both produce new
// material and report the kid it replaced.
func TestRotateReplacesMaterialAndReportsTheOldKid(t *testing.T) {
	dir := t.TempDir()
	first, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}

	oldKid, second, err := Addr{Dir: dir}.Rotate()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if oldKid != first.Kid {
		t.Errorf("rotate reported old kid %q, want %q", oldKid, first.Kid)
	}
	if second.Kid == first.Kid {
		t.Error("rotation produced the same kid, so nothing was replaced")
	}

	reloaded, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Kid != second.Kid {
		t.Errorf("the rotated key was not installed: reloaded %q, want %q", reloaded.Kid, second.Kid)
	}
}

// Signature round-trip, because the material is only useful if a second process
// loading the same file can verify what the first one signed.
func TestSignatureVerifiesFromASecondLoad(t *testing.T) {
	dir := t.TempDir()
	signer, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := Addr{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}

	msg := []byte("token bytes")
	sig := signer.Sign(msg)
	if !verifier.Verify(msg, sig) {
		t.Fatal("a signature made with the loaded key must verify with the same file")
	}
	if verifier.Verify([]byte("different bytes"), sig) {
		t.Fatal("a signature must not verify for different content")
	}
}
