package directive

import (
	"strings"
	"testing"
)

// TestAValueNothingNamesIsRefused.
//
// The mirror of "vars does not define $X" was silent: `cmd: "run h:1 $1"` with
// `args: ["a","b","c"]` ran `a` and dropped two values without a word, so a caller — or a
// model — that passed three and referenced one believed all three had arrived. A rule
// that only fires in one direction is how a table that is wrong by one entry stays wrong.
func TestAValueNothingNamesIsRefused(t *testing.T) {
	_, err := ParseWithSubst("run h:1 $1", Subst{Pos: []string{"a", "b", "c"}})
	if err == nil {
		t.Fatal("two unreferenced args were accepted")
	}
	for _, want := range []string{"$2", "$3", "never references"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %s", want, err)
		}
	}
	if strings.Contains(err.Error(), "$1") {
		t.Errorf("the refusal names the value that WAS used: %s", err)
	}

	// vars, the same way.
	if _, err := ParseWithSubst("run h:1 $CMD", Subst{Named: map[string]string{"CMD": "ls", "EXTRA": "rm"}}); err == nil {
		t.Fatal("an unreferenced var was accepted")
	} else if !strings.Contains(err.Error(), "$EXTRA") {
		t.Errorf("the refusal does not name the unused var: %s", err)
	}

	// An empty cmd cannot reference anything, so a table with it is a mistake too.
	if _, err := ParseWithSubst("   ", Subst{Pos: []string{"a"}}); err == nil {
		t.Fatal("args with an empty cmd were accepted")
	}

	// The legitimate shapes stay legitimate: every value named, and a value named twice.
	if _, err := ParseWithSubst("run h:1 $1; run h:1 $1", Subst{Pos: []string{"echo hi"}}); err != nil {
		t.Errorf("a value referenced twice was refused: %v", err)
	}
	if _, err := ParseWithSubst("run h:1 $1", Subst{Pos: []string{"echo hi"}}); err != nil {
		t.Errorf("a single referenced value was refused: %v", err)
	}
	if _, err := ParseWithSubst("ls host", Subst{}); err != nil {
		t.Errorf("a call with no values at all was refused: %v", err)
	}
}
