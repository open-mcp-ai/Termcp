package directive

import (
	"strings"
	"testing"
)

// TestAValueRegionIsNotALiteralDollar pins #298.
//
// `put daily $MISSING` with NO vars table was accepted and stored the eight characters
// `$MISSING`; the same line with a table that simply lacked that name was refused with
// "vars does not define $MISSING". One behaviour depended on whether the TABLE was
// empty instead of on whether the NAME had a value — the same call, two answers.
//
// The exemption that made it possible is a real one: with no table, `grep $HOME` is
// the shell's own `$HOME` and must pass through. It applies where a shell reads the
// `$`, and a verb that declared `PayloadType: PayloadVar` said its payload is ONE
// REFERENCE and that the shell text arrives in that value — so inside that region the
// shell expands nothing, and an undefined name has one reading: a misspelling.
//
// This is not a new rule; it is the other half of one that already fired. Non-empty
// table, missing name → refused. Empty table → let it through. A rule that fires in
// one direction is the shape this parser refuses elsewhere.
func TestAValueRegionIsNotALiteralDollar(t *testing.T) {
	// `spanVerbatim` is half of the declaration and not a detail: it is what says the
	// payload is DATA, so no shell reads it. `run` uses the default mode because its
	// value IS a shell line, and there an undefined `$NAME` is still the shell's own —
	// that is why the rule cannot be about the empty table alone.
	g := NewGrammar([]Op{{
		Name: "put", Kind: KindPayload, Target: true, PayloadSpan: spanVerbatim,
		PayloadType: PayloadVar, Usage: "put <tree> $1",
	}})
	cases := []struct {
		name string
		sub  Subst
	}{
		{"empty table", Subst{}},
		{"table that lacks the name", Subst{Named: map[string]string{"OTHER": "x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseGrammar("put daily $MISSING", tc.sub, g)
			if err == nil {
				t.Fatalf("$MISSING was stored as the payload's literal bytes")
			}
			if !strings.Contains(err.Error(), "vars does not define $MISSING") {
				t.Errorf("the refusal must name the variable:\n%v", err)
			}
		})
	}
}

// TestAShellOwnedDollarStaysALiteral is the negative half: the exemption must keep
// applying everywhere a shell can read, or `grep $HOME` stops meaning what it says.
//
// Two shapes that must keep the old reading: a KindFlags verb (its payload is in no
// declared region), and a PayloadVar verb whose payload IS a shell line (`run`, whose
// value the terminal reads) — the probe table in this package writes `run … $cmd` on
// purpose, and it is the counterexample that keeps the strict rule from being about
// "an empty table" instead of "a region no shell reads".
func TestAShellOwnedDollarStaysALiteral(t *testing.T) {
	g := NewGrammar([]Op{{Name: "grep", Kind: KindFlags, Usage: "grep $1"}})
	f, err := ParseGrammar("grep $HOME", Subst{}, g)
	if err != nil {
		t.Fatalf("grep $HOME: %v", err)
	}
	if got := f.Statements[0].Path; got != "grep $HOME" {
		t.Errorf("$HOME did not survive as the shell's own variable, Path = %q", got)
	}

	// And the shell-line value region: `run h:1 $cmd` with no vars hands `$cmd` to the
	// terminal, which expands it. Refusing it would break a documented spelling.
	run := NewGrammar([]Op{{
		Name: "run", Kind: KindPayload, Target: true, PayloadType: PayloadVar,
		Usage: "run <channel> $1",
	}})
	f, err = ParseGrammar("run h:1 $cmd", Subst{}, run)
	if err != nil {
		t.Fatalf("run h:1 $cmd with no vars: %v", err)
	}
	if got := f.Statements[0].Payload; got != "$cmd" {
		t.Errorf("payload = %q, want the shell's own $cmd", got)
	}
}

// TestADeclaredValueRegionSurvivesItsOwnValue pins that the check did not loosen the
// substitution itself: a defined name still fills the payload, which is the case every
// caller of a PayloadVar verb writes.
func TestADeclaredValueRegionSurvivesItsOwnValue(t *testing.T) {
	g := NewGrammar([]Op{{
		Name: "put", Kind: KindPayload, Target: true, PayloadSpan: spanVerbatim,
		PayloadType: PayloadVar, Usage: "put <tree> $1",
	}})
	f, err := ParseGrammar("put daily $BODY", Subst{Named: map[string]string{"BODY": "the text"}}, g)
	if err != nil {
		t.Fatalf("put daily $BODY: %v", err)
	}
	if got := f.Statements[0].Payload; got != "the text" {
		t.Errorf("payload = %q, want the caller's value", got)
	}
}

// TestTheSpacedChannelAddressIsOneWordInBothWalks pins the second defect #298's fix
// uncovered: the span walk and the parser disagreed about how many words a target
// takes.
//
// `run x 1 $1` is the spaced spelling of the channel `x:1`. parseStatement folds the
// two words into one address; statementRawSpans did not, so the payload region started
// at `1` and the reference beside it looked like shell text glued to a literal — the
// line was refused with "a variable cannot sit inside a shell line" about a line that
// has no shell text in it at all. One fold, two walks, one rule.
func TestTheSpacedChannelAddressIsOneWordInBothWalks(t *testing.T) {
	f, err := ParseWithSubst("run x 1 $1", Subst{Pos: []string{"echo hi"}})
	if err != nil {
		t.Fatalf("the spaced channel address was not folded before the payload: %v", err)
	}
	st := f.Statements[0]
	if st.Target != "x:1" {
		t.Errorf("target = %q, want the folded address x:1", st.Target)
	}
	if st.Payload != "echo hi" {
		t.Errorf("payload = %q, want the caller's value", st.Payload)
	}
}
