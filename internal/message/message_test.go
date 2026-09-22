package message

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	return NewManager(store)
}

// Appending output returns the offset it was written at, and those offsets tile
// the stream: each append starts where the previous one ended.
func TestAppendOutput_ReturnsTiledOffsets(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	var want []byte
	for i := 0; i < 5; i++ {
		chunk := []byte(fmt.Sprintf("chunk-%d;", i))
		off, err := mgr.AppendOutput(sess, shell, chunk)
		if err != nil {
			t.Fatal(err)
		}
		if off != int64(len(want)) {
			t.Fatalf("append %d returned offset %d, want %d", i, off, len(want))
		}
		want = append(want, chunk...)
	}

	got, total, err := mgr.OutputByteRange(sess, shell, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("stream = %q, want %q", got, want)
	}
	if total != int64(len(want)) {
		t.Errorf("total = %d, want %d", total, len(want))
	}
}

// A status mark is written once per transition, not once per append: the index
// must not grow a line per read chunk.
func TestAppendOutput_MarksOnlyOnStatusChange(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	for i := 0; i < 6; i++ {
		if _, err := mgr.AppendOutput(sess, shell, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.AppendMarkOnly(sess, shell, api.LogAIInput); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AppendMarkOnly(sess, shell, api.LogAIInput); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AppendOutput(sess, shell, []byte("y")); err != nil {
		t.Fatal(err)
	}

	marks, err := mgr.Marks(sess, shell)
	if err != nil {
		t.Fatal(err)
	}
	// Six output appends plus two identical input marks collapse to: output,
	// input, output.
	if len(marks) != 3 {
		t.Fatalf("got %d marks, want 3: %+v", len(marks), marks)
	}
	if marks[0].Status != api.LogOutput || marks[0].Offset != 0 {
		t.Errorf("mark 0 = %+v, want output at 0", marks[0])
	}
	if marks[1].Status != api.LogAIInput || marks[1].Offset != 6 {
		t.Errorf("mark 1 = %+v, want input at 6 (input carries no bytes)", marks[1])
	}
	if marks[2].Status != api.LogOutput || marks[2].Offset != 6 {
		t.Errorf("mark 2 = %+v, want output at 6", marks[2])
	}
}

// Input carries no bytes: the terminal echoes keystrokes into the output stream,
// so storing them again would duplicate every keystroke in the log.
func TestAppendMarkOnly_AddsNoBytes(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	if _, err := mgr.AppendOutput(sess, shell, []byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AppendMarkOnly(sess, shell, api.LogAPIInput); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AppendOutput(sess, shell, []byte("after")); err != nil {
		t.Fatal(err)
	}

	got, total, err := mgr.OutputByteRange(sess, shell, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "beforeafter" {
		t.Errorf("stream = %q, want %q (input must not add bytes)", got, "beforeafter")
	}
	if total != 11 {
		t.Errorf("total = %d, want 11", total)
	}
}

// Concurrent appends to one shell must not interleave their bytes: the log is a
// single ordered stream, so every byte lands exactly once and offsets stay tiled.
func TestAppendOutput_ConcurrentAppendsDoNotInterleave(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	const writers, each = 8, 40
	var wg sync.WaitGroup
	offsets := make(chan int64, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				off, err := mgr.AppendOutput(sess, shell, []byte(fmt.Sprintf("[w%d-%02d]", w, i)))
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				offsets <- off
			}
		}(w)
	}
	wg.Wait()
	close(offsets)

	got, total, err := mgr.OutputByteRange(sess, shell, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(got)) {
		t.Fatalf("total %d != payload %d", total, len(got))
	}
	// Every returned offset must be the start of a complete, well-formed token:
	// an interleaved write would have split one.
	for off := range offsets {
		if off < 0 || off >= int64(len(got)) {
			t.Fatalf("offset %d out of range (%d bytes)", off, len(got))
		}
		if got[off] != '[' {
			t.Fatalf("offset %d does not start a token: %q", off, got[off:off+8])
		}
		rest := got[off:]
		if end := bytes.IndexByte(rest, ']'); end < 0 {
			t.Fatalf("token at %d is unterminated", off)
		} else if strings.ContainsAny(string(rest[1:end]), "[]") {
			t.Fatalf("token at %d is interleaved: %q", off, rest[:end+1])
		}
	}
}

// A window read is positional: any offset, any length, independent of how the
// bytes were chunked on the way in.
func TestOutputByteRange_WindowsMatchTheStream(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	full := []byte(strings.Repeat("中文ABC", 300))
	for i := 0; i < len(full); i += 97 {
		end := i + 97
		if end > len(full) {
			end = len(full)
		}
		if _, err := mgr.AppendOutput(sess, shell, full[i:end]); err != nil {
			t.Fatal(err)
		}
	}

	n := int64(len(full))
	starts := []int64{0, 1, 2, 3, 4, 5, 6, 100, 101, 102}
	// Offsets near a 4096-byte read boundary, when the stream is long enough to
	// have one, since that is where a character is most likely to be split.
	for _, s := range []int64{4093, 4094, 4095, 4096, 4097} {
		if s < n {
			starts = append(starts, s)
		}
	}
	starts = append(starts, n-1, n, n+5)

	for _, start := range starts {
		for _, max := range []int{1, 5, 64, 4096} {
			got, total, err := mgr.OutputByteRange(sess, shell, start, max)
			if err != nil {
				t.Fatal(err)
			}
			if total != n {
				t.Fatalf("total = %d, want %d", total, n)
			}
			end := start + int64(max)
			if start > n {
				start = n // a read past the end is clamped, not rejected
			}
			if end > total {
				end = total
			}
			if !bytes.Equal(got, full[start:end]) {
				t.Fatalf("window start=%d max=%d = %q, want %q", start, max, got, full[start:end])
			}
		}
	}
}

// A zero-length log is a valid answer, not an error, and a read past the end is
// empty while still reporting the true total.
func TestOutputByteRange_EmptyAndPastEnd(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	got, total, err := mgr.OutputByteRange(sess, shell, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || total != 0 {
		t.Fatalf("empty log: got %q total %d, want empty and 0", got, total)
	}

	if _, err := mgr.AppendOutput(sess, shell, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	got, total, err = mgr.OutputByteRange(sess, shell, 99, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("read past end returned %q, want empty", got)
	}
	if total != 3 {
		t.Errorf("read past end reported total %d, want 3", total)
	}
}

// ForgetSession drops in-memory state but leaves the bytes: forgetting is not
// deleting.
func TestForgetSession_KeepsBytesOnDisk(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	if _, err := mgr.AppendOutput(sess, shell, []byte("keepme")); err != nil {
		t.Fatal(err)
	}
	mgr.ForgetSession(sess)

	got, _, err := mgr.OutputByteRange(sess, shell, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keepme" {
		t.Errorf("after ForgetSession stream = %q, want %q", got, "keepme")
	}
}

// Forgetting a session must also drop its remembered status, or a later append
// would skip writing a mark because it looks like a repeat of a stale status.
func TestForgetSession_ResetsRememberedStatus(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	if err := mgr.AppendMarkOnly(sess, shell, api.LogAIInput); err != nil {
		t.Fatal(err)
	}
	mgr.ForgetSession(sess)
	if err := mgr.AppendMarkOnly(sess, shell, api.LogAIInput); err != nil {
		t.Fatal(err)
	}

	marks, err := mgr.Marks(sess, shell)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 2 {
		t.Fatalf("got %d marks, want 2 (the status cache must not survive ForgetSession)", len(marks))
	}
}
