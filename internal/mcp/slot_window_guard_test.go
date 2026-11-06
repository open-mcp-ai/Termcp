package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE WINDOW IS THE TOOL'S CONTRACT, NOT EACH DIRECTIVE'S MEMORY (issue #209).
//
// `bytes_limit` is documented as an argument of `term`, so a caller has no reason to
// expect it to hold on one directive and not the next. It used to hold only where a
// handler remembered to call `generatedSlot`: `help <node>`, `ls task`, `cdp protocol`
// and a peer's `tools/list` returned the whole body however small a window was asked
// for — the same parameter meaning "a budget" in one reply and "not consulted" in the
// next.
//
// The fix is one guard on the way out (`applySlotWindows`), which is exactly what makes
// these assertions worth writing: each of these directives returns its own slot literal
// and never mentions a window, so each one goes red if the guard is removed. That is the
// anti-vacuity property the issue asked for — a test that cannot pass for the wrong
// reason.
func TestABareSlotObeysTheCallersWindow(t *testing.T) {
	s := newTestServer(t)

	for _, cmd := range []string{
		"help file",    // a node page: a slot literal built from the usage lines
		"help channel", // same shape, a different node
		"ls task",      // an empty collection, still generated
	} {
		t.Run(cmd, func(t *testing.T) {
			// A window small enough to cut every one of them, and large enough that the
			// cut is unambiguous rather than a rounding accident.
			cut := termText(t, s, map[string]any{"cmd": cmd, "bytes_limit": 60})
			if !strings.HasPrefix(cut, "TERM/1 206") {
				t.Fatalf("%q ignored bytes_limit=60:\n%s", cmd, firstLine(cut))
			}
			if !strings.Contains(cut, "Truncated:") {
				t.Errorf("%q was cut without saying what it withheld:\n%s", cmd, cut)
			}
			// The size has to travel: a generated reply has no cursor, so the number is
			// the only way to price the rest before buying it.
			if !strings.Contains(cut, "Content-Range: bytes */") {
				t.Errorf("%q was cut without reporting its full size:\n%s", cmd, cut)
			}
			// And asking for no bytes is the free form of the same question.
			zero := termText(t, s, map[string]any{"cmd": cmd, "bytes_limit": 0})
			if !strings.HasPrefix(zero, "TERM/1 204") {
				t.Errorf("%q ignored bytes_limit=0:\n%s", cmd, firstLine(zero))
			}
		})
	}
}

// THE GUARD CUTS CONTENT AND NOTHING ELSE.
//
// A receipt is an answer about a transfer, a diagnosis is an answer about a mistake, and
// a truncation already reports itself. Cutting any of them to the byte window produced
// replies that could not be acted on: a 202 whose body stops mid-sentence names no
// amount of anything. The guard therefore acts only on 200, which is what this pins.
func TestTheWindowGuardLeavesNonContentAlone(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	// A 202 receipt: the transfer's name and its bytes have to survive the window.
	dir := t.TempDir()
	src := filepath.Join(dir, "receipt.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	receipt := termText(t, s, map[string]any{
		"cmd":         "read " + sid + " file " + src + " | write " + sid + " file " + src + ".out",
		"bytes_limit": 40,
	})
	if !strings.HasPrefix(receipt, "TERM/1 202") {
		t.Fatalf("a pipeline receipt was cut to the window:\n%s", receipt)
	}
	if !strings.Contains(receipt, "Task:") {
		t.Errorf("the receipt lost the handle that makes it actionable:\n%s", receipt)
	}

	// A refusal: the sentence is what the caller acts on, so it is not the window's to cut.
	refusal := termText(t, s, map[string]any{
		"cmd":         "read " + h + " --since 1",
		"bytes_limit": 20,
	})
	if !strings.HasPrefix(refusal, "TERM/1 400") {
		t.Fatalf("a refusal was answered with a status the window chose:\n%s", refusal)
	}
	if !strings.Contains(refusal, "peer's frame log") {
		t.Errorf("the refusal was truncated before it named the right form:\n%s", refusal)
	}
}

// A PEER'S ANSWER IS CONTENT TOO, AND IT WAS THE BIGGEST ONE.
//
// `bytes_limit` held on the terminal's own listings and not on the two doors that
// return a peer's document: `get <s> mcp:1 tools` and `cdp protocol` answered with
// whatever the peer published — the case issue #209 is actually about, a 41 KB
// tools/list — while a window that small would have cut any local listing to size.
// Both paths return a slot literal and never name a window, so these two assertions
// are the regression: without the guard they answer 200 with the whole document.
func TestAPeersDocumentObeysTheCallersWindow(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		id := req["id"]
		switch req["method"] {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"protocolVersion": "2024-11-05",
					"serverInfo":   map[string]any{"name": "mock", "version": "1"},
					"capabilities": map[string]any{"tools": map[string]any{}}}})
		case "tools/list":
			// A table with enough text in it that a 60-byte window cannot hold it.
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "echo", "description": strings.Repeat("describe the echo tool. ", 12),
						"inputSchema": map[string]any{"type": "object"}},
					{"name": "shout", "description": strings.Repeat("describe the shout tool. ", 12),
						"inputSchema": map[string]any{"type": "object"}},
				}}})
		default:
			http.Error(w, "unknown method", 404)
		}
	}))
	defer srv.Close()

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal mcp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	whole := termText(t, s, map[string]any{"cmd": "get " + h + " tools"})
	if len(whole) <= 60 {
		t.Fatalf("the mock table is too small to prove anything:\n%s", whole)
	}
	cut := termText(t, s, map[string]any{"cmd": "get " + h + " tools", "bytes_limit": 60})
	if !strings.HasPrefix(cut, "TERM/1 206") {
		t.Fatalf("a peer's tools/list ignored bytes_limit=60:\n%s", firstLine(cut))
	}
	if !strings.Contains(cut, "Content-Range: bytes */") {
		t.Errorf("the cut table does not report its full size, so it cannot be priced:\n%s", cut)
	}
	// A single tool's full schema takes the same path.
	schema := termText(t, s, map[string]any{"cmd": "get " + h + " tools echo", "bytes_limit": 60})
	if !strings.HasPrefix(schema, "TERM/1 206") {
		t.Errorf("one tool's schema ignored bytes_limit=60:\n%s", firstLine(schema))
	}
}
