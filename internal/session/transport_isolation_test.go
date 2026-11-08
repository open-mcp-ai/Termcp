package session

import (
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestEachSessionDialsItsOwnTransport pins that a session opened from a host
// gets a connection of its own, never a client another session already holds.
//
// The transport pool (#237) exists for ONE-SHOT `use` operations: `run
// use:<host>` and the file verbs rent a connection for the length of one call so
// they leave no session record behind. When `Manager.Create` also drew its
// transport from that pool, two sessions on the same host shared a single SSH
// client — and then the sessions were not independent at all: closing one closed
// the client under the other, and the survivor every UI heuristic still called
// "running" could not open a channel ("use of closed network connection").
//
// The check is the two identities plus surviving the sibling's close, because
// either half alone passes on the broken code: the addresses prove the sharing,
// and opening a new channel on the survivor proves what the sharing cost.
func TestEachSessionDialsItsOwnTransport(t *testing.T) {
	srv := startTestServer(t)
	m := NewManager(nil, nil, srv)

	first, err := m.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "first"))
	if err != nil {
		t.Fatalf("create the first session: %v", err)
	}
	second, err := m.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "second"))
	if err != nil {
		t.Fatalf("create the second session: %v", err)
	}
	defer second.Terminate(true, 0)

	firstClient := first.execSession.SSHClient()
	secondClient := second.execSession.SSHClient()
	if firstClient == nil || secondClient == nil {
		t.Fatalf("an internal session owns no SSH client (first=%v second=%v)", firstClient, secondClient)
	}
	if firstClient == secondClient {
		t.Fatal("two sessions were handed the SAME SSH client: re-opening a host on the UI would reuse " +
			"the connection another session holds, and closing that one would take this session's channels with it")
	}

	first.Terminate(true, 0)

	// A new channel is the cheapest proof that the survivor's connection is
	// still usable: it needs a live SSH session on the survivor's own client.
	if _, err := second.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, ""); err != nil {
		t.Fatalf("the surviving session cannot open a channel after its sibling closed: %v", err)
	}
	if status := second.Info().Status; status != api.SessionRunning {
		t.Fatalf("closing one session changed the other's status to %q", status)
	}
}
