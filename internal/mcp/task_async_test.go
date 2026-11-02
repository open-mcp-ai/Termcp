package mcp

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAFileUploadAnswersWithAReceipt is issue #158's core contract.
//
// The caller of this daemon is usually an agent behind a tool-call watchdog, so a
// synchronous transfer of a large file is not "slow" — it is a call that WILL be
// killed partway, leaving a truncated file on the far side with no error anywhere.
// The fix is not a longer timeout; it is answering with a receipt in milliseconds and
// letting the caller ask how it went.
func TestAFileUploadAnswersWithAReceipt(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	dir := t.TempDir()
	body := make([]byte, 1<<20)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "landed.bin")

	// A PIPELINE, NOT --local-path. The flag is gone: it read the daemon's disk with
	// no capability check, which is what --no-internal exists to prevent. Both ends
	// here are the same session, so this pins the receipt contract of the copy path
	// that replaced it.
	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + src + " | write " + sid + " file " + dst})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("a transfer must answer with a receipt, not a result:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	if taskID == "" {
		t.Fatalf("the receipt carries no task id, so the transfer cannot be asked about:\n%s", got)
	}
	if !strings.Contains(got, "get task "+taskID) {
		t.Errorf("the receipt does not say how to wait for it:\n%s", got)
	}
	if !strings.Contains(got, "Status:   running") {
		t.Errorf("the receipt does not report the state it is in:\n%s", got)
	}

	// `get task` with a window is how a caller waits: it returns the moment the
	// transfer finishes, so a completed transfer costs no extra waiting.
	final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10})
	if !strings.Contains(final, "Status:   done") {
		t.Fatalf("the transfer did not finish:\n%s", final)
	}
	if !strings.Contains(final, "100%") {
		t.Errorf("a finished transfer does not report 100%%:\n%s", final)
	}

	landed, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("the file never landed: %v", err)
	}
	if !bytes.Equal(landed, body) {
		t.Fatalf("the landed file differs from the source: %d bytes vs %d", len(landed), len(body))
	}
}

// TestAFailedTransferLeavesNoHalfFile is the other half of #158's acceptance: an
// interrupted transfer must not leave a truncated file where the real one goes.
//
// The upload is a whole-file rewrite, so it streams to a temporary sibling and
// renames it over the target only on a clean exit. A failure removes the sibling —
// which is why the test asserts the ABSENCE of both names rather than only the
// absence of the target.
func TestAFailedTransferLeavesNoHalfFile(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	dir := t.TempDir()
	dst := filepath.Join(dir, "never.bin")
	// THE SOURCE IS MISSING, and the pipeline still answers with a receipt: the copy
	// is a background task, so a source that cannot be opened surfaces on the task
	// rather than as a refusal here. That is the same contract the --local-path route
	// had, on the route that survives.
	got := termText(t, s, map[string]any{
		"cmd": "read " + sid + " file " + filepath.Join(dir, "missing.bin") + " | write " + sid + " file " + dst,
	})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("the receipt must come back even when the source is missing:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10})
	if !strings.Contains(final, "Status:   failed") {
		t.Fatalf("a transfer that could not read its source must report failed:\n%s", final)
	}
	if !strings.Contains(final, "Error:") {
		t.Errorf("a failure without its reason sends the caller hunting:\n%s", final)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "never.bin") {
			t.Errorf("a failed transfer left %q behind; the target must never hold a partial file", e.Name())
		}
	}
}

// TestTaskVerbsRefuseWhatTheyCannotAnswer covers the shapes a caller reaches for by
// mistake, because each one used to be answerable with something that looked right.
func TestTaskVerbsRefuseWhatTheyCannotAnswer(t *testing.T) {
	s := newTestServer(t)

	unknown := termText(t, s, map[string]any{"cmd": "get task task_nope", "timeout": 1})
	if !strings.HasPrefix(unknown, "TERM/1 4") {
		t.Errorf("an unknown task id is a caller mistake, not a 200:\n%s", unknown)
	}
	if !strings.Contains(unknown, "ls task") {
		t.Errorf("the refusal does not say how to find the live ones:\n%s", unknown)
	}

	if got := termText(t, s, map[string]any{"cmd": "close task task_nope"}); !strings.HasPrefix(got, "TERM/1 4") {
		t.Errorf("cancelling an unknown task must be refused:\n%s", got)
	}

	// `ls task <id>` would answer the whole list and look filtered (the same silent
	// drop `ls host <name>` was fixed for).
	if got := termText(t, s, map[string]any{"cmd": "ls task task_x"}); !strings.HasPrefix(got, "TERM/1 4") {
		t.Errorf("`ls task <id>` must be refused, not answered with the whole list:\n%s", got)
	}

	// And the empty listing teaches the verb that fills it.
	empty := termText(t, s, map[string]any{"cmd": "ls task"})
	if !strings.Contains(empty, "read") || !strings.Contains(empty, "write") {
		t.Errorf("an empty task list does not say how a transfer is started:\n%s", empty)
	}
}

// TestAnUploadLandsWhereTheArgumentSaid pins the end-to-end half of the sentinel leak.
//
// The parser's own test proves the hole is filled; this proves the filled value is
// what the file verb acts on. They are different claims: a value expanded into a field
// nobody reads is still a file in the wrong place, and the reported symptom was
// exactly that — a 202 "accepted" receipt naming a substitution hole, and the caller
// finding out by looking for the file where they had asked.
func TestAnUploadLandsWhereTheArgumentSaid(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "asked-for.bin")

	// The destination arrives as a positional value, which is how an agent passes a
	// path it computed: the address in the receipt must be the address it computed.
	got := termText(t, s, map[string]any{
		"cmd":  "read " + sid + " file " + src + " | write " + sid + " file $1",
		"args": []any{dst},
	})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("a transfer must answer with a receipt:\n%s", got)
	}
	if strings.Contains(got, "__hole_") {
		t.Fatalf("the receipt names a substitution placeholder as the destination:\n%s", got)
	}
	if !strings.Contains(got, dst) {
		t.Fatalf("the receipt does not name the path that was asked for (%s):\n%s", dst, got)
	}
	// And the bytes really are there, which is the part a receipt cannot be trusted
	// about on its own.
	taskID := headerValueOr(got, "Task", "")
	if taskID == "" {
		t.Fatalf("no task id in:\n%s", got)
	}
	done := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 20})
	if !strings.Contains(done, "done") {
		t.Fatalf("the transfer did not finish:\n%s", done)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("the file is not where the argument said: %v", err)
	}
}
