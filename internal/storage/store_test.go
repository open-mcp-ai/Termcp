package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// A windowed read answers with the spans that decide the window's bytes: the span
// the window starts inside, the marks that start within it, and the mark that
// closes the last overlap. That last one is what lets the caller derive every
// span's end without the window having to say where it came from.
func TestMarks_WindowIsTheSpansDecidingIt(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s7", "sh7"

	// Marks at 0, 100, 200, 300 over a log of 400 bytes.
	for _, off := range []int64{0, 100, 200, 300} {
		if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: off, Offset: off}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AppendLog(sess, shell, make([]byte, 400)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		start, end int64
		want       []int64
		wantCloser int64
	}{
		// The window starts inside span [0,100): that span's mark is the carry, 100
		// starts inside the window, and 200 is the boundary the last of them ends at —
		// returned as the closer rather than as a span, since its own span does not
		// overlap the window.
		{"middle of a span", 50, 150, []int64{0, 100}, 200},
		// A mark exactly on the window's start is the carry, not a member.
		{"start on a mark", 100, 150, []int64{100}, 200},
		// A mark exactly on the end is the closer: it is where the window's last span
		// ends, and it does not itself belong to the window.
		{"end on a mark", 100, 200, []int64{100}, 200},
		// The whole log: every span, and no closer (there is nothing past it).
		{"whole log", 0, 400, []int64{0, 100, 200, 300}, 0},
		// A window past the last mark still gets that mark carried in, so a row of
		// bytes written after it is coloured — and no closer, the log ends there.
		{"past the last mark", 350, 400, []int64{300}, 0},
		// An empty window is legal and answered: the mark on its start byte is what
		// a zero-length span sits on, and the next mark closes it.
		{"empty window", 200, 200, []int64{200}, 300},
		// A window before the first mark is the mark on its start byte and the one
		// that closes it.
		{"before the first mark", 0, 50, []int64{0}, 100},
	}
	for _, tc := range cases {
		marks, closer, err := st.ReadMarksWindow(sess, shell, tc.start, tc.end)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got []int64
		for _, m := range marks {
			got = append(got, m.Offset)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: offsets %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: offsets %v, want %v", tc.name, got, tc.want)
				break
			}
		}
		if closer != tc.wantCloser {
			t.Errorf("%s: closer %d, want %d", tc.name, closer, tc.wantCloser)
		}
	}
}

// Marks sharing the window's start offset are all kept. A submitted line and the
// review decision about it are both zero-length at the same byte, so a carry that
// kept only the newest would drop the row's own status and leave the row reading
// as the decision that was made about it.
func TestMarks_WindowKeepsEveryMarkAtTheStartOffset(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s8", "sh8"

	for _, m := range []api.LogMark{
		{Status: api.LogOutput, Time: 1, Offset: 0},
		{Status: api.LogAPIInput, Time: 2, Offset: 40},
		{Status: api.LogApprovalRequest, Time: 3, Offset: 40},
		{Status: api.LogOutput, Time: 4, Offset: 40},
		{Status: api.LogOutput, Time: 5, Offset: 90},
	} {
		if err := st.AppendMark(sess, shell, m); err != nil {
			t.Fatal(err)
		}
	}

	marks, closer, err := st.ReadMarksWindow(sess, shell, 60, 95)
	if err != nil {
		t.Fatal(err)
	}
	// The carry is the whole offset-40 group; 90 starts inside the window, so it is
	// a span of it too, and there is no closer above the window.
	want := []struct {
		status api.LogStatus
		offset int64
	}{
		{api.LogAPIInput, 40},
		{api.LogApprovalRequest, 40},
		{api.LogOutput, 40},
		{api.LogOutput, 90},
	}
	if len(marks) != len(want) {
		t.Fatalf("got %d marks, want %d: %+v", len(marks), len(want), marks)
	}
	for i, w := range want {
		if marks[i].Status != w.status || marks[i].Offset != w.offset {
			t.Errorf("marks[%d] = %+v, want %s@%d", i, marks[i], w.status, w.offset)
		}
	}
	if closer != 0 {
		t.Errorf("closer = %d, want 0: no mark sits at or past the window's end", closer)
	}
}

// The sparse index must not change what a window answers: the same index backs a
// read that lands mid-stride as one that lands exactly on an entry, and a log long
// enough to have many entries exercises both. A mark count just past a stride
// boundary is where an off-by-one in the seek would show up as a missing carry.
func TestMarks_WindowAnswersTheSameAtEveryOffset(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s9", "sh9"

	// 200 marks, so the index has several entries and a read of any single-mark
	// window has to seek to the group holding it.
	const n = 200
	for i := 0; i < n; i++ {
		off := int64(i * 10)
		if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: off, Offset: off}); err != nil {
			t.Fatal(err)
		}
	}
	total := int64(n * 10)
	if _, err := st.AppendLog(sess, shell, make([]byte, total)); err != nil {
		t.Fatal(err)
	}

	// The full read is the reference: a one-span window at mark i must agree with
	// it about that span and the next one, wherever the seek landed.
	all, err := st.ReadMarks(sess, shell)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("full read returned %d marks, want %d", len(all), n)
	}
	for i := 0; i < n; i++ {
		start := int64(i * 10)
		marks, closer, err := st.ReadMarksWindow(sess, shell, start, start+1)
		if err != nil {
			t.Fatalf("window at %d: %v", start, err)
		}
		// A one-byte window holds exactly the span over that byte, and the next mark
		// is the boundary it ends at. The last mark has no successor to report.
		want := 1
		wantCloser := start + 10
		if i == n-1 {
			wantCloser = 0
		}
		if len(marks) != want {
			t.Fatalf("window at %d returned %d marks, want %d: %+v", start, len(marks), want, marks)
		}
		if marks[0] != all[i] {
			t.Errorf("window at %d returned %+v, want %+v", start, marks[0], all[i])
		}
		if closer != wantCloser {
			t.Errorf("window at %d: closer %d, want %d", start, closer, wantCloser)
		}
	}
}

// The sparse index is extended by what was appended, so a window read after new
// marks arrive sees them without re-reading the file from its start.
func TestMarks_WindowSeesAppendedMarks(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s10", "sh10"

	for i := 0; i < 70; i++ {
		if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: int64(i), Offset: int64(i * 10)}); err != nil {
			t.Fatal(err)
		}
	}
	// First read builds the index up to here.
	if _, _, err := st.ReadMarksWindow(sess, shell, 0, 10); err != nil {
		t.Fatal(err)
	}
	// Marks written after that read must be found by the next one: the index
	// consumes the appended bytes, not the file again.
	if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogAIInput, Time: 800, Offset: 710}); err != nil {
		t.Fatal(err)
	}

	marks, _, err := st.ReadMarksWindow(sess, shell, 700, 720)
	if err != nil {
		t.Fatal(err)
	}
	// The carry is the last mark below the window (690), then the new mark itself:
	// a window read after an append sees it without re-reading the file.
	if len(marks) != 2 || marks[0].Offset != 690 || marks[1].Status != api.LogAIInput || marks[1].Offset != 710 {
		t.Fatalf("appended marks are not in the window: %+v", marks)
	}
}

// A malformed line is skipped by the index exactly as a read skips it, and it is
// not counted either: the index and the read must agree on which lines are marks,
// or a window would seek by a count of lines that is not the file's.
func TestMarks_WindowSkipsMalformedAndTornLines(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s12", "sh12"

	for i := 0; i < 70; i++ {
		if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: int64(i), Offset: int64(i * 10)}); err != nil {
			t.Fatal(err)
		}
	}
	// A torn append that a later write closes into an invalid line: a crash between
	// the bytes and the newline leaves exactly this.
	f, err := os.OpenFile(st.MarkPath(sess, shell), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"s\":\"o\",\"t\":999,\"i\":70\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogAIInput, Time: 800, Offset: 710}); err != nil {
		t.Fatal(err)
	}

	marks, _, err := st.ReadMarksWindow(sess, shell, 700, 720)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 2 || marks[0].Offset != 690 || marks[1].Status != api.LogAIInput {
		t.Fatalf("a malformed line derailed the window: %+v", marks)
	}

	// A torn line at the very end is not a mark: the index stops before it, and a
	// window either side of it still answers from the marks that are complete.
	f, err = os.OpenFile(st.MarkPath(sess, shell), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"s":"o","t":1000,"i"`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := st.ReadMarksWindow(sess, shell, 710, 711); err != nil {
		t.Fatalf("a torn trailing line must not fail the read: %v", err)
	}
	marks, _, err = st.ReadMarksWindow(sess, shell, 700, 720)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 2 || marks[1].Offset != 710 {
		t.Fatalf("a torn trailing line changed the window: %+v", marks)
	}
}

// Deleting a shell drops its index, so a new shell that reuses the id is not
// answered from positions in a log that no longer exists.
func TestMarks_WindowIndexDiesWithTheShell(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s11", "sh11"

	for i := 0; i < 80; i++ {
		if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: int64(i), Offset: int64(i * 100)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ReadMarks(sess, shell); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteShell(sess, shell); err != nil {
		t.Fatal(err)
	}

	// The same id, a much shorter file: a stale index would seek past its end and
	// come back empty.
	if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: 1, Offset: 0}); err != nil {
		t.Fatal(err)
	}
	marks, closer, err := st.ReadMarksWindow(sess, shell, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 1 || marks[0].Offset != 0 {
		t.Fatalf("a reused shell id was answered from the deleted shell's index: %+v", marks)
	}
	if closer != 0 {
		t.Errorf("closer = %d, want 0: the only mark is the last one", closer)
	}
}

// An offset is only useful if it is where the bytes actually landed, and that
// holds only if the seek that learns the offset and the write that consumes it
// are one indivisible step. They are now performed by one goroutine; if that ever
// regresses into "find the offset, then write" as two separately synchronized
// steps, two concurrent appends are handed the same offset and the second one
// overwrites the first. Nothing about the resulting file looks wrong - it is the
// size it should be - so the loss is only visible through the offsets.
//
// This is why the test reads back at the returned offsets instead of counting
// bytes: counting would pass even while offsets were being handed out twice.
func TestAppendLog_ConcurrentOffsetsMatchWhereBytesLanded(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"

	// Each writer appends a payload it can recognise, so a byte found at the wrong
	// offset is identifiable as "written by someone else" rather than just "wrong".
	const writers, perWriter, chunk = 8, 40, 64

	var wg sync.WaitGroup
	offsets := make([][]int64, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('A' + w)}, chunk)
			for i := 0; i < perWriter; i++ {
				off, err := st.AppendLog(sess, shell, payload)
				if err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
				offsets[w] = append(offsets[w], off)
			}
		}(w)
	}
	wg.Wait()

	total, err := st.LogSize(sess, shell)
	if err != nil {
		t.Fatal(err)
	}
	const want = writers * perWriter * chunk
	if total != want {
		t.Fatalf("log is %d bytes, want %d: an append overwrote another", total, want)
	}

	// Every offset must be distinct, aligned, and hold that writer's own bytes.
	seen := make(map[int64]int)
	for w := 0; w < writers; w++ {
		for _, off := range offsets[w] {
			if other, dup := seen[off]; dup {
				t.Fatalf("offset %d was handed to writer %d and writer %d", off, other, w)
			}
			seen[off] = w

			if off%chunk != 0 {
				t.Errorf("writer %d got misaligned offset %d", w, off)
			}
			got, err := st.ReadLog(sess, shell, off, chunk)
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat([]byte{byte('A' + w)}, chunk)
			if !bytes.Equal(got, want) {
				t.Fatalf("writer %d: bytes at its offset %d belong to another writer", w, off)
			}
		}
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("saw %d distinct offsets, want %d", len(seen), writers*perWriter)
	}
}

// A shell's handle cache is replaced by an owner goroutine, so the operations that
// used to be three separately locked helpers must still be safe against each
// other. A close that ran concurrently with an append used to be serialized by the
// handle mutex; it now queues behind the append on the owner's channel, and the
// append that follows a close must still succeed because Close releases handles
// rather than retiring the store.
func TestAppendLog_CloseAndDeleteRaceAppends(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := st.AppendLog(sess, shell, bytes.Repeat([]byte("x"), 128)); err != nil {
				// Close/DeleteShell concurrent with an append must not surface an
				// error from a handle that was closed underneath it.
				t.Errorf("append %d: %v", i, err)
				return
			}
		}
	}()

	for i := 0; i < 20; i++ {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	// The handle cache must be rebuilt on demand, not latched shut.
	if _, err := st.AppendLog(sess, shell, []byte("after close")); err != nil {
		t.Fatalf("append after Close: %v", err)
	}

	close(stop)
	<-done
}

// Deleting a shell closes its cached handle through the owner, so a later append
// must reopen rather than write through a closed file descriptor.
func TestAppendLog_DeleteShellThenAppendReopens(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"

	if _, err := st.AppendLog(sess, shell, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteShell(sess, shell); err != nil {
		t.Fatal(err)
	}
	off, err := st.AppendLog(sess, shell, []byte("second"))
	if err != nil {
		t.Fatalf("append after DeleteShell: %v", err)
	}
	if off != 0 {
		t.Fatalf("recreated log should start at 0, got %d", off)
	}
	got, err := st.ReadLog(sess, shell, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("got %q, want %q", got, "second")
	}
}

// Closing a session is a delete, and a delete has to stay deleted. A writer that had
// not yet noticed (the transcript loop can be draining when the delete lands) used to
// have its next append recreate the session directory through MkdirAll - without the
// manifest LoadSessions needs, so the directory was invisible to the loader and could
// never be cleaned up. Refusing the append keeps the delete final and tells the writer
// to stop, which is what it does on any append error.
func TestDeleteSession_LateAppendDoesNotResurrectTheDirectory(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"

	if err := st.SaveSession(api.Session{ID: sess}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog(sess, shell, []byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.sessionDir(sess)); !os.IsNotExist(err) {
		t.Fatalf("session dir still present after delete: %v", err)
	}

	if _, err := st.AppendLog(sess, shell, []byte("late")); err == nil {
		t.Error("late append succeeded; expected refusal")
	}
	if _, err := os.Stat(st.sessionDir(sess)); err == nil {
		t.Error("ORPHAN: late append recreated the deleted session directory")
	}
}

// The other half of the rule above, and the reason it cannot simply refuse whenever
// the directory is missing: a session starts its output pipes in New, before Create
// persists it, so the first bytes of a session's life arrive before its directory
// exists. Those bytes must be written. Refusing them would drop real output to fix a
// delete-path bug.
func TestAppendLog_BeforeFirstPersistIsAllowed(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"

	off, err := st.AppendLog(sess, shell, []byte("first bytes"))
	if err != nil {
		t.Fatalf("first append to an unpersisted session must be accepted: %v", err)
	}
	if off != 0 {
		t.Errorf("offset = %d, want 0", off)
	}
	if err := st.AppendMark(sess, shell, api.LogMark{Status: api.LogOutput, Time: 1, Offset: 0}); err != nil {
		t.Fatalf("mark on an unpersisted session must be accepted: %v", err)
	}
}

// An id reused after a delete must be writable again.
func TestDeleteSession_ReusedIDIsWritableAgain(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"

	if err := st.SaveSession(api.Session{ID: sess}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog(sess, shell, []byte("late")); err == nil {
		t.Error("append after delete should be refused")
	}
	// A new session claiming the same id.
	if err := st.SaveSession(api.Session{ID: sess}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendLog(sess, shell, []byte("fresh")); err != nil {
		t.Fatalf("append after re-creating the session id must be accepted: %v", err)
	}
}

// writeManifest skips a write whose encoded bytes match the last one it wrote, which
// is what makes a whole-table sweep cost only the sessions that changed. The check is
// on the observable consequence rather than the cache: an unchanged manifest is not
// rewritten, so its mtime does not move. A rewrite of identical bytes would need a
// new temp file and rename, so a stable mtime means no write happened.
func TestSaveSession_UnchangedManifestIsNotRewritten(t *testing.T) {
	st := newStore(t)
	sess := api.Session{ID: "s1", Name: "n", Status: api.SessionRunning, CreatedAt: 1, UpdatedAt: 2}
	if err := st.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.sessionDir(sess.ID), "manifest.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Same content, repeatedly.
	for i := 0; i < 5; i++ {
		time.Sleep(10 * time.Millisecond)
		if err := st.SaveSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("manifest was rewritten despite identical content (mtime %v -> %v)",
			before.ModTime(), after.ModTime())
	}

	// A real change must still be written.
	sess.Name = "renamed"
	if err := st.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st2.ModTime().Equal(before.ModTime()) {
		t.Error("a changed manifest was NOT written")
	}
	got, err := st.readManifest(st.sessionDir(sess.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" {
		t.Errorf("on-disk name = %q, want renamed", got.Name)
	}
}

// The dangerous case: a manifest is removed from disk while its content hash is
// still cached. Skipping the next write would leave a directory with no manifest,
// which LoadSessions then ignores forever.
func TestSaveSession_DeletedManifestIsRewrittenNotSkipped(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"
	session := api.Session{ID: sess, Name: "n", Status: api.SessionRunning, CreatedAt: 1}
	sh := api.Session{ID: shell, Status: api.SessionRunning, CreatedAt: 1}

	if err := st.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveShell(sess, sh); err != nil {
		t.Fatal(err)
	}

	// Delete the whole session (drops hashes), then reuse the id with identical
	// content: the write must happen.
	if err := st.DeleteSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(st.sessionDir(sess), "manifest.json")); err != nil {
		t.Fatalf("ORPHAN: manifest not written after delete+recreate: %v", err)
	}
	loaded, err := st.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != sess {
		t.Errorf("LoadSessions saw %d sessions, want the recreated one", len(loaded))
	}
}

// DeleteShell removes one shell's manifest while the session hash stays cached;
// the shell's own hash must go too.
func TestSaveShell_DeletedManifestIsRewritten(t *testing.T) {
	st := newStore(t)
	const sess, shell = "s1", "sh1"
	if err := st.SaveSession(api.Session{ID: sess}); err != nil {
		t.Fatal(err)
	}
	sh := api.Session{ID: shell, Status: api.SessionRunning, CreatedAt: 1}
	if err := st.SaveShell(sess, sh); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteShell(sess, shell); err != nil {
		t.Fatal(err)
	}
	// Same shell content again.
	if err := st.SaveShell(sess, sh); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.shellDir(sess, shell), "manifest.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ORPHAN: shell manifest not rewritten after delete: %v", err)
	}
}
