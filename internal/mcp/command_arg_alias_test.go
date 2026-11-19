package mcp

import (
	"strings"
	"testing"
)

func TestTermCommandArgumentAlias(t *testing.T) {
	s := newTestServer(t)
	args := map[string]any{"command": "help close"}
	got := termText(t, s, args)
	want := termText(t, s, map[string]any{"cmd": "help close"})
	const hint = "Argument-Alias: command -> cmd; use cmd next time\n"
	if !strings.Contains(got, hint) || strings.Replace(got, hint, "", 1) != want {
		t.Fatalf("alias must return the canonical result with one hint:\n%s", got)
	}
	if _, ok := args["cmd"]; ok || args["command"] != "help close" {
		t.Fatal("alias normalization changed the caller's arguments")
	}
}

func TestTermCommandArgumentConflict(t *testing.T) {
	s := newTestServer(t)
	for _, cmd := range []any{nil, "", "help close"} {
		got := termText(t, s, map[string]any{"cmd": cmd, "command": "help close"})
		if !strings.Contains(got, "400 Bad Request") || !strings.Contains(got, "both cmd and command") {
			t.Fatalf("cmd=%v: want explicit conflict, got %s", cmd, got)
		}
	}
}

func TestTermCommandArgumentValidation(t *testing.T) {
	s := newTestServer(t)
	got := termText(t, s, map[string]any{"command": 123})
	if !strings.Contains(got, "400 Bad Request") || !strings.Contains(got, "cmd must be a string") || !strings.Contains(got, "Argument-Alias:") {
		t.Fatalf("alias must retain cmd validation and identify the alias: %s", got)
	}
	got = termText(t, s, map[string]any{"script": "help close"})
	if !strings.Contains(got, "unknown argument script") {
		t.Fatalf("other spellings must remain unknown: %s", got)
	}
}
