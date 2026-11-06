package directive

import (
	"strings"
	"testing"
)

// TestTheRunPageSaysItTypesIntoATerminal.
//
// An issue reported four symptoms of one misreading: the caller read `run` as "submit a
// job and tell me when it is done", so a line eaten by a program waiting for input
// looked like a command that ran, and a 204 looked like a command that never started.
// The tool cannot detect the foreground process (it lives on the far side of the SSH
// connection, and a Windows terminal has no such notion), so the honest fix is the
// model, stated where the caller reads it — and stated once, not re-derived per symptom.
func TestTheRunPageSaysItTypesIntoATerminal(t *testing.T) {
	for _, want := range []string{
		"THIS TYPES INTO A TERMINAL",
		"OWNS THE CHANNEL'S INPUT",
		"takes your line as its ANSWER",
		// A pipe is not the clean case: its stdin is a real stdin.
		"THE SAME IS TRUE OF A PIPE",
	} {
		if !strings.Contains(runDetail, want) {
			t.Errorf("the run page no longer says %q, so the caller is back to guessing what run does", want)
		}
	}
}

// TestTheReplysTypedByHeaderIsDocumented.
//
// A header the caller cannot look up is a header it will ignore. Typed-By: api is the
// one fact about "who else is on this channel" termcp can prove, so the pages that
// produce it and the index it comes from all have to name it.
func TestTheReplysTypedByHeaderIsDocumented(t *testing.T) {
	if !strings.Contains(runDetail, "Typed-By: api") {
		t.Error("help run does not name the header its own replies carry")
	}
	if !strings.Contains(readDetail, "Typed-By: api") {
		t.Error("help read does not name the header its own replies carry")
	}
	if !strings.Contains(spanDetail, "Typed-By: api") {
		t.Error("help span does not connect the input statuses to the header they produce")
	}
	// AND IT MUST NOT BE SOLD AS A GUARANTEE. A shared channel is normal — a person can
	// type at any moment — so the absence of the header proves only that no submitted
	// person-typed line starts in that window. Documenting it as "the channel is clean"
	// would be a promise termcp cannot keep.
	if !strings.Contains(readDetail, "NOT A CLEANLINESS GUARANTEE") {
		t.Error("help read does not warn that an absent header is not a cleanliness guarantee")
	}
	if !strings.Contains(runDetail, "SOMEONE ELSE MAY BE USING THIS CHANNEL") {
		t.Error("help run does not say the channel may be shared")
	}
	// The index is the authoritative record, so `span` has to describe what it holds.
	for _, want := range []string{"RECORD OF WHO PUT WHICH BYTES HERE", "who typed this, and when"} {
		if !strings.Contains(spanDetail, want) {
			t.Errorf("help span does not say %q, so the input record is invisible", want)
		}
	}
}

// TestTheRollbackHeaderIsDocumented. A header the caller cannot look up is a header it
// will ignore, and this one exists precisely because the reply looks like a plain short
// window. The read page is where a caller looks after seeing it.
func TestTheRollbackHeaderIsDocumented(t *testing.T) {
	for _, want := range []string{"Trimmed: bytes A-B withheld", "read --cursor A", "WHY WITHHOLD AND NOT HAND THE FRAGMENT OVER"} {
		if !strings.Contains(readDetail, want) {
			t.Errorf("help read does not explain the header a rolled-back window carries (%q)", want)
		}
	}
}

// TestTheRunPageSaysTheInterfaceIsImprecise.
//
// The reports that keep arriving about a pty share one expectation: that the tool should
// know whether the command ran, whether something holds the foreground, whether the line
// was eaten. A person at the same screen cannot know any of that either — they read the
// screen — and the terminal has never offered a sharper sense. That is the usable model,
// so it is stated as a rule rather than re-argued per report.
func TestTheRunPageSaysTheInterfaceIsImprecise(t *testing.T) {
	for _, want := range []string{
		// The rule itself.
		"THE INTERFACE IS IMPRECISE, AND THAT IS THE INTERFACE",
		// What to do instead of asking for a state.
		"USE IT THE WAY A HUMAN USES ONE",
		// And the negative fact an empty reply must not be read as.
		"an empty window is not evidence that nothing happened",
	} {
		if !strings.Contains(runDetail, want) {
			t.Errorf("help run no longer says %q, so a caller is back to expecting a precise "+
				"state from an interface that has none", want)
		}
	}
}
