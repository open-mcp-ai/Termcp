package mcp

// `args` TAKES THE TWO SHAPES THE SUBSTITUTION ENGINE ALREADY HAS.
//
// The array form is positional ($1, $2, …) and unchanged. The object form is named
// ($NAME), and it exists for one caller: the one passing a JSON frame. Before it, `call
// s:1 cdp:1 $FRAME` had to be spelled as a quoted JSON string inside the value, and
// getting that quoting wrong is not visible anywhere — the frame simply arrives
// malformed. With the object form the frame travels as the object it is and is
// substituted as its JSON text.
//
// The rule that keeps this from guessing: a STRING is verbatim (exactly as before),
// every other JSON value is its compact JSON spelling. Every assertion below reads the
// expansion out of a dry run, which is the one place the substituted line is printed.

import (
	"strings"
	"testing"
)

// ONE ASSERTION PER PEER SPELLING, because there is one implementation: the object form
// is expanded during substitution, above every directive, so cdp/mcp/jrpc cannot drift
// apart in what they accept. This test is what keeps that a fact rather than a claim.
func TestArgsObjectCarriesJSONIntoTheReference(t *testing.T) {
	s := newTestServer(t)
	want := `{"method":"Page.navigate","params":{"url":"about:blank"}}`
	for _, peer := range []string{"cdp:1", "mcp:1", "jrpc:1"} {
		t.Run(peer, func(t *testing.T) {
			got := termText(t, s, map[string]any{
				"cmd": "call s:1 " + peer + " $FRAME",
				"args": map[string]any{"FRAME": map[string]any{"method": "Page.navigate",
					"params": map[string]any{"url": "about:blank"}}},
				"dry_run": true,
			})
			// The object went in as a Go value and comes out as JSON text: sorted keys,
			// no whitespace. That is what a peer parses, and it is the whole point.
			if !strings.Contains(got, want) {
				t.Fatalf("the object form did not expand to its JSON text:\n%s", got)
			}
		})
	}
}

func TestArgsScalarsKeepTheirJSONSpelling(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"number", float64(3), "run s:1 3"},
		{"bool", true, "run s:1 true"},
		{"array", []any{float64(1), "two"}, `run s:1 [1,"two"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, map[string]any{
				"cmd":     "run s:1 $V",
				"args":    map[string]any{"V": tc.in},
				"dry_run": true,
			})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("dry-run of %v did not expand to %q:\n%s", tc.in, tc.want, got)
			}
		})
	}

	// A STRING IS STILL NOT QUOTED OR ESCAPED. That is the pre-existing contract for the
	// list form, and the object form must not quietly change it.
	got := termText(t, s, map[string]any{
		"cmd":     "run s:1 $V",
		"args":    map[string]any{"V": `two words ORD""ER`},
		"dry_run": true,
	})
	if !strings.Contains(got, `run s:1 two words ORD""ER`) {
		t.Fatalf("a string value was rewritten on its way in:\n%s", got)
	}
}

func TestArgsObjectRefusesAPositionalLookingKey(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{
		"cmd":  "run s:1 $1",
		"args": map[string]any{"1": "echo hi"},
	})
	if !res.IsError {
		t.Fatalf("a digit-only key was accepted: %s", toolResultText(res))
	}
	text := toolResultText(res)
	if !strings.Contains(text, "looks positional") || !strings.Contains(text, "args: [") {
		t.Fatalf("the refusal does not say which spelling to use instead:\n%s", text)
	}
}

func TestArgsObjectRefusesANameTwoTablesDefine(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{
		"cmd":  "run s:1 $V",
		"args": map[string]any{"V": "one"},
		"vars": map[string]any{"V": "two"},
	})
	if !res.IsError {
		t.Fatalf("one name in two tables was resolved silently: %s", toolResultText(res))
	}
	text := toolResultText(res)
	if !strings.Contains(text, "both args and vars/secrets") {
		t.Fatalf("the refusal does not name the two tables:\n%s", text)
	}
}

// A BARE STRING IS STILL REFUSED, so `args: "pwd"` cannot silently mean `args: ["pwd"]`.
func TestArgsBareStringIsStillRefused(t *testing.T) {
	s := newTestServer(t)
	res := termCall(t, s, map[string]any{"cmd": "run s:1 $1", "args": "echo hi"})
	if !res.IsError || !strings.Contains(toolResultText(res), "args must be a list of values") {
		t.Fatalf("a bare string arg was not refused with both spellings:\n%s", toolResultText(res))
	}
}
