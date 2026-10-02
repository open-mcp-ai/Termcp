package mcp

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// shell_output is one tool with three read forms, and the split into
// parseReadParams / readOutputWindow put a seam exactly where the three diverge.
// These tests pin the two halves of that seam on their own: the argument
// contract (which is what decides the form) and the window arithmetic (which is
// what a caller pages with). The end-to-end behaviour is covered elsewhere;
// what is checked here is the part that has no server in it.

// TestParseReadParamsDefaults pins the defaults a caller who sends only a
// shell_id gets. They are part of the contract: tail_lines 0 with offset -1 is
// what selects the live cursor form, so changing any of these silently changes
// which of the three reads happens.
func TestParseReadParamsDefaults(t *testing.T) {
	p, bad := parseReadParams(map[string]any{})
	if bad != nil {
		t.Fatalf("empty arguments were rejected: %s", bad.Content[0].(mcpgo.TextContent).Text)
	}
	if !p.stripAnsi {
		t.Error("strip_ansi defaults to true: raw ANSI escapes are not what a caller wants by default")
	}
	if p.timeout != 3.0 {
		t.Errorf("timeout = %v, want 3", p.timeout)
	}
	if p.maxBytes != 8192 {
		t.Errorf("max_bytes = %d, want 8192", p.maxBytes)
	}
	if p.offset != -1 {
		t.Errorf("offset = %d, want -1: -1 is what means \"no positional read\"", p.offset)
	}
	if p.tailLines != 0 || p.maxLines != 0 || p.readerID != 0 {
		t.Errorf("zero values not preserved: %+v", p)
	}
}

// TestParseReadParamsValues pins that what the caller sends arrives intact.
// The tool takes numbers as JSON numbers, so every integer field is parsed
// through a float; a coercion bug here would show up as a wrong window rather
// than an error.
func TestParseReadParamsValues(t *testing.T) {
	p, bad := parseReadParams(map[string]any{
		"strip_ansi": false,
		"timeout":    float64(1.5),
		"max_lines":  float64(7),
		"max_bytes":  float64(1234),
		"reader_id":  float64(9),
		"offset":     float64(42),
		"tail_lines": float64(3),
	})
	if bad != nil {
		t.Fatalf("valid arguments were rejected: %s", bad.Content[0].(mcpgo.TextContent).Text)
	}
	want := readParams{stripAnsi: false, timeout: 1.5, maxLines: 7, maxBytes: 1234, readerID: 9, offset: 42, tailLines: 3}
	if p != want {
		t.Errorf("parsed %+v, want %+v", p, want)
	}
}

// TestParseReadParamsRanges pins the domain of each bounded field. These are the
// three documented ranges; the point of checking them here is that the check
// moved out of the handler, so a future edit to one of them must fail a test
// rather than quietly widen what the tool accepts.
func TestParseReadParamsRanges(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		ok   bool
	}{
		{"timeout 0 is the non-blocking read", map[string]any{"timeout": float64(0)}, true},
		{"timeout 60 is the ceiling", map[string]any{"timeout": float64(60)}, true},
		{"timeout below 0", map[string]any{"timeout": float64(-0.1)}, false},
		{"timeout above 60", map[string]any{"timeout": float64(60.1)}, false},
		{"tail_lines 0 is the default", map[string]any{"tail_lines": float64(0)}, true},
		{"tail_lines negative", map[string]any{"tail_lines": float64(-1)}, false},
		{"offset -1 means no positional read", map[string]any{"offset": float64(-1)}, true},
		{"offset below -1", map[string]any{"offset": float64(-2)}, false},
		{"offset 0 is a valid first byte", map[string]any{"offset": float64(0)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, bad := parseReadParams(c.args)
			if c.ok && bad != nil {
				t.Errorf("rejected a valid value: %s", bad.Content[0].(mcpgo.TextContent).Text)
			}
			if !c.ok && bad == nil {
				t.Error("accepted a value outside the documented range")
			}
		})
	}
}

// TestOutputWindowResultShape pins the JSON keys a caller pages with. Renaming
// or dropping one of these breaks every client that reads a stream in chunks,
// which is the normal way to consume output, so the shape is worth asserting
// rather than assuming.
func TestOutputWindowResultShape(t *testing.T) {
	src := &outputSource{sessID: "s1", shellID: "sh1"}
	w := outputWindow{output: "one\ntwo\n", start: 10, end: 18, total: 40, hasMore: true}

	got := w.result(src)

	for _, key := range []string{
		"output", "has_more", "lines_returned", "bytes_returned",
		"start_offset", "end_offset", "total_bytes", "source", "session_id", "shell_id", "session_status",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("result is missing %q", key)
		}
	}
	if got["lines_returned"] != 2 {
		t.Errorf("lines_returned = %v, want 2", got["lines_returned"])
	}
	if got["bytes_returned"] != len("one\ntwo\n") {
		t.Errorf("bytes_returned = %v, want %d", got["bytes_returned"], len("one\ntwo\n"))
	}
	if got["start_offset"] != int64(10) || got["end_offset"] != int64(18) || got["total_bytes"] != int64(40) {
		t.Errorf("offsets not carried through: %v/%v/%v", got["start_offset"], got["end_offset"], got["total_bytes"])
	}
	if got["has_more"] != true {
		t.Errorf("has_more = %v, want true", got["has_more"])
	}
	if got["source"] != SourcePersisted {
		t.Errorf("source = %v, want %q: a nil live shell is a persisted stream", got["source"], SourcePersisted)
	}
	if got["session_id"] != "s1" || got["shell_id"] != "sh1" {
		t.Errorf("stream identity lost: %v/%v", got["session_id"], got["shell_id"])
	}
	if _, ok := got["session_uptime_seconds"]; ok {
		t.Error("session_uptime_seconds must only appear for a live source")
	}
}

// TestOutputWindowResultLiveAddsUptime covers the one key that depends on the
// source rather than the window: uptime only means something while the shell is
// alive, so a persisted read must not claim it.
func TestOutputWindowResultLiveAddsUptime(t *testing.T) {
	live := &outputSource{live: &session.ChildShell{}, sessID: "s1", shellID: "sh1", created: 0}
	got := outputWindow{output: "x"}.result(live)
	if _, ok := got["session_uptime_seconds"]; !ok {
		t.Error("a live source must report session_uptime_seconds")
	}
	if got["source"] != SourceLive {
		t.Errorf("source = %v, want %q", got["source"], SourceLive)
	}
}

// TestOutputWindowResultEmptyHasZeroCounts pins the empty read, which is what a
// live cursor returns when nothing has been written yet. It must be a normal
// result with zero counts, not an error: a caller polling an idle shell would
// otherwise have to treat "nothing yet" as a failure.
func TestOutputWindowResultEmptyHasZeroCounts(t *testing.T) {
	got := outputWindow{}.result(&outputSource{sessID: "s", shellID: "sh"})

	if got["output"] != "" {
		t.Errorf("output = %q, want empty", got["output"])
	}
	if got["lines_returned"] != 0 || got["bytes_returned"] != 0 {
		t.Errorf("empty read reported %v lines / %v bytes", got["lines_returned"], got["bytes_returned"])
	}
	if got["has_more"] != false {
		t.Errorf("has_more = %v, want false", got["has_more"])
	}
}
