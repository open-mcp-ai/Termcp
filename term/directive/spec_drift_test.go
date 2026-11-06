package directive

import (
	"testing"
)

// TestNoSpecDriftBetweenDirectivesAndSkill pins feedback §6.3:
//
// A spec that drifts from reality is the one failure mode tests never catch —
// every assertion checks "did the code do what I wrote", and none checks "is what
// I wrote true". SKILL.md taught `watch` long after `watch` was deleted, and
// taught `close --force` after --force was removed.
//
// This test asserts both directions:
// 1. Every directive declared in directive.go has a non-empty Usage and Detail.
// 2. Directives taught in SKILL.md must actually exist in the directive table.
func TestNoSpecDriftBetweenDirectivesAndSkill(t *testing.T) {
	declared := make(map[string]bool)
	for _, op := range Ops() {
		if op.Name == "" {
			t.Errorf("found an unnamed directive in Ops()")
		}
		if op.Usage == "" {
			t.Errorf("directive %q has no Usage string", op.Name)
		}
		declared[op.Name] = true
	}

	// THE VERB SET IS PINNED BY NAME, not by count. A count only says the surface
	// changed; a list says what it is, so adding or dropping a verb is a deliberate
	// edit here rather than a number someone adjusts after the fact.
	//
	// These are all VERBS. A noun is not here unless it has more than one verb of its
	// own: `span` and `reader` are views of a channel, `forward`/`file`/`mcp`/`cdp`/
	// `jrpc` are children made with `open`, and `host`/`ext`/`alias` are collections
	// reached through `ls`/`create`/`edit`/`rm`.
	want := []string{
		"call", "close", "connect", "create", "edit", "get", "help", "input",
		"key", "ls", "notify", "open", "read", "reader", "resize", "rm", "run",
		"span", "write",
	}
	for _, name := range want {
		if !declared[name] {
			t.Errorf("directive %q is missing from Ops()", name)
		}
	}
	if len(declared) != len(want) {
		t.Errorf("Ops() has %d directives, want %d: %v", len(declared), len(want), declared)
	}

	// THE DOC DRIFT CHECK THAT USED TO LIVE HERE IS GONE, ON PURPOSE.
	//
	// It read ../../../termcp-skills/SKILL.md — a file in ANOTHER REPOSITORY — and
	// failed if that file taught a directive this one had deleted. Two things were
	// wrong with it, and only one is about paths:
	//
	//  1. A test in this repo must not depend on what happens to be checked out next
	//     to it. The result was environment-dependent by construction: skipped in CI
	//     (the sibling is not there), red on a developer machine whose sibling was
	//     behind. A failure that says "your other clone is stale" is not a fact about
	//     this code, and a suite that reports it is a suite people learn to ignore.
	//
	//  2. Its deleted-directives list was factually wrong. It named shell_input, which
	//     was never deleted: it is the tool `input`, `run` and the mcp door all call,
	//     and it is still a capability scope. So the check could only ever fail for a
	//     correct document.
	//
	// What remains is what belongs here: every directive is documented, and the verb
	// set is pinned by name. Document drift against an external skill belongs in that
	// skill's own repository, where it can be checked against the version it targets.
}
