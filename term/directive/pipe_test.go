package directive

import (
	"strings"
	"testing"
)

// TestAPipeIsTheOnePipelineThisLanguageHas pins the shape and the refusals.
//
// Before this, `read a1f3 | write a2b4 file /x` parsed as ONE statement — `read` —
// and the right side was thrown away with err == nil. A caller that asked for a copy
// got a read and a 200.
func TestAPipeIsTheOnePipelineThisLanguageHas(t *testing.T) {
	f, err := Parse(`read a1f3:1 | write a2b4 file /tmp/x`)
	if err != nil {
		t.Fatalf("read | write must parse: %v", err)
	}
	if len(f.Statements) != 1 {
		t.Fatalf("a pipeline is one statement in the reply, got %d", len(f.Statements))
	}
	left := f.Statements[0]
	if left.Op != "read" || left.Target != "a1f3:1" {
		t.Fatalf("left side = %s %q", left.Op, left.Target)
	}
	if left.Pipe == nil {
		t.Fatal("the right side was dropped again")
	}
	if left.Pipe.Op != "write" || left.Pipe.Kind != "file" || left.Pipe.Key != "/tmp/x" {
		t.Fatalf("right side = %s %q %q", left.Pipe.Op, left.Pipe.Kind, left.Pipe.Key)
	}
	if left.Pipe.Target != "a2b4" {
		t.Fatalf("the destination session was lost: %q", left.Pipe.Target)
	}
	if !left.Pipe.Piped {
		t.Error("the right half does not know it is piped, so it could be run on its own")
	}
	// The echo has to be the whole line: a receipt that echoes only `read …` reads
	// like the copy was never asked for.
	if !strings.Contains(left.Path, "|") {
		t.Errorf("the reply would echo %q, which does not show the copy", left.Path)
	}
}

// TestAPipeIsRefusedWhereItIsNotThatPipeline. Every one of these used to be a silent
// drop of everything to the right of the `|`.
func TestAPipeIsRefusedWhereItIsNotThatPipeline(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`read a1f3:1 | grep x`, "must be one"},
		{`read a1f3:1 | read a2b4:1`, "only as `read"},
		{`close a1f3:1 | write a2b4 file /x`, "only as `read"},
		{`write a1f3 file /x --data hi | read a2b4:1`, "only as `read"},
		{`read a1f3:1 |& write a2b4 file /x`, "`|&`"},
		{`read a1f3:1 | write a2b4 file /x | write a3c5 file /y`, "exactly two ends"},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil {
			t.Errorf("%s: accepted, and the right side would be silently dropped or guessed", c.src)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error does not name the rule:\n%v", c.src, err)
		}
	}
}

// TestAPayloadDoesNotKeepItsPipe pins the end of the two-language ambiguity.
//
// `run h:1 $1 | wc -l` used to mean "the remote shell pipes `$1` into wc", because the
// payload of a payload verb was the rest of the line and the `|` in it belonged to the
// shell. That is the same text in which a termcp `|` is an operator, so which language
// was speaking could only be decided by knowing the verb — and the mistake it invites
// is silent: `run h:1 ls | write h:2 file /x` asks the REMOTE shell for a program
// called `write`, which on Linux exists (it messages another user's terminal).
//
// A payload is now exactly one reference, so the pipe has nowhere to hide: it is
// either the value's own text (`args: ["ls | wc -l"]`) or it is refused.
func TestAPayloadDoesNotKeepItsPipe(t *testing.T) {
	if _, err := Parse(`run h:1 $1 | wc -l`); err == nil {
		t.Fatal("a shell pipeline written beside a reference was accepted")
	}

	// Inside the value it is the shell's, and nothing rescans it.
	f, err := ParseWithSubst(`run h:1 $1`, Subst{Pos: []string{"ls | wc -l"}})
	if err != nil {
		t.Fatalf("a pipeline inside the value must be the caller's: %v", err)
	}
	if got := f.Statements[0].Payload; got != "ls | wc -l" {
		t.Fatalf("payload = %q", got)
	}
	if f.Statements[0].Pipe != nil {
		t.Fatal("a pipe inside a value was read as a termcp pipeline")
	}
}

// TestAPipeOnASubstitutedTargetWaitsForTheValue: the pair is built at parse time, so
// the target check that runs after substitution has to see the RIGHT side too. A
// wrong kind there would otherwise reach the dispatcher.
func TestAPipeOnASubstitutedTargetWaitsForTheValue(t *testing.T) {
	f, err := ParseWithSubst(`read $1 | write a2b4 file /x`, Subst{Pos: []string{"a1f3:1"}})
	if err != nil {
		t.Fatalf("a substituted channel is a channel: %v", err)
	}
	if f.Statements[0].Pipe == nil {
		t.Fatal("the pipeline was lost when the target came from args")
	}
}

// TestAnUnspacedPipeIsTheOperatorToo. There is no lexical exception: every `|` between
// two verbs is the pipeline operator, spaced or not. The usage lines' `a|b`
// alternatives are documentation, and the test that parses those lines reduces them
// before handing them to the parser — the parser does not carry a special case for
// prose.
func TestAnUnspacedPipeIsTheOperatorToo(t *testing.T) {
	if _, err := Parse(`read a1f3:1|write a2b4 file /x`); err != nil {
		t.Fatalf("an unspaced pipeline must still parse: %v", err)
	}
	// And the silent drop is gone in this spelling too.
	if _, err := Parse(`read a1f3:1|grep x`); err == nil {
		t.Error("an unspaced pipe on a non-write right side is still silently dropped")
	}
}
