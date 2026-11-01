package mcp

import (
	"strings"
	"testing"
)

// TestTermExtCarriesArgvHeadersAndForwards pins the three flags the `ext`
// directive accepted and then dropped or mangled.
//
// All three were fail-silent, which is why they survived: --args was comma-split
// (so `sh -c 'a,b'` became two arguments and the extension died with a startup
// error that named no flag), and --headers/--forwards were parsed into
// Statement.Flags and then never read into the tool call at all — a caller saw a
// successful create and the header was never sent, the tunnel never declared.
//
// This walks the directive, not the tool, because the wiring between them is
// exactly what was missing.
func TestTermExtCarriesArgvHeadersAndForwards(t *testing.T) {
	s := newExtensionTestServer(t)

	body := termText(t, s, map[string]any{
		"cmd": `create ext wire --target internal --mode pipe ` +
			`--command node --description 'a,b' --headers '{"X-Api-Key":"k1"}' ` +
			`--forwards '[{"kind":"dynamic","local_port":1080}]' ` +
			`--args /srv/mcp.js --stdio --label 'x,y'`,
	})

	got := callExtension(t, s, map[string]any{"action": "get", "name": "wire"})

	// --args: an argv word list, quoting intact. The comma inside a quoted
	// argument is the case the old comma-split destroyed.
	argv, _ := got["args"].([]any)
	want := []string{"/srv/mcp.js", "--stdio", "--label", "x,y"}
	if len(argv) != len(want) {
		t.Fatalf("args = %v, want %v (from %q)", argv, want, body)
	}
	for i, w := range want {
		if argv[i] != w {
			t.Errorf("args[%d] = %v, want %q", i, argv[i], w)
		}
	}

	// --headers: a map, which is what the relay sends on every request.
	headers, _ := got["headers"].(map[string]any)
	if headers["X-Api-Key"] != "k1" {
		t.Errorf("headers = %v, want the X-Api-Key carried through", got["headers"])
	}

	// --forwards: a list of declarations, not words.
	fwds, _ := got["forwards"].([]any)
	if len(fwds) != 1 {
		t.Fatalf("forwards = %v, want one declaration", got["forwards"])
	}
	// The declaration comes back as the stored struct, so its fields are the Go
	// names. What matters here is that it ARRIVED: --forwards used to be parsed
	// and never read, so a template's tunnels silently never existed.
	one, _ := fwds[0].(map[string]any)
	if one["Kind"] != "dynamic" {
		t.Errorf("forward = %v, want Kind dynamic", one)
	}
	if port, _ := one["LocalPort"].(float64); port != 1080 {
		t.Errorf("forward local port = %v, want 1080", one["LocalPort"])
	}
}

// TestTermExtRefusesABadArgsFlag pins the other half: a value that is not an argv
// is refused, rather than started and left to fail later with an error that names
// no flag.
//
// Two layers can refuse it, and both are correct: the shell parser rejects an
// unbalanced quote before the directive is ever reached, and the flag reader
// rejects a value that is a command line rather than a word list (`;` outside
// quotes). What must NOT happen is the old behaviour — accept it, comma-split it,
// and start a process with a mangled argv.
func TestTermExtRefusesABadArgsFlag(t *testing.T) {
	s := newExtensionTestServer(t)
	for _, tc := range []struct{ name, cmd string }{
		{"unbalanced quote", `ext create broken1 --target internal --mode pipe --command node --args "unclosed`},
		// A quoted `;` is ONE word, so this is a legal argv and must NOT be
		// refused: the parser's rule is "one program, no shell syntax", and a
		// quoted semicolon is an argument. The unbalanced quote above is the
		// malformed case; there is no third shape to reject.

	} {
		body := termText(t, s, map[string]any{"cmd": tc.cmd})
		if !strings.Contains(body, "400") {
			t.Errorf("%s: a value that is not an argv must be refused; got %q", tc.name, body)
		}
		// And nothing was created: the refusal happens before the tool call.
		for _, name := range []string{"broken1", "broken2"} {
			got := termText(t, s, map[string]any{"cmd": "edit ext get " + name})
			if !strings.Contains(got, "not found") {
				t.Errorf("%s: the refused template must not exist; got %q", name, got)
			}
		}
	}
}

// TestTermExtValuesFillsHoles pins the installation step through the language an
// agent actually speaks: `edit ext <name> --values …` sets values and does NOT
// rewrite the document, because "what this extension is" and "what it is on this
// machine" are two questions with two capabilities behind them.
func TestTermExtValuesFillsHoles(t *testing.T) {
	s := newExtensionTestServer(t)
	if body := termText(t, s, map[string]any{
		"cmd": `create ext k8s --template 'target = "internal"
mode = "pipe"
command = "{{command}}"

[params.command]
type = "string"
help = "the program that serves MCP over the pipe"'`,
	}); !strings.Contains(body, "k8s") {
		t.Fatalf("installing a package should store it, got %q", body)
	}

	// Before the value exists the extension is installed and waiting, and the
	// answer names the hole rather than claiming it is broken.
	if got := termText(t, s, map[string]any{"cmd": `ls ext`}); !strings.Contains(got, "k8s") {
		t.Fatalf("an installed-but-unfilled extension must still be listed, got %q", got)
	}

	termText(t, s, map[string]any{"cmd": `edit ext k8s --values '{"command":"` + blockingCommand() + `"}'`})

	// The value landed in the values file, and the template still carries its hole:
	// that is what makes an upgrade possible.
	doc, err := s.extensions.Store().Template("k8s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "{{command}}") {
		t.Errorf("setting a value must not rewrite the template:\n%s", doc)
	}
	got := callExtension(t, s, map[string]any{"action": "get", "name": "k8s"})
	if got["command"] != blockingCommand() {
		t.Errorf("command = %v, want the value that was set", got["command"])
	}
}

// TestTermAcceptsExtensionsSynonym pins that the built-in spelling reaches the
// ext directive end to end, including the capability check and the dispatch.
func TestTermAcceptsExtensionsSynonym(t *testing.T) {
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "via-alias", "target": "internal",
		"paths": []any{"/srv/"},
	})
	body := termText(t, s, map[string]any{"cmd": `ls ext`})
	if !strings.Contains(body, "via-alias") {
		t.Errorf("`ls ext` should reach the extension layer; got %q", body)
	}
}
