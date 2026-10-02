package mcp

import (
	"context"
	"testing"
)

// session_start must not report a pid. It used to return one that was always 0:
// the process is remote and SSH does not tell us its number, so the field could
// never have held anything true. Asserted on the tool response because that is
// the contract clients actually read.
func TestSessionStartReportsNoPID(t *testing.T) {
	s := newTestServer(t)

	startReq := makeRequest(map[string]any{
		"ssh_config": "internal",
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
	})
	res, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("session_start returned an error: %+v", res)
	}

	m := parseResult(t, res)
	if pid, ok := m["pid"]; ok {
		t.Errorf("session_start still reports pid=%v; it was always 0 and has been removed", pid)
	}
	// The keys clients depend on must survive the removal.
	for _, k := range []string{"session_id", "shell_id", "ssh_config"} {
		if _, ok := m[k]; !ok {
			t.Errorf("session_start response lost %q: %v", k, m)
		}
	}
}
