package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/open-mcp-ai/termcp/mcp"
)

// The client list answers "who is connected to this instance?" (issue #99), and the
// value comes entirely from the transport's session hooks. These tests drive the
// protocol over real HTTP — initialize, then DELETE — because the part that cannot
// be checked by reading the code is the ORDER of the two facts: the session exists
// before it has a name, and the name arrives with initialize.
//
// A hand-rolled client is used on purpose. A library client would also test that
// library; what matters here is that the bytes on the wire produce a row in the
// registry.

// openStreamableSession performs the handshake and returns the session id the
// server minted. Close it with endStreamableSession.
func openStreamableSession(t *testing.T, url, namespace, name, version string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` +
		mcpgo.LATEST + `","capabilities":{},"clientInfo":{"name":"` + name + `","version":"` + version + `"}}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if namespace != "" {
		req.Header.Set(NamespaceHeader, namespace)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize answered %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not hand out a session id")
	}
	return sid
}

// endStreamableSession terminates one, the way a client that is done does.
func endStreamableSession(t *testing.T, url, sid string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestConnectionsReportsRealClients(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	// Nothing attached yet: an empty list, not nil.
	if got := s.Connections(); len(got) != 0 {
		t.Fatalf("a fresh server has no clients, got %+v", got)
	}

	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	// A listener is how the Web UI learns without polling; count its calls.
	var wakes int
	s.SetConnectionsListener(func() { wakes++ })

	sid := openStreamableSession(t, ts.URL, "", "claude-ai", "0.1.0")

	conns := waitForConnections(t, s, 1)
	c := conns[0]
	if c.Name != "claude-ai" || c.Version != "0.1.0" {
		t.Errorf("client = %s/%s, want claude-ai/0.1.0", c.Name, c.Version)
	}
	if c.SessionID == "" {
		t.Error("a connection needs the transport session id so two clients with one name can be told apart")
	}
	if c.ConnectedAt == "" {
		t.Error("a connection needs a connected_at timestamp")
	}
	if wakes == 0 {
		t.Error("attaching a client must wake the UI listener, or the panel would only update on a reload")
	}

	// A second client is a second row, not a replacement.
	sid2 := openStreamableSession(t, ts.URL, "", "cursor", "2")
	conns = waitForConnections(t, s, 2)
	// Oldest first, so the panel does not reshuffle between frames.
	if conns[0].Name != "claude-ai" || conns[1].Name != "cursor" {
		t.Errorf("connections = %s, %s; want claude-ai first (oldest first)", conns[0].Name, conns[1].Name)
	}

	// Ending a session removes the row: a stale entry would be a client that is not
	// actually attached any more.
	endStreamableSession(t, ts.URL, sid)
	conns = waitForConnections(t, s, 1)
	if conns[0].Name != "cursor" {
		t.Errorf("after the first client left, the survivor is %q, want cursor", conns[0].Name)
	}
	_ = sid2
}

// A caller that claimed a namespace is reported with it: that is what tells the
// owner which agent a connection belongs to when several share the instance.
func TestConnectionsRecordsTheCallersNamespace(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	openStreamableSession(t, ts.URL, "alice", "agent-a", "1")

	conns := waitForConnections(t, s, 1)
	if conns[0].Namespace == "" {
		t.Fatal("a caller that claimed a namespace must be listed with it, or the owner cannot tell two agents apart")
	}
	if conns[0].Name != "agent-a" {
		t.Errorf("name = %q, want agent-a", conns[0].Name)
	}
}

// waitForConnections polls until the registry holds want rows. The hooks are
// called from the transport's goroutine, so an immediate read races the
// registration that follows the client's own request.
func waitForConnections(t *testing.T, s *Server, want int) []Connection {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := s.Connections()
		if len(got) == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("connections = %d, want %d (%+v)", len(got), want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The registry is read from the HTTP handler goroutine while the transport's own
// goroutine mutates it, so it must be safe under -race. This is the cheap check
// that the lock actually covers both paths; the end-to-end test above is what
// exercises it concurrently.
func TestConnRegistryIsSafeUnderConcurrentUse(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.connections.add(fakeSession{id: "s1"})
			s.connections.noteNamespace("s1", "ns:alice")
			s.connections.list()
			s.connections.count()
			s.connections.remove(fakeSession{id: "s1"})
		}
	}()
	for i := 0; i < 200; i++ {
		s.connections.list()
		s.connections.count()
	}
	<-done
}

// fakeSession is the minimum mcp-go session the registry touches.
type fakeSession struct{ id string }

func (f fakeSession) SessionID() string { return f.id }
func (f fakeSession) GetClientInfo() mcpgo.Implementation {
	return mcpgo.Implementation{}
}
func (f fakeSession) Notify(ctx context.Context, method string, params any) error { return nil }
