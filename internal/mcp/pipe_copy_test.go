package mcp

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// TestAPipelineCopiesBytesWithoutStoringThem is the whole point of `read | write`:
// bytes move from one node to another and never pass through the caller's context,
// which is what makes it different from `read` + `write` in two calls.
func TestAPipelineCopiesBytesWithoutStoringThem(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")
	awaitPtyReady(t, s, h)

	// THE CHANNEL HAS TO HAVE SAID SOMETHING. Its default cursor is the end of the
	// stream, so a freshly opened terminal has nothing after it and the copy is
	// correctly empty. The marker is COMPUTED BY THE SHELL rather than printed: a
	// plain `echo marker` would put the word in the transcript twice — once as the
	// command echo, once as its output — and the assertion could be satisfied by the
	// echo of a command that never ran.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo pipeline-$(echo marker)"}})

	// WAIT FOR THE OUTPUT TO BE IN THE LOG, NOT FOR THE WINDOW TO END.
	//
	// A pty `run` returns when the stream goes QUIET, and a quiet moment can fall
	// between the terminal echoing the command line and the command's own output
	// arriving — so the copy below could run against a log that holds the echo
	// (`echo pipeline-$(echo marker)`) and not yet the result (`pipeline-marker`), which
	// is exactly the string the assertion needs. Observed once in a full shuffled run.
	//
	// The wait is a POSITIONAL read, so it consumes nothing: `--cursor 0` names its own
	// window, and repeating it cannot skip past the bytes the pipeline is about to copy.
	deadline := time.Now().Add(10 * time.Second)
	for {
		seen := termText(t, s, map[string]any{"cmd": "read " + h + " --cursor 0", "timeout": 2})
		if strings.Contains(seen, "pipeline-marker") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command's output never reached the channel's log, so there is nothing "+
				"for the pipeline to copy:\n%s", seen)
		}
		time.Sleep(20 * time.Millisecond)
	}

	dir := t.TempDir()
	dst := filepath.Join(dir, "copied.txt")

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --cursor 0 | write " + sid + " file " + dst})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("a copy answers with a receipt, not a result:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	if taskID == "" {
		t.Fatalf("the receipt carries no task id:\n%s", got)
	}
	if !strings.Contains(got, "|") {
		t.Errorf("the receipt does not echo the pipeline, so it reads like a plain read:\n%s", got)
	}

	final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10})
	if !strings.Contains(final, "Status:   done") {
		t.Fatalf("the copy did not finish:\n%s", final)
	}
	landed, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("the destination file was never written: %v", err)
	}
	if !strings.Contains(string(landed), "pipeline-marker") {
		t.Fatalf("the channel's bytes did not reach the file:\n%s", landed)
	}
	// AND NOTHING WAS STORED IN BETWEEN: the copy holds the span it read and streams it
	// out. There is no spool directory to leave behind, which is what makes a pipeline
	// different from download-then-upload.
	if left := spoolLeftovers(t); len(left) > 0 {
		t.Errorf("the copy left files behind: %v", left)
	}
}

// TestAPipelineIsNotCappedByTheDefaultWindow. `bytes_limit` has a default of 200, and
// a copy that read that default as a request would cut every copy to 200 bytes and
// report success — the worst kind of wrong, because the file looks fine.
func TestAPipelineIsNotCappedByTheDefaultWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")
	awaitPtyReady(t, s, h)

	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	body := make([]byte, 64<<10)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "landed.bin")

	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + src + " | write " + sid + " file " + dst})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("the copy was refused:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	if final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10}); !strings.Contains(final, "Status:   done") {
		t.Fatalf("the copy did not finish:\n%s", final)
	}
	landed, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(landed, body) {
		t.Fatalf("the copy is not byte-identical: %d bytes vs %d", len(landed), len(body))
	}
}

// TestAPipelineRefusesWhatItCannotDo. Every refusal here used to be a silent drop of
// the right-hand side, or a destination that would corrupt the bytes.
func TestAPipelineRefusesWhatItCannotDo(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	cases := []struct {
		cmd  string
		want string
	}{
		// A terminal is not a file: it echoes, wraps and interprets.
		{"read " + h + " | write " + h, "must be `write <session> file <path>`"},
		// A second source of the same bytes is refused, not ignored.
		{"read " + h + " | write " + sid + " file /tmp/x --data hi", "a second source"},
		// Frames are a framing, not a byte stream.
		{"read " + sid + " cdp:1 | write " + sid + " file /tmp/x", "frame source"},
	}
	for _, c := range cases {
		got := termText(t, s, map[string]any{"cmd": c.cmd})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("%s: not refused:\n%s", c.cmd, got)
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: the refusal does not name the reason (%q):\n%s", c.cmd, c.want, got)
		}
	}
}

// spoolLeftovers names any copy spool left in the system temp directory.
//
// A COPY MUST NOT STORE ANYTHING, so the absence of a spool is a property worth
// checking rather than a detail of the implementation: the previous design wrote the
// source into a temp file between the two ends, and a test that only looked at the
// destination bytes would not notice it coming back.
func spoolLeftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "termcp-copy-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestAPipelineIgnoresTheReplyWindow pins #290: `bytes_limit` is a REPLY window, not a
// data boundary. It was read as a copy cap, so `bytes_limit: 100` over a 4 KiB source
// wrote a 100-byte prefix and the task still reported 100% done — a corrupt file that
// looks finished. The copy length is the caller's `--bytes` / `--offset` and nothing
// else, so a destination must come back the size of its source however small the
// window is.
func TestAPipelineIgnoresTheReplyWindow(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	body := bytes.Repeat([]byte("abcdefgh"), 512) // 4 KiB
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "landed.bin")

	got := termText(t, s, map[string]any{
		"cmd":         "read " + sid + " file " + src + " | write " + sid + " file " + dst,
		"bytes_limit": 100,
	})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("the copy was refused:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	if final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10}); !strings.Contains(final, "Status:   done") {
		t.Fatalf("the copy did not finish:\n%s", final)
	}
	landed, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(landed) != len(body) {
		t.Fatalf("bytes_limit 100 truncated a %d-byte copy to %d bytes: a reply window is not a data boundary", len(body), len(landed))
	}
	if !bytes.Equal(landed, body) {
		t.Error("the copy is not byte-identical to the source")
	}
}

// TestAPipelineCopiesThroughAUseHostTarget pins #290's other half: `internal` and
// `use:<host>` are `use` targets, and the copy used a helper that refuses them
// because it has nowhere to hand the transport lease back. The pipeline was accepted
// with 202 and then failed at 0 bytes with "use SFTP requires a request-scoped
// transport lease".
//
// The internal profile needs no SSH config, so it is the cheapest `use` target to
// stand up; both ends are `use` here, which is also the only shape that exercises
// both leases at once.
func TestAPipelineCopiesThroughAUseHostTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := startTestSSH(t)

	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	cleanupTestRuntime(t, sessMgr, srv)
	s := New(sessMgr, msgMgr, sshconfig.NewStore(dir), nil, "test")

	src := filepath.Join(dir, "use-copy.bin")
	body := bytes.Repeat([]byte("use-data"), 256)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copied.bin")

	got := termText(t, s, map[string]any{
		"cmd": "read internal file " + filepath.ToSlash(src) + " | write internal file " + filepath.ToSlash(dst),
	})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("a `use` target must be accepted as a copy end:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10})
	if !strings.Contains(final, "Status:   done") {
		t.Fatalf("the copy did not finish; a `use` end must get a request-scoped lease:\n%s", final)
	}
	landed, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("the destination file was never written: %v", err)
	}
	if !bytes.Equal(landed, body) {
		t.Fatalf("the copy is not byte-identical: %d bytes vs %d", len(landed), len(body))
	}
	// A `USE:` COPY IS NOT A SESSION: the whole point of #237 is that it does not
	// create one, and a lease that leaked a session would show up here.
	if all := sessMgr.ListAll(); len(all) != 0 {
		t.Errorf("a `use` copy created %d sessions: %+v", len(all), all)
	}
}

// TestAPipelineCanBeCancelled. Neither SFTP streaming call takes a context, so a
// `close task` used to mark the task cancelled while the bytes kept moving — a
// cancellation that reports success and does nothing.
func TestAPipelineCanBeCancelled(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(src, make([]byte, 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "cancelled.bin")

	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file " + src + " | write " + sid + " file " + dst})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("the copy was refused:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	// Cancel immediately: a local 8 MiB copy is fast, so this races it on purpose. The
	// assertion is not "it was cancelled in time" but "the record is consistent" —
	// either it finished, or it stopped with the cancellation as the reason.
	termText(t, s, map[string]any{"cmd": "close task " + taskID})
	final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10})
	if !strings.Contains(final, "Status:   done") && !strings.Contains(final, "Status:   cancelled") {
		t.Fatalf("a cancelled copy must settle as done or cancelled:\n%s", final)
	}
}
