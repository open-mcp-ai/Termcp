package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sftp"
)

// TestFileResultZeroLengthKeepsShape pins bug #11: a zero-length read must still
// report data, length and bytes_read rather than omitting them under omitempty.
func TestFileResultZeroLengthKeepsShape(t *testing.T) {
	empty := ""
	r := &sftp.FileResult{
		Mode:      "text",
		TotalSize: 10,
		Offset:    10,
		Length:    0,
		BytesRead: 0,
		Data:      &empty,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"data", "length", "bytes_read", "mode", "total_size"} {
		if _, ok := m[want]; !ok {
			t.Errorf("zero-length FileResult JSON must contain %q, got: %s", want, string(b))
		}
	}
}

// TestInternalRealpathResolvesSymlink pins bug #7: realpath on the internal profile
// must resolve symlinks through to their real target, like a real SSH host does.
func TestInternalRealpathResolvesSymlink(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "pty internal --name live"})
	h := handleFrom(t, text)
	sid := strings.SplitN(h, ":", 2)[0]

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got := termText(t, s, map[string]any{
		"cmd": "file " + sid + " --action fs --op realpath --remote-path " + link,
	})
	// COMPARE WHAT THE DIRECTIVE MEANS, NOT THE BYTES IT PRINTS.
	//
	// Two decodings stand between `filepath.Join` and the reply. The SFTP wire form
	// is POSIX-shaped, so Windows paths come back as "/C:/dir/file"; and the OS may
	// answer with a long path where t.TempDir() handed out an 8.3 short one
	// ("RUNNER~1" -> "runneradmin"). Both are the same file, and neither says anything
	// about whether the symlink was resolved -- which is the only claim here.
	//
	// So resolve the link locally the same way and compare FINAL PATHS. If the
	// directive returned the link's own path (the bug), the two disagree. If it
	// returned the target under a different spelling, they agree.
	wantPath, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", link, err)
	}
	var gotPath string
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "canonical_path:") {
			gotPath = strings.TrimSpace(strings.TrimPrefix(line, "canonical_path:"))
			break
		}
	}
	if gotPath == "" {
		t.Fatalf("no canonical_path in reply:\n%s", got)
	}
	norm := func(p string) string {
		p = filepath.FromSlash(strings.TrimPrefix(p, "/"))
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return filepath.ToSlash(p)
	}
	if norm(gotPath) != norm(wantPath) {
		t.Errorf("realpath must resolve the symlink to %q, got %q:\n%s", wantPath, gotPath, got)
	}
}
