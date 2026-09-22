package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// newStore returns a Store whose handles are released when the test ends. The
// append handle is kept open for the life of a shell, and on Windows an open
// handle blocks removal of the file, so a test that leaves it open cannot clean
// up its own temp directory.
func newStore(t *testing.T) *Store {
	t.Helper()
	st := New(t.TempDir())
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// An offset is the file position, so reading a window is a positional read and
// the reported total is the file size. Neither is derived from record lengths,
// which is what makes the two impossible to disagree.
func TestLog_OffsetIsFilePosition(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"

	// Write in several chunks, as the PTY reader does.
	chunks := [][]byte{[]byte("AAAA"), []byte("BBBB"), []byte("CCCC")}
	var want []byte
	for _, c := range chunks {
		off, err := st.AppendLog(sess, shell, c)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(len(want)); off != want {
			t.Fatalf("AppendLog returned offset %d, want %d (the file position)", off, want)
		}
		want = append(want, c...)
	}

	size, err := st.LogSize(sess, shell)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(want)) {
		t.Errorf("LogSize = %d, want %d", size, len(want))
	}

	// Every window must equal the same slice of the bytes, including windows that
	// begin mid-chunk.
	for start := int64(0); start <= size; start++ {
		for _, max := range []int{1, 3, 4, 7, 64} {
			got, err := st.ReadLog(sess, shell, start, max)
			if err != nil {
				t.Fatal(err)
			}
			end := start + int64(max)
			if end > size {
				end = size
			}
			if !bytes.Equal(got, want[start:end]) {
				t.Fatalf("ReadLog(start=%d,max=%d) = %q, want %q", start, max, got, want[start:end])
			}
		}
	}
}

// Multi-byte characters must survive a chunk boundary that splits them: the log
// stores bytes, so a character cut in half by a 4096-byte read is still whole in
// the file, and a window read reassembles it.
func TestLog_SplitMultiByteCharacterSurvives(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s2", "sh2"

	full := []byte(strings.Repeat("中文测试数据", 2000)) // 3 bytes per character
	// Split at a position that lands inside a character (4096 % 3 != 0).
	cut := 4096
	if cut >= len(full) {
		t.Fatalf("test data too small: %d bytes", len(full))
	}
	first, second := full[:cut], full[cut:]

	// Append the two halves as the reader would: a 4096-byte read that lands
	// inside a character.
	if _, err := st.AppendLog(sess, shell, first); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog(sess, shell, second); err != nil {
		t.Fatal(err)
	}
	// A window straddling the boundary must return the character whole.
	got, err := st.ReadLog(sess, shell, int64(cut-2), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full[cut-2:cut+2]) {
		t.Fatalf("window across the split = %v, want %v", got, full[cut-2:cut+2])
	}
	all, err := st.ReadLog(sess, shell, 0, len(full)+16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(all, full) {
		t.Fatalf("reassembled log differs: %d bytes vs %d", len(all), len(full))
	}
	if bytes.Contains(all, []byte("\uFFFD")) {
		t.Error("reassembled log contains U+FFFD: bytes were replaced, not stored")
	}
}

// Marks are an index, not the data: the span each one starts runs to the next
// mark, and the last runs to the end of the file. A mark whose bytes were never
// written (crash between the two appends) must not break reading.
func TestMarks_SpanToNextMarkAndTolerateTrailingGap(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s3", "sh3"

	if _, err := st.AppendLog(sess, shell, []byte("OUT1")); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: 1, Offset: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog(sess, shell, []byte("IN")); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogAPIInput, Time: 2, Offset: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog(sess, shell, []byte("OUT2")); err != nil {
		t.Fatal(err)
	}

	marks, err := st.ReadMarks(sess, shell)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 2 {
		t.Fatalf("got %d marks, want 2: %+v", len(marks), marks)
	}
	if marks[0].Status != api.LogOutput || marks[0].Offset != 0 || marks[0].Time != 1 {
		t.Errorf("mark 0 = %+v", marks[0])
	}
	if marks[1].Status != api.LogAPIInput || marks[1].Offset != 4 || marks[1].Time != 2 {
		t.Errorf("mark 1 = %+v", marks[1])
	}

	// A torn line (a partial append) must not discard the marks before it.
	f, err := os.OpenFile(st.MarkPath(sess, shell), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"s":"o","t":3,"i"`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	marks, err = st.ReadMarks(sess, shell)
	if err != nil {
		t.Fatalf("a torn trailing line must not fail the read: %v", err)
	}
	if len(marks) != 2 {
		t.Errorf("torn line changed the mark count: got %d, want 2", len(marks))
	}
}

// The session list is the directory tree, so a session manifest and its shell
// manifests are the only place the list lives.
func TestSessions_ManifestTreeRoundTrip(t *testing.T) {
	st := newStore(t)

	sess := api.Session{ID: "sess-1", Name: "demo", Status: api.SessionRunning}
	if err := st.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	// Shells are stored under the session directory and ordered by creation time,
	// because ":N" locators count in that order.
	first := api.Session{ID: "sh-a", Name: "first"}
	first.CreatedAt = 1000
	second := api.Session{ID: "sh-b", Name: "second"}
	second.CreatedAt = 2000
	if err := st.SaveShell("sess-1", first); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveShell("sess-1", second); err != nil {
		t.Fatal(err)
	}

	list, err := st.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions, want 1", len(list))
	}
	if list[0].ID != "sess-1" || list[0].Name != "demo" {
		t.Errorf("session = %+v", list[0])
	}
	if len(list[0].Shells) != 2 {
		t.Fatalf("got %d shells, want 2: %+v", len(list[0].Shells), list[0].Shells)
	}

	if got := list[0].Shells[0].ID; got != "sh-a" {
		t.Errorf("shell order = %s first, want sh-a (earliest creation)", got)
	}

	// The manifest must not nest shells inside a shell.
	for _, sh := range list[0].Shells {
		if len(sh.Shells) != 0 {
			t.Errorf("shell %s carries nested shells: %+v", sh.ID, sh.Shells)
		}
	}

	// Deleting a session removes the whole tree.
	if err := st.DeleteSession("sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.sessionDir("sess-1")); !os.IsNotExist(err) {
		t.Errorf("session directory still present after delete: %v", err)
	}
}

// A manifest written by the new code is plain JSON with the mark keys the design
// specifies, so an external reader can consume the index without this package.
func TestMarks_OnDiskShapeUsesShortKeys(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s4", "sh4"

	if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogAIInput, Time: 1758499205123, Offset: 200}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(st.MarkPath(sess, shell))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	if line != `{"s":"a","t":1758499205123,"i":200}` {
		t.Errorf("mark line = %s", line)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["d"]; ok {
		t.Error("mark carries a payload field: marks must index, not store")
	}
}

// Cached handles must not leak between sessions, and closing must flush them.
func TestLog_HandlesArePerShell(t *testing.T) {
	st := newStore(t)

	if _, err := st.AppendLog("sA", "sh1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog("sB", "sh1", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteShell("sA", "sh1"); err != nil {
		t.Fatal(err)
	}
	// The surviving shell's bytes must be unaffected.
	got, err := st.ReadLog("sB", "sh1", 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "b" {
		t.Errorf("shell in another session = %q, want %q", got, "b")
	}
	if _, err := os.Stat(filepath.Join(st.shellDir("sA", "sh1"))); !os.IsNotExist(err) {
		t.Error("deleted shell directory still present")
	}
}
