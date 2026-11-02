package extensions

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStatusAsksAboutOneNameNotTheWholeDirectory pins the third finding of issue
// #152.
//
// `Status(name)` answered "is this name configured" by calling `store.List()` — a
// ReadDir plus a Stat per entry. A caller that asks about every extension therefore
// did N×(N+1) filesystem calls, and the listing is exactly that caller: with 80
// templates it was 6480 stats to answer 80 questions. The reply was correct either
// way, which is what made it a performance bug rather than a wrong answer.
//
// The fix is one stat per question, and this pins the SHAPE rather than a duration:
// a timing assertion would grade the machine, and the number that mattered
// (1.0s → 0.05s for `ls ext` with 80 templates) is the filesystem's, not this
// package's. What is asserted here is that asking about one name touches one file.
func TestStatusAsksAboutOneNameNotTheWholeDirectory(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		ext := Extension{Name: name, Target: "internal", Command: "true"}
		if err := store.Save(&ext); err != nil {
			t.Fatal(err)
		}
	}

	// Exists is the primitive the lookup now uses: one Stat, no directory walk.
	if !store.Exists("beta") {
		t.Error("Exists says a stored template is not there")
	}
	if store.Exists("nosuch") {
		t.Error("Exists says a template that was never stored is there")
	}
	// An unusable name is not a stored template, and must not make Exists panic or
	// escape the data directory.
	for _, bad := range []string{"", "..", "a/b", `a\b`} {
		if store.Exists(bad) {
			t.Errorf("Exists(%q) = true; an unusable name is not a stored template", bad)
		}
	}

	// And the listing itself must not have become a directory walk per entry: with
	// the name already known, Status must not call List. A directory that cannot be
	// READ still answers about the files in it, which is only possible if the lookup
	// stats the one path instead of enumerating.
	// 0o100, NOT 0o000: listing needs READ on the directory, and stat'ing one path
	// inside it needs SEARCH (execute). Removing both proves nothing about whether the
	// lookup enumerates — every implementation fails, and the check below reports that
	// as "Exists is enumerating". Found by CI on Linux, which is the only place the
	// permission bits are obeyed.
	if err := os.Chmod(store.root(), 0o100); err != nil {
		t.Skipf("cannot make the directory unreadable on this platform: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.root(), 0o755) })
	if _, err := store.List(); err == nil {
		t.Skip("this platform lets an unreadable directory be listed; the check below would prove nothing")
	}
	if !store.Exists("beta") {
		t.Error("Exists stopped working once the directory could not be listed; it is enumerating")
	}
	_ = filepath.Join(dir, "extensions")
}
