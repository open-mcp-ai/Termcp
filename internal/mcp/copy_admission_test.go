package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLocalTemp creates a file on THIS machine's filesystem — which is what the
// internal profile's SFTP server serves — and returns its path.
func writeLocalTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func existsLocal(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readLocal(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// A PIPELINE IS TWO FILE OPERATIONS, SO BOTH ENDS ARE ADMITTED (issue #358).
//
// The bypass this pins was measured, not theorised: an extension declaring
// `paths = ["/tmp/x"]` answered `403` to `read <its session> file /tmp/outside.txt`
// and then copied the same file with `read … | write …`, because pipeCopy called neither
// the guard nor the whitelist. A whitelist bypassed by punctuation is not a whitelist.
func TestAPipelineCannotReadOutsideTheWhitelist(t *testing.T) {
	s := newExtensionTestServer(t)
	dir := t.TempDir()
	callExtension(t, s, map[string]any{
		"action": "create", "name": "ro358", "target": "internal",
		"paths": []any{dir}, "enabled": true,
	})
	got := useExtension(t, s, "ro358")
	sid, _ := got["session_id"].(string)
	if sid == "" {
		t.Fatalf("extension has no session: %v", got)
	}

	outside := writeLocalTemp(t, "outside-358.txt", "OUTSIDE-THE-WHITELIST\n")
	dest := dir + "/leak.txt"

	out := termText(t, s, map[string]any{
		"cmd":     "read " + sid + " file " + outside + " | write " + sid + " file " + dest,
		"timeout": 20,
	})
	if !strings.Contains(out, "TERM/1 403") {
		t.Fatalf("a pipeline read outside the whitelist was not refused:\n%s", out)
	}
	// AND NOTHING LANDED. A refusal that still started the copy would be the same bug
	// with a different status code.
	if existsLocal(dest) {
		t.Fatalf("the copy ran despite the refusal: %s exists", dest)
	}
}

// THE DESTINATION NEEDS THE RIGHT TO WRITE WHERE IT WRITES.
//
// A read-only window (`--paths`, no file.write capability) must not become a way to put
// bytes somewhere: the source side of a pipeline is legal for it, the destination is not.
func TestAPipelineCannotWriteIntoAReadOnlySession(t *testing.T) {
	s := newExtensionTestServer(t)
	dir := t.TempDir()
	src := writeLocalTemp(t, "src-358.txt", "PAYLOAD\n")
	callExtension(t, s, map[string]any{
		"action": "create", "name": "onlyread358", "target": "internal",
		"paths": []any{dir}, "enabled": true,
	})
	got := useExtension(t, s, "onlyread358")
	sid, _ := got["session_id"].(string)
	if sid == "" {
		t.Fatalf("extension has no session: %v", got)
	}

	dest := dir + "/landed.txt"
	out := termText(t, s, map[string]any{
		"cmd":     "read internal file " + src + " | write " + sid + " file " + dest,
		"timeout": 20,
	})
	if !strings.Contains(out, "TERM/1 403") {
		t.Fatalf("a pipeline wrote into a session without file.write:\n%s", out)
	}
	if existsLocal(dest) {
		t.Fatalf("the copy ran despite the refusal: %s exists", dest)
	}
}

// AN APPROVED COPY COPIES. THIS IS THE ASSERTION THAT MAKES THE GATE REAL.
//
// A pipeline's destination is a `file.write`, which is a reviewed capability — so the
// copy is held, and a human decides. The trap is the replay: the executor reruns the
// submitted tool with the submitted arguments, and a `file_write` whose body came from
// the OTHER end of a pipeline has no `data` argument at all. Submitting it as a plain
// file_write would therefore mean "a human approved a copy" and the approved operation
// writing a zero-byte file — a gate that reports success and loses the bytes.
//
// So the held operation is `file_copy`, which carries both addresses and performs the
// same copy. This test drives the whole path: hold, approve, replay, and then reads the
// destination to prove the CONTENT arrived.
func TestAnApprovedCopyActuallyCopies(t *testing.T) {
	s := newExtensionTestServer(t)
	dir := t.TempDir()
	src := writeLocalTemp(t, "src-approved.txt", "COPY-ME-358\n")
	dest := dir + "/approved.txt"

	// REVIEW MODE ON THE DESTINATION, which is what holds the write half. The session is
	// started by the same helper the other approval tests use, so this exercises the
	// real queue rather than a stub of one.
	sid, _ := startGatedSession(t, s, 1, time.Minute)

	out := termText(t, s, map[string]any{
		"cmd":     "read internal file " + src + " | write " + sid + " file " + dest,
		"timeout": 20,
	})
	if !strings.Contains(out, "Approval-ID:") || !strings.Contains(out, "Submitted for review:") {
		t.Fatalf("the gated copy was not held for review:\n%s", out)
	}
	if existsLocal(dest) {
		t.Fatal("the copy ran before a human decided")
	}

	q := s.sessMgr.Get(sid).ApprovalQueue()
	if q == nil {
		t.Fatal("no approval queue on the destination session")
	}
	pend := q.List()
	if len(pend) != 1 {
		t.Fatalf("queued %d requests, want 1: %v", len(pend), pend)
	}
	req := pend[0]
	// THE REVIEWER IS TOLD IT IS A COPY, and where from: approving "write /x" without
	// knowing what lands there is not a decision about anything.
	// THE SOURCE IN THE SPELLING THE COPY WILL USE: on Windows adaptCallPaths rewrites it
	// to the POSIX wire form before admission, and the summary reports that one path.
	if !strings.Contains(req.Summary, "copy") || !strings.Contains(req.Summary, wireOrSame(src)) {
		t.Errorf("summary %q does not describe a copy from the source (%q)", req.Summary, wireOrSame(src))
	}
	if _, err := q.Approve(req.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.ExecuteApprovedOperation(req); err != nil {
		t.Fatalf("executing the approved copy: %v", err)
	}

	data, err := readLocal(dest)
	if err != nil {
		t.Fatalf("the approved copy produced no file: %v", err)
	}
	if !strings.Contains(data, "COPY-ME-358") {
		t.Fatalf("the approved copy wrote %q, not the source bytes", data)
	}
}
