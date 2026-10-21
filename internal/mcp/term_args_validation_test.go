package mcp

import (
	"context"
	"strings"
	"testing"
)

// The window parameters are the one place term() has real typed arguments, and
// they were the place a typo cost the least visible thing: `timeout: "abc"` was
// read by a loose getter that could not tell "absent" from "present but wrong", so
// it became 2 seconds with a 200 reply. The caller's typo silently bought a
// different window, and nothing in the reply said so.
//
// These tests pin the split that the argument table now encodes:
//
//	absent        -> the declared default, silently (that default is documented in
//	                 the schema and in docs/api.md, so it is not a surprise)
//	present+wrong -> 400 naming the argument and what it accepts
//
// The checks run in parseTermArgs, which is the ONE path both entrances share —
// the MCP transport goes through handleTerm and POST /api/term calls Term directly,
// so a rule that lived only in mcp-go's schema validator would cover one of them.

// A mistyped window value is refused, naming the argument and what it wants.
func TestWindowArgumentTypesAreRefused(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		args map[string]any
		want string // a substring the message must contain
	}{
		{"timeout string", map[string]any{"timeout": "abc"}, "timeout must be a number"},
		{"timeout bool", map[string]any{"timeout": true}, "timeout must be a number"},
		{"bytes_limit string", map[string]any{"bytes_limit": "many"}, "bytes_limit must be a number"},
		{"lines_limit array", map[string]any{"lines_limit": []any{1}}, "lines_limit must be a number"},
		{"learn_mode string", map[string]any{"learn_mode": "yes"}, "learn_mode must be true or false"},
		{"pipe_to number", map[string]any{"pipe_to": 3}, "pipe_to must be a string"},
		{"overflow typo", map[string]any{"on_bytes_overflow": "trunc"}, "must be one of truncate, fold"},
		{"overflow wrong type", map[string]any{"on_time_overflow": 1}, "must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"cmd": "ps"}
			for k, v := range tc.args {
				args[k] = v
			}
			got := s.Term(context.Background(), "ps", args).Text()
			if !strings.Contains(got, "400") {
				t.Fatalf("a mistyped window value must be a 400:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("the error must say %q:\n%s", tc.want, got)
			}
		})
	}
}

// Out-of-range numbers are refused with the bound, not clamped.
func TestWindowArgumentBoundsAreRefused(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"timeout": -1}, "timeout must be >= 0"},
		{map[string]any{"timeout": 99999}, "timeout must be <= 3600"},
		{map[string]any{"bytes_limit": -5}, "bytes_limit must be >= 0"},
		{map[string]any{"lines_limit": -5}, "lines_limit must be >= 0"},
		{map[string]any{"total_bytes_limit": 0}, "total_bytes_limit must be >= 1"},
	}
	for _, tc := range cases {
		args := map[string]any{"cmd": "ps"}
		for k, v := range tc.args {
			args[k] = v
		}
		got := s.Term(context.Background(), "ps", args).Text()
		if !strings.Contains(got, "400") || !strings.Contains(got, tc.want) {
			t.Errorf("%v must be refused with %q:\n%s", tc.args, tc.want, got)
		}
	}
}

// An ABSENT argument still takes its declared default. This is the other half of
// the contract: refusing typos must not turn into demanding every parameter.
func TestAbsentWindowArgumentsStillDefault(t *testing.T) {
	s := newTestServer(t)
	for _, args := range []map[string]any{
		{"cmd": "ps"},
		{"cmd": "ps", "timeout": nil},
		{"cmd": "ps", "on_bytes_overflow": ""},
	} {
		got := s.Term(context.Background(), "ps", args).Text()
		// The status line, not the whole reply: a body can contain "400" by
		// coincidence (a byte count, a session id), which would make this assertion
		// fail for the wrong reason.
		if strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("an absent or empty window argument must take its default:\n%s", got)
		}
	}
}

// The schema publishes the same bounds the parser enforces, from one declaration.
// If these drift, a client validates against a rule the server does not apply.
func TestSchemaPublishesTheDeclaredBounds(t *testing.T) {
	s := newTestServer(t)
	tool, ok := s.mcpServer.ListTools()["term"]
	if !ok {
		t.Fatal("the term tool is not registered")
	}
	props := tool.Tool.InputSchema.Properties
	for _, name := range []string{"timeout", "bytes_limit", "lines_limit", "total_bytes_limit"} {
		raw, ok := props[name]
		if !ok {
			t.Fatalf("the schema does not publish %q", name)
		}
		prop, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%q is not an object schema: %T", name, raw)
		}
		d, ok := termArgDeclFor(name)
		if !ok {
			t.Fatalf("no declaration for %q", name)
		}
		if v, want := boundValue(d.Min); want {
			got, _ := prop["minimum"].(float64)
			if got != v {
				t.Errorf("%s: schema minimum = %v, declaration says %v", name, prop["minimum"], v)
			}
		}
		if v, want := boundValue(d.Max); want {
			got, _ := prop["maximum"].(float64)
			if got != v {
				t.Errorf("%s: schema maximum = %v, declaration says %v", name, prop["maximum"], v)
			}
		}
	}
}
