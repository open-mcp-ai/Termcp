package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ENDING A SESSION HAS TO END ITS GET STREAM.
//
// The stream loop waits on the session's out channel, and closing a session clears that
// pointer without touching the channel a reader is parked on. So the reader stayed, the
// keep-alive tick kept writing to a live connection, and a subscriber sat on a stream that
// could never carry another frame — the failure with no symptom. EndSessions is what an
// extension endpoint says when the thing behind it is gone (internal/mcp/ext_pump.go), so
// this test belongs to the transport: the stream must end, and the transport must stay
// usable for the next client.
func TestEndSessionsEndsAnAttachedGetStream(t *testing.T) {
	srv := newTestServer(t)
	tr := NewStreamableHTTPServer(srv)
	ts := httptest.NewServer(tr)
	defer ts.Close()

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"sub","version":"1"}}}`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(init))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sid := resp.Header.Get(sessionHeader)
	if sid == "" {
		t.Fatal("initialize did not hand out a session id")
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set(sessionHeader, sid)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("GET answered %d, want the stream", stream.StatusCode)
	}

	// ATTACHED IS A STATE, so wait for it and not for a duration. Do returning means the
	// headers were flushed, which the handler does right after openStream — this loop is
	// what makes that a fact the test reads rather than one it assumes.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, ok := tr.sessions.Load(sid); ok && v.(*streamClient).hasOpenStream() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the GET stream never attached to its session")
		}
		time.Sleep(2 * time.Millisecond)
	}

	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, stream.Body)
		close(ended)
	}()

	if n := tr.EndSessions(context.Background()); n != 1 {
		t.Fatalf("EndSessions ended %d sessions, want 1", n)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the attached GET stream is still open after its session ended")
	}

	// ONCE: the idle reaper and a DELETE can both reach a session, so the second ending
	// must be a no-op rather than a close of a closed channel.
	if n := tr.EndSessions(context.Background()); n != 0 {
		t.Fatalf("a second EndSessions ended %d sessions, want none", n)
	}

	// THE TRANSPORT STILL SERVES: the point is "the thing behind this endpoint is gone",
	// not "this endpoint is closed". A client that comes back initializes as if new.
	if code := postFrame(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sid); code != http.StatusNotFound {
		t.Errorf("a request naming the ended session answered %d, want 404", code)
	}
	if code := postFrame(t, ts.URL, init, ""); code != http.StatusOK {
		t.Fatalf("initialize after EndSessions answered %d", code)
	}
}

// postFrame posts one frame and reports the status, so a contract like "404 for a session
// that ended" is one line rather than five.
func postFrame(t *testing.T, url, body, sessionID string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(sessionHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
