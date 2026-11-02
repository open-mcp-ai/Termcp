package extensions

import (
	"strings"
	"testing"
)

// An MCP extension is a program plus its argv. There is nothing conventional to
// guess, so the preset supplies neither and the caller fills both in.
//
// The default it used to supply was wrong in both directions, and each direction
// is a separate failure worth pinning.
func TestMcpCreateRequiresTheProgram(t *testing.T) {
	// DIRECTION ONE: naming a program must not keep the default's arguments.
	// `--command cat` ran `cat -y example-mcp-server` and died with "cat: illegal
	// option -- y", which names neither the flag nor the default that supplied it.
	ext := Extension{Mode: "pipe"}
	if err := ApplyArgs(&ext, map[string]any{"command": "cat"}, false); err != nil {
		t.Fatalf("ApplyArgs: %v", err)
	}
	if ext.Command != "cat" {
		t.Errorf("Command = %q, want cat", ext.Command)
	}
	if len(ext.Args) != 0 {
		t.Errorf("an author's command must not inherit the preset's args, got %v", ext.Args)
	}

	// DIRECTION TWO: naming nothing is refused at create time. The old preset
	// produced a template that validated and could never run, which is worse than
	// an error: it looks configured.
	if err := ApplyArgs(&ext, map[string]any{}, false); err != nil {
		t.Fatalf("ApplyArgs: %v", err)
	}
	if ext.Command != "cat" {
		t.Errorf("an empty create should keep the preset's (absent) command, got %q", ext.Command)
	}

	bare := Extension{Mode: "pipe"}
	bare.Name, bare.Target = "probe", "internal"
	err := bare.Validate()
	if err == nil {
		t.Fatal("an mcp template with no command must not validate")
	}
	if !strings.Contains(err.Error(), "requires a command") {
		t.Errorf("the refusal must say a command is required, got %q", err)
	}
}

// The argv the author DOES give is carried through untouched, including a flag
// that looks like the one the preset used to add.
func TestMcpCreateCarriesTheAuthorsArgv(t *testing.T) {
	ext := Extension{Mode: "pipe"}
	err := ApplyArgs(&ext, map[string]any{
		"command": "npx", "args": []any{"-y", "@scope/server"},
	}, false)
	if err != nil {
		t.Fatalf("ApplyArgs: %v", err)
	}
	if ext.Command != "npx" || len(ext.Args) != 2 || ext.Args[0] != "-y" || ext.Args[1] != "@scope/server" {
		t.Errorf("argv = %q %v, want npx [-y @scope/server]", ext.Command, ext.Args)
	}
}
