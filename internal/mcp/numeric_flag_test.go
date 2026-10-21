package mcp

import (
	"context"
	"strings"
	"testing"
)

// A numeric flag that does not parse is a 400, not a substitute value.
//
// The bug class: `flagNumber` returned 0 for anything it could not parse, and 0 is
// a legal value nearly everywhere it lands. So the typo flowed into a handler whose
// own default (24x80 for resize, the default window for read, the default grace for
// close) turned it into a DIFFERENT operation that succeeded. The reply said 200;
// the caller's mistake had been silently reinterpreted.
//
// The fix is at the layer that can still name the flag: the directive parses its own
// numeric flags before handing anything to a handler.

// Every numeric flag a directive reads refuses a non-number, naming the flag.
func TestNumericFlagsRefuseNonNumbers(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"resize rows", "resize " + h + " abc 120", "--rows"},
		{"resize cols", "resize " + h + " 40 xyz", "--cols"},
		{"read tail", "read " + h + " --tail abc", "--tail"},
		{"read cursor", "read " + h + " --cursor abc", "--cursor"},
		{"read from-line", "read " + h + " --from-line abc", "--from-line"},
		{"key repeat", "key " + h + " enter --repeat abc", "--repeat"},
		{"close grace", "close " + h + " --grace abc", "--grace"},
		{"notify for", "notify --for abc hi", "--for"},
		{"watch for", "watch " + h + " --event output --for abc", "--for"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, map[string]any{"cmd": tc.cmd})
			if !strings.Contains(got, "400") {
				t.Fatalf("a non-numeric value must be a 400:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("the error must name %q so the caller can find the word:\n%s", tc.want, got)
			}
			if !strings.Contains(got, "got") {
				t.Errorf("the error must show what was received:\n%s", got)
			}
		})
	}
}

// The non-number must not have been ACTED ON either: a refused statement produces
// no side effect. `resize abc` used to reach the PTY as a 24x80 resize.
func TestRefusedNumericFlagHasNoEffect(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	before := ptySize(t, s, h)
	if got := termText(t, s, map[string]any{"cmd": "resize " + h + " abc 999"}); !strings.Contains(got, "400") {
		t.Fatalf("resize abc must be refused:\n%s", got)
	}
	if after := ptySize(t, s, h); after != before {
		t.Errorf("a refused resize changed the size: %dx%d -> %dx%d", before.rows, before.cols, after.rows, after.cols)
	}
}

// A zero or negative size is refused too: it is not a terminal anyone can want, and
// passing it through made the handler's default perform a resize nobody asked for.
func TestResizeRefusesNonPositiveSizes(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, cmd := range []string{
		"resize " + h + " 0 0",
		"resize " + h + " -5 5",
		"resize " + h + " 5 -5",
	} {
		got := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.Contains(got, "400") || !strings.Contains(got, ">= 1") {
			t.Errorf("%q must be refused with the bound:\n%s", cmd, got)
		}
	}
	// A real size still works, and really takes effect.
	if got := termText(t, s, map[string]any{"cmd": "resize " + h + " 42 142"}); !strings.Contains(got, "204") {
		t.Fatalf("a valid resize must succeed:\n%s", got)
	}
	if size := ptySize(t, s, h); size.rows != 42 || size.cols != 142 {
		t.Errorf("after `resize %s 42 142` the size is %dx%d", h, size.rows, size.cols)
	}
}

// ABSENT is still fine: refusing typos must not become demanding every flag.
func TestAbsentNumericFlagsStillWork(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, cmd := range []string{
		"read " + h,
		"key " + h + " enter",
		"resize " + h + " 30 90",
	} {
		got := termText(t, s, map[string]any{"cmd": cmd})
		// Check the STATUS LINE, not the whole reply: a session id contains random
		// hex, so "400" can appear in the Path or the body by coincidence (and did,
		// making this assertion fail on a randomly generated id).
		if strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("%q must not be refused:\n%s", cmd, got)
		}
	}
}

// ptySize reads a channel's current dimensions from the manager, which is where
// ResizePty publishes them.
func ptySize(t *testing.T, s *Server, handle string) struct{ rows, cols int } {
	t.Helper()
	cs, bad := s.requireShell(handle)
	if bad != nil {
		t.Fatalf("requireShell(%s): %s", handle, toolResultText(bad))
	}
	info := cs.Info()
	return struct{ rows, cols int }{info.Rows, info.Cols}
}

// A directive that forwards MANY numeric flags refuses on the first bad one rather
// than filling the rest and running with a zero. `file` carries eight of them.
func TestManyNumericFlagsRefuseTheFirstBadOne(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd": "file " + h + " --action read --remote-path /tmp/x --offset abc --length 5",
	})
	if !strings.Contains(got, "400") || !strings.Contains(got, "--offset") {
		t.Fatalf("the first unparseable numeric flag must be refused by name:\n%s", got)
	}
	_ = context.Background()
}
