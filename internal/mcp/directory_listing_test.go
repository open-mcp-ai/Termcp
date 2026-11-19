package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type listedDir struct {
	Type    string `json:"type"`
	Entries []struct {
		Name  string
		Type  string
		Size  *int64
		Mtime *int64
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

func readDirectoryFixture(t *testing.T, s *Server, sid, dir string, budget int) listedDir {
	t.Helper()
	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file $1", "args": []any{dir}, "bytes_limit": budget})
	var page listedDir
	_, body, ok := strings.Cut(got, "\n\n")
	if !ok || json.Unmarshal([]byte(body), &page) != nil || page.Type != "directory" || page.Entries == nil {
		t.Fatalf("expected a single parseable directory object:\n%s", got)
	}
	return page
}

func TestDirectoryReadAndDefaultWrite(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir := t.TempDir()
	if page := readDirectoryFixture(t, s, sid, dir, 8192); len(page.Entries) != 0 || page.Truncated {
		t.Fatalf("empty: %+v", page)
	}
	name := "a file.txt"
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	page := readDirectoryFixture(t, s, sid, dir, 8192)
	if page.Truncated || len(page.Entries) != 2 || page.Entries[0].Name != name || page.Entries[0].Type != "file" || page.Entries[0].Size == nil || *page.Entries[0].Size != 8 || page.Entries[0].Mtime == nil || *page.Entries[0].Mtime <= 0 || page.Entries[1].Name != "sub" || page.Entries[1].Type != "directory" {
		t.Fatalf("bad metadata: %+v", page)
	}
	got := termText(t, s, map[string]any{"cmd": "write " + sid + " file $1 --data nope", "args": []any{dir}})
	if !statusLineIs(got, "400") || !strings.Contains(got, "<directory>/<name>") {
		t.Fatalf("implicit write: %s", got)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "original" {
		t.Fatalf("implicit write changed bytes: %q, %v", b, err)
	}
	if es, err := os.ReadDir(dir); err != nil || len(es) != 2 {
		t.Fatalf("implicit write changed directory: %v, %v", es, err)
	}

	t.Run("symlink", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "link")); err != nil {
			t.Skipf("no symlink support: %v", err)
		}
		page := readDirectoryFixture(t, s, sid, dir, 8192)
		if len(page.Entries) != 3 || page.Entries[1].Name != "link" || page.Entries[1].Type != "symlink" {
			t.Fatalf("link was followed or hidden: %+v", page)
		}
	})
}

func TestDirectoryReadCapsCompleteEntries(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir := t.TempDir()
	for i := 139; i >= 0; i-- {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%03d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	page := readDirectoryFixture(t, s, sid, dir, 32768)
	if len(page.Entries) != 128 || !page.Truncated || page.Entries[0].Name != "entry-000" || page.Entries[127].Name != "entry-127" {
		t.Fatalf("uncapped/unsorted: %+v", page)
	}
	small := readDirectoryFixture(t, s, sid, dir, 1024)
	if len(small.Entries) == 0 || len(small.Entries) >= len(page.Entries) || !small.Truncated {
		t.Fatalf("bad window: %+v", small)
	}
	fold := termText(t, s, map[string]any{"cmd": "read " + sid + " file $1", "args": []any{dir}, "bytes_limit": 1024, "on_bytes_overflow": "fold"})
	if !statusLineIs(fold, "206") || strings.Contains(fold, "entry-000") {
		t.Fatalf("fold exposed entries: %s", fold)
	}
}

func TestDirectoryReadRequiresStatPermission(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	const secretName = "should-not-be-listed"
	if err := os.WriteFile(filepath.Join(dir, secretName), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	sid := guardTestSession(t, s, []string{"file_read"}, nil)
	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file $1", "args": []any{dir}})
	if !statusLineIs(got, "403") || strings.Contains(got, secretName) {
		t.Fatalf("directory metadata without file.stat: %s", got)
	}
}

func TestDirectoryReadReportsFilesystemPermission(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0); err != nil {
		t.Skipf("permission fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if _, err := os.ReadDir(dir); !os.IsPermission(err) {
		t.Skip("this filesystem or principal can still enumerate mode 000")
	}
	got := termText(t, s, map[string]any{"cmd": "read " + sid + " file $1", "args": []any{dir}})
	if !statusLineIs(got, "403") {
		t.Fatalf("unreadable directory reported as a successful listing: %s", got)
	}
}
