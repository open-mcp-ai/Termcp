package directive

import (
	"strings"
	"testing"
)

// TestFramePayloadExpandsInsideQuotes pins the rule that made `args` unusable on the
// three frame directives (cdp, jrpc, mcp).
//
// A JSON-RPC frame's quotes are JSON SYNTAX, not shell quoting. Their own help says
// "The frame is the REST of the line. QUOTE IT." — and under shell rules a reference
// inside single quotes is not expanded, so the documented spelling silently disabled
// the substitution channel:
//
//	cdp call h '{"expression":"$1"}'   args: ["2+3"]
//	→ {"expression":"$1"}              the browser received a literal $1
//
// A frame payload therefore ignores the quote state: a reference expands wherever it
// stands, and the value is inserted verbatim because a JSON value must arrive as
// JSON, not as a single-quoted shell word.
func TestFramePayloadExpandsInsideQuotes(t *testing.T) {
	cases := []struct {
		cmd  string
		arg  string
		want string
	}{
		// One verb, one payload rule: whichever door the handle belongs to, `call`
		// carries the frame verbatim, so the three doors are three handles rather than
		// three spellings of the same statement.
		{`call h '{"expression":"$1"}'`, "2+3", `'{"expression":"2+3"}'`},
		{`call h '{"name":"$1"}'`, "term", `'{"name":"term"}'`},
		{`call h '{"m":"$1"}'`, "ping", `'{"m":"ping"}'`},
		// Unquoted frames work too: the old behaviour only ever worked here, which is
		// why the bug went unnoticed.
		{`call h {"id":1,"v":"$1"}`, "x", `{"id":1,"v":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Pos: []string{tc.arg}})
			if err != nil {
				t.Fatalf("ParseWithSubst: %v", err)
			}
			if got := f.Statements[0].Payload; got != tc.want {
				t.Errorf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFramePayloadDoesNotQuoteNamedValues pins the second half: `vars` is the DATA
// channel, and quoting is what makes a value one word for a SHELL. A frame is not
// read by a shell, so the same quoting corrupted it:
//
//	{"e":"$EXPR"}  with EXPR=2+3  →  {"e":"'2+3'"}
//
// The peer then saw the shell quotes as part of the value.
func TestFramePayloadDoesNotQuoteNamedValues(t *testing.T) {
	f, err := ParseWithSubst(`call h '{"e":"$EXPR"}'`,
		Subst{Named: map[string]string{"EXPR": "2+3"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Payload, `'{"e":"2+3"}'`; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
}

// TestShellQuotingIsUnchangedOutsideFrames pins that the fix did not leak: a shell
// command line keeps the shell's reading, because that is what its caller wrote.
func TestShellQuotingIsUnchangedOutsideFrames(t *testing.T) {
	// Inside single quotes a shell expands nothing, so the reference is the shell's — and
	// the value the caller passed is therefore never referenced, which is now its own
	// refusal ("cmd never references $1") rather than a silently dropped argument.
	_, err := ParseWithSubst(`run h:1 '$1'`, Subst{Pos: []string{"x"}})
	if err == nil {
		t.Fatal("a value nothing references must be refused, quoted payload or not")
	}
	if !strings.Contains(err.Error(), "never references $1") {
		t.Errorf("the refusal should name the unused value: %v", err)
	}

	// And a named value in a shell line obeys the same rule as a positional one: it is
	// the whole line or it is refused. Nothing about frames changed that.
	f, err := ParseWithSubst(`run h:1 $V`, Subst{Named: map[string]string{"V": "a b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.Statements[0].Payload, "a b"; got != want {
		t.Errorf("vars payload = %q, want %q", got, want)
	}
}

// TestFrameArgsStillRejectBadIndexes pins that the frame rule did not weaken the
// guards: an out-of-range reference is reported before anything is sent.
func TestFrameArgsStillRejectBadIndexes(t *testing.T) {
	_, err := ParseWithSubst(`call h '{"e":"$2"}'`, Subst{Pos: []string{"only"}})
	if err == nil {
		t.Fatal("an out-of-range reference in a frame must be refused")
	}
	if !strings.Contains(err.Error(), "1 element") {
		t.Errorf("the refusal should count the args: %v", err)
	}
}
