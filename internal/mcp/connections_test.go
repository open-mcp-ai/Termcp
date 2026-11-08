package mcp

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// The client list answers "who is connected to this instance?" (issue #99), and
// the value comes entirely from mcp-go's session hooks: there is no other place a
// client attaching or detaching is observable. This test drives a REAL client over
// the real transport, because the part that cannot be checked by reading the code
// is the ordering the library chooses — mcp-go registers the session *before* the
// client has sent anything, so a name read at registration time is empty for every
// client. That is the assumption the registry is built on, and it is exactly the
// kind of thing that changes under a dependency upgrade.
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

	ctx := context.Background()
	cli, err := client.NewStreamableHttpClient(ts.URL, transport.WithContinuousListening())
	if err != nil {
		t.Fatalf("streamable client: %v", err)
	}
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "claude-ai", Version: "0.1.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// The client must appear, with the name it announced. Registration happens
	// before initialize, so this also proves the name is read from the session
	// rather than captured in the register hook.
	conns := waitForConnections(t, s, 1)
	c := conns[0]
	if c.Name != "claude-ai" || c.Version != "0.1.0" {
		t.Errorf("client = %s/%s, want claude-ai/0.1.0 (the name arrives with initialize, after the session registers)",
			c.Name, c.Version)
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
	cli2, err := client.NewStreamableHttpClient(ts.URL)
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	defer cli2.Close()
	if err := cli2.Start(ctx); err != nil {
		t.Fatalf("start second: %v", err)
	}
	init2 := mcpgo.InitializeRequest{}
	init2.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init2.Params.ClientInfo = mcpgo.Implementation{Name: "cursor", Version: "2"}
	if _, err := cli2.Initialize(ctx, init2); err != nil {
		t.Fatalf("initialize second: %v", err)
	}
	conns = waitForConnections(t, s, 2)
	// Oldest first, so the panel does not reshuffle between frames.
	if conns[0].Name != "claude-ai" || conns[1].Name != "cursor" {
		t.Errorf("connections = %s, %s; want claude-ai first (oldest first)", conns[0].Name, conns[1].Name)
	}

	// Disconnecting removes the row: a stale entry would be a client that is not
	// actually attached any more.
	if err := cli.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	conns = waitForConnections(t, s, 1)
	if conns[0].Name != "cursor" {
		t.Errorf("after the first client left, the survivor is %q, want cursor", conns[0].Name)
	}
}

// A caller that claimed a namespace is reported with it: that is what tells the
// owner which agent a connection belongs to when several share the instance.
func TestConnectionsRecordsTheCallersNamespace(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	ctx := context.Background()
	tr, err := transport.NewStreamableHTTP(ts.URL, transport.WithHTTPHeaders(map[string]string{
		NamespaceHeader: "alice",
	}))
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	cli := client.NewClient(tr)
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cli.Close()
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "agent-a", Version: "1"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

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
			owner := connectionOwner{
				PrincipalID:  "agent-1",
				AgentName:    "agent one",
				CredentialID: "credential-1",
				Transport:    ConnectionTransportStreamableHTTP,
				ClientMode:   ClientModeStdioBridge,
			}
			s.connections.add(fakeSession{id: "s1"}, owner)
			s.connections.noteOwner("s1", owner)
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

func (f fakeSession) Initialize()       {}
func (f fakeSession) Initialized() bool { return true }
func (f fakeSession) SessionID() string { return f.id }
func (f fakeSession) NotificationChannel() chan<- mcpgo.JSONRPCNotification {
	return make(chan mcpgo.JSONRPCNotification, 1)
}
