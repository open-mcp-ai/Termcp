package directive

import (
	"strings"
	"testing"
)

// TestTheHolePrefixNeverCollides pins the invariant the fill pass depends on.
//
// `expand` finds holes by scanning the source for the prefix, so a prefix that appears
// in the caller's bytes would make the scan read THEIR text as a hole. Randomness does
// not fix that — it only makes it unlikely — so the check is the property that matters
// and it is tested directly, because no test can force a random prefix to collide.
func TestTheHolePrefixNeverCollides(t *testing.T) {
	cases := []struct {
		name string
		src  string
		seed string
	}{
		{"a clean source takes the seed as-is", "write h file __HOLE__", "__hole_abc_"},
		{"a source that already holds it is extended", "x __hole_abc_ y", "__hole_abc_"},
		{"and again, because one underscore is not enough", "__hole_abc_ and ___hole_abc_", "__hole_abc_"},
		{"the bare marker is the fallback seed", "a __hole_ b", "__hole_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uniquePrefix(tc.src, tc.seed)
			if strings.Contains(tc.src, got) {
				t.Errorf("uniquePrefix returned %q, which the source already contains: %q", got, tc.src)
			}
			// It must still carry the seed, or the token stops looking like a hole and
			// `isSentinel` cannot recognize a leak.
			if !strings.Contains(got, tc.seed) {
				t.Errorf("uniquePrefix returned %q, which no longer contains the seed %q", got, tc.seed)
			}
		})
	}
}

// TestASourceFullOfTheMarkerStillParses pins the collision path end to end: a caller
// whose cmd carries marker text still gets its values filled, because the prefix was
// extended rather than reused.
func TestASourceFullOfTheMarkerStillParses(t *testing.T) {
	// The marker appears in the cmd itself, as a literal the caller wrote. A naive fixed
	// prefix would find that text and read it as a hole.
	//
	// The marker sits in the PATH and the reference in the flag, because a reference has
	// to be a whole word: `--data __hole__ $1` would be a value followed by a stray word,
	// which the parser refuses rather than dropping.
	f, err := ParseWithSubst(
		`write h file __hole__ --data $1`,
		Subst{Pos: []string{"value"}},
	)
	if err != nil {
		t.Fatalf("ParseWithSubst: %v", err)
	}
	st := f.Statements[0]
	// The marker text the CALLER wrote stays their bytes: the collision path is what
	// makes a marker-shaped literal survive while a reference beside it is filled.
	if st.Key != "__hole__" {
		t.Errorf("key = %q, want the caller's own marker text", st.Key)
	}
	d, _ := st.Flag("data")
	if d != "value" {
		t.Errorf("data = %q, want the reference filled in", d)
	}
}
