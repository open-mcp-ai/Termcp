package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A PTY executes this binary directly (pipe mode may run a shell wrapper).
// Its own PID is an oracle independent of the reconciler and the SSH server.
func TestExtensionPIDChild(t *testing.T) {
	path := os.Getenv("TERMCP_TEST_EXT_PID_FILE")
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}

func TestGetExtensionObservedHealth(t *testing.T) {
	s := newExtensionTestServer(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pid")
	t.Setenv("TERMCP_TEST_EXT_PID_FILE", path)
	const name = "health-example"
	callExtension(t, s, map[string]any{"action": "create", "name": name, "target": "internal", "command": binary, "mode": "pty", "args": []any{"-test.run=^TestExtensionPIDChild$"}})
	get := func() map[string]any { return callExtension(t, s, map[string]any{"action": "get", "name": name}) }
	idle := get()
	if idle["pid"] != nil || idle["last_active_at"] != nil || idle["channel_open"] != nil || idle["restarts"] != float64(0) {
		t.Fatalf("unstarted template has invented evidence: %v", idle)
	}
	useExtension(t, s, name)
	open := get()
	if open["channel_open"] != true || open["restarts"] != float64(0) || open["last_active_at"] == nil {
		t.Fatalf("unexpected live health: %v", open)
	}
	deadline := time.Now().Add(30 * time.Second)
	var pid int
	for {
		b, _ := os.ReadFile(path)
		pid, _ = strconv.Atoi(string(b))
		if pid > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not publish its PID")
		}
		time.Sleep(20 * time.Millisecond)
	}
	open = get()
	if open["pid"] != float64(pid) {
		t.Fatalf("reported PID %v; child reports %d", open["pid"], pid)
	}
	doc := termText(t, s, map[string]any{"cmd": "get ext " + name})
	for _, field := range []string{fmt.Sprintf("pid: %d\n", pid), "channel_open: true", "last_active_at: ", "restarts: 0"} {
		if !strings.Contains(doc, field) {
			t.Errorf("missing %q in public response:\n%s", field, doc)
		}
	}
	if got := get(); got["last_active_at"] != open["last_active_at"] {
		t.Errorf("reading health changed activity: %v -> %v", open, got)
	}
	if _, err := s.extensions.Release(name, open["shell_id"].(string)); err != nil {
		t.Fatal(err)
	}
	closed := get()
	if closed["pid"] != nil || closed["channel_open"] != nil || closed["last_active_at"] != open["last_active_at"] {
		t.Fatalf("closed health: %v", closed)
	}
}
