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
// so a rule that lived only in the schema validator would cover one of them.

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
		{"overflow typo", map[string]any{"on_bytes_overflow": "trunc"}, "must be one of truncate, fold"},
		{"overflow wrong type", map[string]any{"on_lines_overflow": 1}, "must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"cmd": "ls"}
			for k, v := range tc.args {
				args[k] = v
			}
			got := s.Term(context.Background(), "ls", args).Text()
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
//
// ONLY TWO KINDS OF BOUND ARE LEFT, and both are about the call working at all rather than
// about how much it costs: a negative number is a typo (there is no "-1 seconds"), and a
// window narrower than one character can never return one, so it withholds the same bytes
// on every call and advances nothing. Every CEILING was removed on purpose — time and space
// are what the caller pays for, and this server has no basis to overrule the only party that
// can judge the trade. Those numbers are published as sizes to stay under
// (TestTheSchemaSaysWhichWindowSizesAreRecommended) and honoured above them
// (TestALargeWindowIsHonouredNotRefused).
func TestWindowArgumentBoundsAreRefused(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"timeout": -1}, "timeout must be >= 0"},
		{map[string]any{"bytes_limit": -5}, "bytes_limit must be >= 0"},
		{map[string]any{"lines_limit": -5}, "lines_limit must be >= 0"},
		{map[string]any{"total_bytes_limit": 0}, "total_bytes_limit must be >= 1"},
		// The floor, not a ceiling: a window too small to hold one character withholds the
		// same bytes on every call and advances nothing.
		{map[string]any{"bytes_limit": 2}, "smaller than one character"},
	}
	for _, tc := range cases {
		args := map[string]any{"cmd": "ls"}
		for k, v := range tc.args {
			args[k] = v
		}
		got := s.Term(context.Background(), "ls", args).Text()
		if !strings.Contains(got, "400") || !strings.Contains(got, tc.want) {
			t.Errorf("%v must be refused with %q:\n%s", tc.args, tc.want, got)
		}
	}
}

// TestALargeWindowIsHonouredNotRefused is the other half of the rule, for time.
//
// A wait above the recommendation used to be a 400. The reasoning was sound as far as it
// went — the transport in front of the caller gives up sooner, so a longer window loses the
// reply — but it is the CALLER that loses the reply, and it is the only party that knows
// which client it is talking through. Refusing made its vocabulary depend on a timeout
// somewhere it cannot see. The number is a recommendation now, and the reply reports how
// long it actually waited.
func TestALargeWindowIsHonouredNotRefused(t *testing.T) {
	s := newTestServer(t)
	got := s.Term(context.Background(), "ls", map[string]any{"cmd": "ls", "timeout": 99999}).Text()
	if strings.Contains(got, "400") || strings.Contains(got, "timeout must be") {
		t.Fatalf("a long wait is the caller's own risk to take:\n%s", got)
	}
}

// TestTheSchemaSaysWhichWindowSizesAreRecommended is where the hint lives.
//
// A size that is not enforced has to be stated, or it is not a hint — it is a number in a
// comment. The caller reads the argument description before choosing, so the recommendation
// belongs there, in the words that say what it is for.
func TestTheSchemaSaysWhichWindowSizesAreRecommended(t *testing.T) {
	for name, want := range map[string]string{
		"bytes_limit":       "stay under",
		"lines_limit":       "stay under",
		"total_bytes_limit": "stay under",
	} {
		decl, ok := termArgDeclFor(name)
		if !ok {
			t.Fatalf("no declaration for %s", name)
		}
		if !strings.Contains(decl.DocNote, want) {
			t.Errorf("%s does not say it is a size to %q: %s", name, want, decl.DocNote)
		}
	}
	// And the text the model actually reads is rendered from the same declaration, so a
	// recommendation that lived only in this struct would never reach a caller.
	var rendered strings.Builder
	for _, d := range termArgDecls {
		rendered.WriteString(d.Desc)
		rendered.WriteString(d.DocNote)
	}
	if !strings.Contains(rendered.String(), "stay under") {
		t.Error("the rendered schema text does not carry the recommendation")
	}
}

// An ABSENT argument still takes its declared default. This is the other half of
// the contract: refusing typos must not turn into demanding every parameter.
func TestAbsentWindowArgumentsStillDefault(t *testing.T) {
	s := newTestServer(t)
	for _, args := range []map[string]any{
		{"cmd": "ls"},
		{"cmd": "ls", "timeout": nil},
		{"cmd": "ls", "on_bytes_overflow": ""},
	} {
		got := s.Term(context.Background(), "ls", args).Text()
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
	tool, ok := s.mcpServer.Tool("term")
	if !ok {
		t.Fatal("the term tool is not registered")
	}
	props := tool.InputSchema.Properties
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
		// NO MAXIMUM, AND THE SCHEMA MUST NOT INVENT ONE. Every upper bound in this table
		// was removed on purpose: the caller pays for the time and the context, and a
		// published ceiling would make a client refuse a value this server honours.
		if raw, present := prop["maximum"]; present {
			t.Errorf("%s: the schema publishes a maximum (%v) that no longer exists", name, raw)
		}
	}
}
