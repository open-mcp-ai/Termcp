package mcp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUseCommandAdvertisesReadableChannel(t *testing.T) {
	s := newTestServer(t)
	gate := filepath.Join(t.TempDir(), "release")
	// No race against a sleep: the marker cannot arrive before the first call returns.
	line := `while [ ! -f '` + strings.ReplaceAll(gate, "'", `'\''`) + `' ]; do sleep .1; done; echo USE_READ_BACK`
	if runtime.GOOS == "windows" {
		line = `powershell.exe -NoProfile -Command "while (-not (Test-Path -LiteralPath '` + strings.ReplaceAll(gate, "'", "''") + `')) { Start-Sleep -Milliseconds 100 }; Write-Output USE_READ_BACK"`
	}
	reply := s.Term(t.Context(), "run use:internal $1", map[string]any{"args": []any{line}, "timeout": 0.15})
	container := s.sessMgr.LiveUseHost("internal")
	if container != nil {
		t.Cleanup(func() { _ = s.sessMgr.Delete(container.ID) })
	}
	slot := reply.Slots[0]
	if slot.Code/100 != 2 || slot.Headers["Still-Running"] != "true" {
		t.Fatalf("expected live command: %+v", slot)
	}
	handle := slot.Headers["Channel"]
	if !strings.Contains(handle, ":") {
		t.Fatalf("Channel %q is not a session:index target accepted by read", handle)
	}
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(30 * time.Second)
	for time.Now().Before(until) {
		r := s.Term(t.Context(), "read "+handle+" --cursor 0", map[string]any{"timeout": 1}).Slots[0]
		if r.Code >= 400 {
			t.Fatalf("public read refused advertised handle: %+v", r)
		}
		if strings.Contains(r.Body, "USE_READ_BACK") {
			return
		}
	}
	t.Fatal("read never delivered the output produced after run returned")
}

func TestExitedUsePipeExplainsItsStderr(t *testing.T) {
	s := newTestServer(t)
	reply := s.Term(t.Context(), "run use:internal $1", map[string]any{"args": []any{`echo MARKER >&2; exit 7`}, "timeout": 4})
	channel := reply.Slots[0].Headers["Channel"]
	if c := s.sessMgr.LiveUseHost("internal"); c != nil {
		t.Cleanup(func() { _ = s.sessMgr.Delete(c.ID) })
	}
	waitForShellToExit(t, s, shellIDFromHandle(t, s, channel))
	bad := s.Term(t.Context(), "run "+channel+" $1", map[string]any{"args": []any{"echo nope"}}).Slots[0]
	read := "read " + channel + " --stream e --tail 10"
	if bad.Code != 409 || !strings.Contains(bad.Body, "exit code 7") || !strings.Contains(bad.Body, read) {
		t.Fatalf("no useful pipe diagnosis: %+v", bad)
	}
	got := s.Term(t.Context(), read, nil).Slots[0]
	if !strings.Contains(got.Body, "MARKER") {
		t.Fatalf("stderr hint did not retrieve stderr: %+v", got)
	}
}
