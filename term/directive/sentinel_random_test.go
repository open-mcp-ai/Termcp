package directive

import (
	"strings"
	"testing"
)

// TestTheSentinelIsRandomPerCall pins the property that makes a leaked hole obvious.
//
// The token used to be `__hole_<n>__`: fixed prefix, sequential index, so it was
// PREDICTABLE. A predictable token is one a caller can type, which collapsed two
// different facts into the same string — "this is a hole the fill pass has not reached"
// and "this is what the caller wrote". After the fill bug, `write h file $1` wrote a
// file literally named `__hole_0__` and nothing could tell that apart from a caller
// who had chosen that name on purpose.
//
// Random per call separates them by construction: two calls never mint the same token,
// and no caller can guess one.
func TestTheSentinelIsRandomPerCall(t *testing.T) {
	const cmd = "write h file $1 --data $2"

	// Two parses of the SAME cmd with the same values must not share a hole name.
	// Reading it back requires the unexpanded statement, which is what the parser sees
	// before expandStatement runs — so this asks the table directly.
	src1 := "write h file __HOLE__ --data x"
	p1 := sentinelPrefix(src1)
	p2 := sentinelPrefix(src1)
	if p1 == p2 {
		t.Errorf("two calls minted the same sentinel %q; it is not random", p1)
	}
	for _, p := range []string{p1, p2} {
		if !strings.HasPrefix(p, sentinelMarker) {
			t.Errorf("sentinel %q does not carry the marker %q, so a leak is unrecognizable", p, sentinelMarker)
		}
		// It has to be ONE ordinary shell word: the parse runs against a line the
		// caller's data cannot reshape.
		if strings.ContainsAny(p, " \t\n'\"$`;&|<>()") {
			t.Errorf("sentinel %q is not a single plain shell word", p)
		}
	}

	// And the values still arrive: randomness must not break the fill.
	f, err := ParseWithSubst(cmd, Subst{Pos: []string{"/tmp/x", "hello"}})
	if err != nil {
		t.Fatalf("ParseWithSubst(%q): %v", cmd, err)
	}
	st := f.Statements[0]
	if st.Key != "/tmp/x" {
		t.Errorf("key = %q, want /tmp/x", st.Key)
	}
	if d, _ := st.Flag("data"); d != "hello" {
		t.Errorf("data = %q, want hello", d)
	}
}

// TestACallerCannotForgeAHole pins the other half: the marker is not a secret, so a
// caller CAN write a string that looks like one — and must still get its own bytes
// back, because the fill is single-pass over the tokens it minted itself.
func TestACallerCannotForgeAHole(t *testing.T) {
	for _, value := range []string{
		"__hole_0011223344556677__",
		"a __hole_deadbeefdeadbeef__ b",
		sentinelMarker,
	} {
		f, err := ParseWithSubst("write h file $1", Subst{Pos: []string{value}})
		if err != nil {
			t.Fatalf("ParseWithSubst: %v", err)
		}
		if got := f.Statements[0].Key; got != value {
			t.Errorf("key = %q, want the caller's own bytes %q", got, value)
		}
	}
}

// TestALeakedHoleIsRecognizable pins why the marker survives at all.
//
// Randomness means a reader cannot know the exact token, so the leak check has to match
// on the marker. This is the check that turns "a path that happens to look odd" into a
// refusal — the difference between the reported bug (a file created under a name nobody
// wrote) and a typo the caller can see.
func TestALeakedHoleIsRecognizable(t *testing.T) {
	leaked := sentinelPrefix("some cmd") + "3__"
	if !isSentinel(leaked) {
		t.Errorf("a hole minted by this package is not recognized as one: %q", leaked)
	}
	for _, ok := range []string{"/tmp/x", "", "report.txt", "a__tmb"} {
		if isSentinel(ok) {
			t.Errorf("%q is the caller's own text and must not be treated as a hole", ok)
		}
	}
}
