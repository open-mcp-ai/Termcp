package mcp

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestReaderDirectiveRegistersAndConsumesIndependently pins #286's second half:
// `reader` returned an id that nothing on this surface could read.
//
// A reader is a SECOND cursor on the same channel, so the two assertions that
// matter are that the id reads the bytes AND that it does not steal them from
// the default cursor.
func TestReaderDirectiveRegistersAndConsumesIndependently(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	reg := termText(t, s, map[string]any{"cmd": "reader " + h})
	if !strings.HasPrefix(reg, "TERM/1 200") {
		t.Fatalf("reader registration failed:\n%s", reg)
	}
	body := reg
	if i := strings.Index(reg, "\n\n"); i >= 0 {
		body = reg[i+2:]
	}
	m := regexp.MustCompile(`reader\s+(\d+)`).FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("could not extract a reader id from the reply:\n%s", reg)
	}
	readerID := m[1]

	const marker = "reader-check-123"
	// `run` types and presses enter; on a ready shell it produces the marker.
	reply := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"echo " + marker},
		"timeout": 5,
	})
	if !strings.Contains(reply, marker) {
		t.Fatalf("the run never produced the marker:\n%s", reply)
	}

	// THE REGISTERED READER SEES THE BYTES. Polled, bounded by deadline.
	deadline := time.Now().Add(10 * time.Second)
	seen := ""
	for {
		seen = termText(t, s, map[string]any{"cmd": "read " + h + " --reader " + readerID, "timeout": 1})
		if strings.Contains(seen, marker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("`read --reader %s` never saw the run's bytes:\n%s", readerID, seen)
		}
	}

	if drop := termText(t, s, map[string]any{"cmd": "reader " + h + " --drop " + readerID}); !strings.HasPrefix(drop, "TERM/1 204") {
		t.Fatalf("reader unregister failed:\n%s", drop)
	}

	// A DROPPED ID MUST NOT RESOLVE TO THE DEFAULT CURSOR: falling back would hand
	// the caller somebody else's position while looking like a successful read.
	afterDrop := termText(t, s, map[string]any{"cmd": "read " + h + " --reader " + readerID})
	if !strings.Contains(afterDrop, "reader_not_registered") {
		t.Errorf("a dropped reader must not resolve to the default cursor:\n%s", afterDrop)
	}
}
