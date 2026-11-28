package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// `args` HAS TWO SHAPES, AND JSON IS WHAT TELLS THEM APART (one name, two call
// shapes — `foo(a, b)` and `foo(a: 1, b: 2)` share one method name). An array is the
// positional values for $1…; an object is the arguments of the tool a `call <peer>
// <tool>` line invokes, and it fills that slot only when the line leaves it empty.
//
// The refusals are the point of the group: a second source, a line that calls no
// tool, and a type that is neither shape are all answered with a 400 that names the
// rule, because an object `args` that nothing reads does not fail on its own — the
// reply is a 200 with the caller's parameters gone.

func argsObjectCall(t *testing.T, value any) termArgs {
	t.Helper()
	args := map[string]any{"cmd": "call s1 mcp:1 term", "args": value}
	win, derr := parseTermArgs(args)
	if derr != nil {
		t.Fatalf("parseTermArgs refused %v: %s", value, derr.Msg)
	}
	return win
}

func TestArgsObjectCarriesTypesToTheTool(t *testing.T) {
	// NUMBERS AND BOOLEANS KEEP THEIR TYPE. This is the reason the object form exists
	// at all: `vars`/`secrets` are string→string, so `{"timeout": 3}` through them
	// would reach the tool as `"3"`.
	win := argsObjectCall(t, map[string]any{"cmd": "ls", "timeout": 3, "deep": map[string]any{"a": true}})
	if len(win.Args) != 0 {
		t.Fatalf("an object args must not become positional values; got %v", win.Args)
	}
	var sent map[string]any
	if err := json.Unmarshal(win.ArgsObject, &sent); err != nil {
		t.Fatalf("the object args is not JSON: %v (%s)", err, win.ArgsObject)
	}
	if sent["timeout"] != float64(3) {
		t.Errorf("timeout reached the tool as %T(%v), want the number 3", sent["timeout"], sent["timeout"])
	}
	if _, isString := sent["cmd"].(string); !isString {
		t.Errorf("cmd reached the tool as %T, want a string", sent["cmd"])
	}
}

func TestArgsObjectOnALineThatCallsNoToolIsRefused(t *testing.T) {
	// THE SILENT FAILURE THIS PREVENTS: the object fills no $1 and reaches no tool, so
	// every substitution table is empty and the line runs with nothing the caller sent
	// — a 200 with no way to notice.
	for _, cmd := range []string{"run s1 'ls'", "call s1 mcp:1 '{\"jsonrpc\":\"2.0\"}'", "lss"} {
		args := map[string]any{"cmd": cmd, "args": map[string]any{"name": "x"}}
		win, derr := parseTermArgs(args)
		if derr != nil {
			t.Fatalf("%s: parseTermArgs refused: %s", cmd, derr.Msg)
		}
		file, err := directive.ParseWithSubst(cmd, directive.Subst{Named: win.Vars, Pos: win.Args})
		if err != nil {
			continue // an invented directive is refused by the parser, which is also a refusal
		}
		slot, bad := objectArgsSlot(file.Statements, win)
		if !bad {
			t.Errorf("%q with an object args ran with nothing reading it; want a 400", cmd)
			continue
		}
		if slot.Code != 400 || !strings.Contains(slot.Body, "nothing would read it") {
			t.Errorf("%q: got %d %q, want a 400 naming the rule", cmd, slot.Code, slot.Body)
		}
	}
}

func TestArgsObjectShapeCheckLetsAToolCallThrough(t *testing.T) {
	// A LINE THAT SPELLS ARGUMENTS TWICE STILL CALLS A TOOL. The shape check must hand it
	// to mcpToolCall, whose refusal names the conflict; refusing it here as "no tool
	// call" would misdescribe the line and send the caller to the wrong fix.
	for _, payload := range []string{"term", `term {"cmd": "ls"}`, "term", "search"} {
		win := argsObjectCall(t, map[string]any{"cmd": "ls"})
		st := directive.Statement{Op: "call", Target: "s1", Kind: "mcp", Payload: payload}
		if _, bad := objectArgsSlot([]directive.Statement{st}, win); bad {
			t.Errorf("payload %q was refused by the shape check; it is a tool call", payload)
		}
	}
}

func TestArgsObjectAndWrittenArgumentsAreOneSource(t *testing.T) {
	// THE LINE SPELLS ITS ARGUMENTS TWICE. Preferring the object would drop the words
	// the caller wrote; preferring the words would drop the object — either way one of
	// the caller's two answers vanishes and the reply still says 200.
	win := argsObjectCall(t, map[string]any{"cmd": "ls"})
	stmt := directive.Statement{Op: "call", Target: "s1", Kind: "mcp", Payload: `term {"cmd": "ls"}`}
	_, bad := objectArgsPayload(stmt, `term {"cmd": "ls"}`, win)
	if bad == nil {
		t.Fatal("two sources were accepted; want a 400")
	}
	if bad.Code != 400 || !strings.Contains(bad.Body, "twice") {
		t.Errorf("got %d %q, want a 400 naming the conflict", bad.Code, bad.Body)
	}
}

func TestArgsObjectFillsOnlyTheEmptySlot(t *testing.T) {
	// THE INJECTION ITSELF: the object is appended to a payload that is just the tool
	// name, and one reader (splitToolCall) then hands the tool its arguments — so the
	// two spellings of a tool call cannot drift apart.
	win := argsObjectCall(t, map[string]any{"cmd": "pwd"})
	stmt := directive.Statement{Op: "call", Payload: "term"}
	payload, bad := objectArgsPayload(stmt, "term", win)
	if bad != nil {
		t.Fatalf("the injection was refused: %d %s", bad.Code, bad.Body)
	}
	name, args, split := splitToolCall(stmt, payload)
	if split != nil {
		t.Fatalf("splitToolCall refused the injected payload: %d %s", split.Code, split.Body)
	}
	if name != "term" {
		t.Errorf("tool name = %q, want term", name)
	}
	if args["cmd"] != "pwd" {
		t.Errorf("the tool did not receive the object's cmd: %v", args)
	}
}

func TestArgsObjectNeedsExactlyOneToolCall(t *testing.T) {
	// ONE OBJECT, ONE CALL. A line with a shell statement AND a tool call is ordinary
	// (`ls; call s mcp:1 term`) and the object belongs to the call — so the question is
	// how many tool calls there are, not whether every statement is one.
	win := argsObjectCall(t, map[string]any{"cmd": "ls"})
	mixed := []directive.Statement{
		{Op: "run", Payload: "ls"},
		{Op: "call", Target: "s1", Kind: "mcp", Payload: "term"},
	}
	if _, bad := objectArgsSlot(mixed, win); bad {
		t.Error("a shell statement beside one tool call was refused; the object has exactly one reader")
	}
	two := []directive.Statement{
		{Op: "call", Target: "s1", Kind: "mcp", Payload: "term"},
		{Op: "call", Target: "s1", Kind: "mcp", Payload: "search"},
	}
	slot, bad := objectArgsSlot(two, win)
	if !bad {
		t.Fatal("two tool calls with one object args were accepted; which call it belongs to is unstated")
	}
	if !strings.Contains(slot.Body, "2 tool calls") {
		t.Errorf("the refusal does not say how many calls it found: %q", slot.Body)
	}
}

func TestArgsObjectRefusalIsAVerdictInADryRun(t *testing.T) {
	// A DRY RUN ANSWERS "IS THIS LINE VALID", and an object args on a line that calls no
	// tool makes it invalid. The two headers are the answer; a bare 400 would leave a
	// harness unable to tell this from a line nobody checked (#241).
	win := argsObjectCall(t, map[string]any{"cmd": "ls"})
	win.DryRun = true
	stmts := []directive.Statement{{Op: "run", Payload: "ls"}}
	slot, bad := objectArgsSlot(stmts, win)
	if !bad {
		t.Fatal("the dry run accepted a misplaced object args")
	}
	if slot.Headers["Dry-Run"] != "true" || slot.Headers["Verdict"] != "invalid" {
		t.Errorf("dry-run headers missing: %v", slot.Headers)
	}
}

func TestArgsArrayElementsMayBeObjects(t *testing.T) {
	// ONE ARGUMENT SET PER CALL. An object serves exactly one tool call, so a line that
	// calls twice takes an array — and an object element keeps the no-escaping property
	// on the second call instead of forcing each set back through a JSON string.
	pos, obj, derr := readArgsArg(map[string]any{"args": []any{
		map[string]any{"cmd": "ls"},
		`{"cmd":"help"}`,
	}})
	if derr != nil {
		t.Fatalf("an array of objects was refused: %s", derr.Msg)
	}
	if obj != nil {
		t.Error("an array must not fill the single-call object slot")
	}
	if len(pos) != 2 {
		t.Fatalf("got %d positional values, want 2", len(pos))
	}
	if pos[0] != `{"cmd":"ls"}` {
		t.Errorf("the object element serialised to %q, want the JSON text of the object", pos[0])
	}
	if pos[1] != `{"cmd":"help"}` {
		t.Errorf("a string element must pass through unchanged; got %q", pos[1])
	}
	// NUMBERS AND NULL STAY REFUSED: 3 has no single spelling as a value in a shell
	// line, and guessing one would put bytes in the command the caller never sent.
	for _, bad := range []any{float64(3), nil, true} {
		if _, _, derr := readArgsArg(map[string]any{"args": []any{bad}}); derr == nil {
			t.Errorf("an element of type %T was accepted; want a refusal naming strings and objects", bad)
		}
	}
}

func TestArgsOfNeitherShapeIsRefusedNamingBoth(t *testing.T) {
	for _, bad := range []any{"pwd", float64(3), true} {
		_, _, derr := readArgsArg(map[string]any{"args": bad})
		if derr == nil {
			t.Errorf("args: %v was accepted; want a refusal naming both shapes", bad)
			continue
		}
		if !strings.Contains(derr.Msg, "array") || !strings.Contains(derr.Msg, "object") {
			t.Errorf("the refusal does not name both shapes: %s", derr.Msg)
		}
	}
}
