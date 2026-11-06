package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// A TARGET THAT NAMES NOTHING MUST SAY SO, NOT ANSWER ABOUT A DIFFERENT COLLECTION.
//
// `ls nosuchthing` fell through to the same filtered listing a real session gets, the
// filter matched nothing, and the reply was the GLOBAL sentence "(nothing is running)"
// plus the hint about `ls host` — a sentence about a different collection, answering a
// different question. A caller asking "what is on this link" was told the machine has no
// sessions at all, and a stale address read as a fact about the world (found by
// dogfooding; same shape as #148 one branch up).
func TestLsOfAnUnknownTargetSaysItDoesNotExist(t *testing.T) {
	s := newTestServer(t)

	slots := s.directiveLS(context.Background(), directive.Statement{Op: "ls", Target: "nosuchthing"}, aWindow())
	if len(slots) != 1 {
		t.Fatalf("ls answered %d slots, want 1", len(slots))
	}
	slot := slots[0]
	if slot.Code != 404 {
		t.Fatalf("ls of an unknown target = %d, want 404; body: %s", slot.Code, slot.Body)
	}
	if !strings.Contains(slot.Body, "no session") {
		t.Fatalf("the refusal does not say the target does not exist:\n%s", slot.Body)
	}
	if strings.Contains(slot.Body, "nothing is running") {
		t.Fatalf("the reply is the global sentence, which is about a different collection:\n%s", slot.Body)
	}

	// The global listing keeps its own answer: the fix must not turn `ls` into a refusal.
	bareSlots := s.directiveLS(context.Background(), directive.Statement{Op: "ls"}, aWindow())
	if len(bareSlots) != 1 {
		t.Fatalf("bare ls answered %d slots, want 1", len(bareSlots))
	}
	bare := bareSlots[0]
	if bare.Code != 200 {
		t.Fatalf("bare ls = %d, want 200; body: %s", bare.Code, bare.Body)
	}
	// And a target that DOES exist still lists that session's channels.
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid := strings.Split(h, ":")[0]
	liveSlots := s.directiveLS(context.Background(), directive.Statement{Op: "ls", Target: sid}, aWindow())
	if len(liveSlots) != 1 {
		t.Fatalf("ls of a live session answered %d slots, want 1", len(liveSlots))
	}
	live := liveSlots[0]
	if live.Code != 200 || !strings.Contains(live.Body, h) {
		t.Fatalf("ls of a live session = %d, body:\n%s", live.Code, live.Body)
	}

	// AND BY ITS PROFILE NAME, WHICH IS THE SPELLING THE FILTER ACCEPTS TOO.
	//
	// `ls internal` is how a caller lists the session on the internal profile, and the
	// first version of this fix refused it: the check knew about session ids while the
	// listing matched on the id OR the profile, so a target that had always worked came
	// back 404. The browser smoke test caught it; this pins it without needing Chrome.
	byProfile := s.directiveLS(context.Background(), directive.Statement{Op: "ls", Target: "internal"}, aWindow())
	if len(byProfile) != 1 {
		t.Fatalf("ls internal answered %d slots, want 1", len(byProfile))
	}
	if byProfile[0].Code != 200 || !strings.Contains(byProfile[0].Body, h) {
		t.Fatalf("ls <profile> = %d, body:\n%s", byProfile[0].Code, byProfile[0].Body)
	}
}

// THE ERROR CODE NAMES THE THING THAT IS MISSING.
//
// `get host nosuchthing` answered `session_not_found` with a body about an ssh config:
// the header sent the caller looking for a session while the body said the profile was
// missing, and the two disagree about which collection was searched.
// `ssh_config_not_found` already existed — sshConfigErrCode answers it for the same
// store — so the code was simply not the one this path used.
func TestAMissingHostIsNotReportedAsAMissingSession(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"cmd": "get host nosuchprofile"})
	if !strings.Contains(got, "Error-Code: ssh_config_not_found") {
		t.Fatalf("get host of a missing profile does not report ssh_config_not_found:\n%s", got)
	}
	if strings.Contains(got, "session_not_found") {
		t.Fatalf("the reply blames a missing session for a missing profile:\n%s", got)
	}
}

// AN ARGUMENT HAS A PAGE, BECAUSE THE GUIDE NAMES IT.
//
// The first screen names `args`, `vars` and `secrets`, so `help secrets` is the next
// thing a caller types — and it answered "unknown directive: secrets" while the page
// already existed in the argument table that the schema and the docs render from. The
// page is derived from that table, so it cannot drift from the schema.
func TestHelpAnswersForAnArgument(t *testing.T) {
	s := newTestServer(t)
	for _, name := range []string{"args", "vars", "secrets"} {
		slot := s.directiveHelp(directive.Statement{Op: "help", Args: []string{name}}, aWindow())
		if slot.Code != 200 {
			t.Fatalf("help %s = %d, want 200; body: %s", name, slot.Code, slot.Body)
		}
		if !strings.Contains(slot.Body, "argument of the `term` tool") {
			t.Errorf("help %s does not say what kind of word it is:\n%s", name, slot.Body)
		}
		decl, ok := termArgDeclFor(name)
		if !ok {
			t.Fatalf("%s is not a declared argument; the test is asserting about nothing", name)
		}
		// The page is the declaration, not a copy of it.
		if !strings.Contains(slot.Body, decl.Desc) {
			t.Errorf("help %s does not carry the declaration's description, so the page and "+
				"the schema can disagree:\n%s", name, slot.Body)
		}
	}
	// A word that is neither a verb, a node nor an argument keeps its 400.
	if slot := s.directiveHelp(directive.Statement{Op: "help", Args: []string{"nosuchword"}}, aWindow()); slot.Code != 400 {
		t.Fatalf("help nosuchword = %d, want 400", slot.Code)
	}
}

// A POINTER IN A REFUSAL MUST LEAD SOMEWHERE REAL.
//
// The message for "this directive needs a session, not a channel" told the caller that
// `ps` prints the session id in the first column. `ps` is not a directive any more — it
// is `ls` — so the one sentence that exists to unstick a caller sent them at a 400.
func TestTheSessionTargetRefusalPointsAtARealDirective(t *testing.T) {
	msg := directive.SessionTargetProblem(directive.Op{Name: "rm"}, "a1f3:1")
	if strings.Contains(msg, "`ps`") {
		t.Fatalf("the refusal points at `ps`, which is not a directive:\n%s", msg)
	}
	if !strings.Contains(msg, "`ls`") {
		t.Fatalf("the refusal does not point at the verb that prints the id:\n%s", msg)
	}
	if _, ok := directive.Lookup("ls"); !ok {
		t.Fatal("the pointer names a verb this build does not have")
	}
}
