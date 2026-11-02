package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAQuotedPathStillLandsWhereItSays pins the end-to-end half of the escaping rule,
// because the failure mode was a SUCCESSFUL command: a path carrying escapes the
// caller never wrote is not a refusal, it is a different file in a different place —
// and the transfer reported `done`.
//
// Quoting a path is the ordinary instinct, so the quoted spelling has to mean the same
// thing as the unquoted one.
func TestAQuotedPathStillLandsWhereItSays(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "asked.bin")

	// A PIPELINE NOW. The quoted-destination question this test pins is unchanged --
	// a path the caller computed must arrive byte for byte -- and the pipeline is the
	// route that still exists for moving bytes onto a session.
	got := termText(t, s, map[string]any{
		"cmd":  `read ` + sid + ` file ` + src + ` | write ` + sid + ` file "$1"`,
		"args": []any{dst},
	})
	if !strings.HasPrefix(got, "TERM/1 202") {
		t.Fatalf("a transfer must answer with a receipt:\n%s", got)
	}
	// The receipt is the assertion that matters: it is what the caller reads to learn
	// where the file went, and it must be the path they asked for, byte for byte.
	if !strings.Contains(got, dst) {
		t.Fatalf("the receipt does not name the path that was asked for (%s):\n%s", dst, got)
	}
	if strings.Contains(got, `\\`) {
		t.Fatalf("the destination carries escape sequences the caller never wrote:\n%s", got)
	}

	taskID := headerValueOr(got, "Task", "")
	if taskID == "" {
		t.Fatalf("no task id in:\n%s", got)
	}
	if done := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 20}); !strings.Contains(done, "done") {
		t.Fatalf("the transfer did not finish:\n%s", done)
	}
	// The file exists at the asked-for path, and NOT at an escaped spelling of it.
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("the file is not where the argument said: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), `\`) {
			t.Errorf("an escaped twin was created: %q", e.Name())
		}
	}
}
