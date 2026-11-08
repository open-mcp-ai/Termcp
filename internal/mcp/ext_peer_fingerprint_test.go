package mcp

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEditingAnExtensionsHeadersReplacesTheMountedPeer pins issue #282.
//
// THE SCENARIO IS THE REPORTED ONE, including the part that made it permanent. An
// endpoint extension is created without the credential it needs, and the server
// answers a missing header by CLOSING THE CONNECTION with zero bytes rather than
// with 401. That answer is a transport failure, so the peer layer keeps the handle
// ("a port that is not up yet is a handle worth having") and caches the mount. When
// the header is then added with `edit ext`, the reconciler rebuilds the instance and
// says so — but the mount key is (session, name) and the session id did not change,
// so the next `open ext` resolved to the mount dialled with the empty headers. The
// reply claimed the old instance was gone while handing back its peer.
//
// WHAT IS PINNED: after `edit ext` adds the credential, the next `open ext` reaches
// the endpoint WITH IT — no intervening `close ext`. The `call` at the end is what
// makes the assertion about behaviour rather than about a handle string: it passes
// only if the live channel was dialled from the current template.
func TestEditingAnExtensionsHeadersReplacesTheMountedPeer(t *testing.T) {
	const token = "Bearer the-right-one"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != token {
			// THE REPORTED SERVER'S BEHAVIOUR, and the reason the stale mount
			// survived: no answer at all, not a refusal. Hijack and close so the
			// client sees EOF, which is a *url.Error and therefore "not reachable
			// yet" rather than "answered and refused".
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var env map[string]any
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			return
		}
		id := env["id"]
		w.Header().Set("Content-Type", "application/json")
		switch env["method"] {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "needs-a-token", "version": "1"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"tools": []any{map[string]any{"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"}}},
			}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "PONG"}},
			}})
		}
	}))
	defer srv.Close()

	s := newExtensionTestServer(t)

	// The endpoint must be loopback-reachable for a `--target internal` template.
	if host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://")); err != nil || host == "" {
		t.Fatalf("unexpected test server URL %q: %v", srv.URL, err)
	}

	if got := termText(t, s, map[string]any{
		"cmd": `create ext tok --target internal --url ` + srv.URL,
	}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("create ext failed:\n%s", got)
	}

	// 1. The first open cannot work: the credential is missing. The handle is kept,
	//    which is the documented lazy-connect contract this bug hid behind.
	first := termText(t, s, map[string]any{"cmd": "open ext tok", "timeout": 10})
	stale, ok := headerValue(t, first, "Handle")
	if !ok || !strings.Contains(stale, "mcp:") {
		t.Fatalf("first open should still hand back a peer handle:\n%s", first)
	}

	// 2. The fix: add the header. `edit ext` rebuilds the instance and says so.
	if got := termText(t, s, map[string]any{
		"cmd": `edit ext tok --headers '{"Authorization":"` + token + `"}'`,
	}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("edit ext failed:\n%s", got)
	}

	// 3. THE ASSERTION: the next open speaks with the new header, with no
	//    intervening `close ext`. Before the fix this returned the same stale peer
	//    and every later call failed with the original EOF.
	second := termText(t, s, map[string]any{"cmd": "open ext tok", "timeout": 10})
	if !strings.HasPrefix(second, "TERM/1 200") {
		t.Fatalf("open ext after the edit should succeed:\n%s", second)
	}
	if !strings.Contains(second, "ping") {
		t.Fatalf("open ext after the edit did not discover the peer's tools:\n%s", second)
	}
	fresh, ok := headerValue(t, second, "Handle")
	if !ok || !strings.Contains(fresh, "mcp:") {
		t.Fatalf("open ext after the edit did not hand back a peer handle:\n%s", second)
	}

	// 4. The address works, not just the reply: a call through the NEW handle
	//    reaches a server that only answers with the credential.
	got := termText(t, s, map[string]any{"cmd": `call ` + fresh + ` ping {}`, "timeout": 10})
	if !strings.HasPrefix(got, "TERM/1 200") || !strings.Contains(got, "PONG") {
		t.Fatalf("the peer mounted after the edit cannot call its tools:\n%s", got)
	}

	// 5. The stale mount is gone, not merely shadowed: the old address no longer
	//    carries the failed channel. A key that leaked would answer here (with the
	//    old EOF) instead of reporting an unknown handle.
	staleCall := termText(t, s, map[string]any{"cmd": `call ` + stale + ` ping {}`, "timeout": 5})
	if strings.HasPrefix(staleCall, "TERM/1 200") {
		t.Fatalf("the stale peer is still callable after the template changed:\n%s", staleCall)
	}
}

// TestAPeerIsNotRemountedWhenTheTemplateIsUnchanged is the other half: the
// fingerprint must discriminate, so a second `open ext` on an unedited template
// still joins the mount instead of dialling a new one. Without this, "replace the
// peer whenever the template differs" could be implemented as "replace it whenever
// open ext runs", and the single-flight this table exists for would be gone.
func TestAPeerIsNotRemountedWhenTheTemplateIsUnchanged(t *testing.T) {
	var handshakes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			return
		}
		id := env["id"]
		w.Header().Set("Content-Type", "application/json")
		switch env["method"] {
		case "initialize":
			handshakes++
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "stable", "version": "1"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"tools": []any{map[string]any{"name": "ping", "description": "p", "inputSchema": map[string]any{"type": "object"}}},
			}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}))
	defer srv.Close()

	s := newExtensionTestServer(t)
	if got := termText(t, s, map[string]any{"cmd": `create ext stable --target internal --url ` + srv.URL}); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("create ext failed:\n%s", got)
	}

	first := termText(t, s, map[string]any{"cmd": "open ext stable", "timeout": 10})
	firstHandle, ok := headerValue(t, first, "Handle")
	if !ok {
		t.Fatalf("first open has no handle:\n%s", first)
	}
	second := termText(t, s, map[string]any{"cmd": "open ext stable", "timeout": 10})
	secondHandle, ok := headerValue(t, second, "Handle")
	if !ok {
		t.Fatalf("second open has no handle:\n%s", second)
	}
	if firstHandle != secondHandle {
		t.Fatalf("an unchanged template must reuse its mount: %q then %q", firstHandle, secondHandle)
	}
	if handshakes != 1 {
		t.Fatalf("an unchanged template was re-dialled %d times, want 1", handshakes)
	}
}
