package message

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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

// The per-session writer is what makes simultaneous appends and a ForgetSession
// safe. Forgetting a session used to drop its lock from a map, so a writer that
// loaded the lock before the drop and one that loaded it after held two
// different locks for the same log: the byte write and the mark that describes
// it could then be interleaved by the second writer, and a mark could be
// appended at an offset read before the first writer's bytes landed.
//
// Losing in-memory state must not lose or reorder accepted writes, which is what
// this checks: every append that returned an offset before ForgetSession came
// back must be readable at that offset afterwards, with its token intact.
func TestAppendOutput_SurvivesConcurrentForgetSession(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	// Appenders keep writing while the session is forgotten underneath them.
	const writers, each = 6, 30
	var wg sync.WaitGroup
	type span struct {
		off   int64
		token string
	}
	results := make(chan span, writers*each)
	var forgot sync.WaitGroup
	forgot.Add(1)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				token := fmt.Sprintf("<%d.%02d>", w, i)
				off, err := mgr.AppendOutput(sess, shell, []byte(token))
				if err != nil {
					// Refusing after the session was forgotten is allowed: the
					// caller stops rather than writing to a released session.
					// What is not allowed is a silent wrong offset.
					return
				}
				results <- span{off, token}
			}
		}(w)
	}

	// Forget once, mid-flight, once the appenders are running.
	go func() {
		defer forgot.Done()
		time.Sleep(2 * time.Millisecond)
		mgr.ForgetSession(sess)
	}()

	forgot.Wait()
	wg.Wait()
	close(results)

	var accepted []span
	for s := range results {
		accepted = append(accepted, s)
	}
	if len(accepted) == 0 {
		t.Fatal("no append was accepted")
	}

	// Every accepted append must be at its offset, as a complete token.
	got, total, err := mgr.OutputByteRange(sess, shell, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(got)) {
		t.Fatalf("log size %d disagrees with the bytes read (%d)", total, len(got))
	}
	seen := map[string]bool{}
	for _, s := range accepted {
		if s.off < 0 || s.off+int64(len(s.token)) > int64(len(got)) {
			t.Fatalf("offset %d for %q is outside the %d-byte log", s.off, s.token, len(got))
		}
		if actual := string(got[s.off : s.off+int64(len(s.token))]); actual != s.token {
			t.Fatalf("offset %d holds %q, want %q: an accepted append was not written at the offset it reported", s.off, actual, s.token)
		}
		seen[s.token] = true
	}
	if len(seen) != len(accepted) {
		t.Fatalf("offsets are not unique: %d accepts produced %d distinct positions", len(accepted), len(seen))
	}

	// And the log is a clean concatenation of accepted tokens: an interleaved
	// write would leave a fragment no writer produced.
	for pos := 0; pos < len(got); {
		end := bytes.IndexByte(got[pos:], '>')
		if end < 0 {
			t.Fatalf("byte %d starts a token that never ends: %q", pos, got[pos:])
		}
		tok := string(got[pos : pos+end+1])
		if !seen[tok] {
			t.Fatalf("byte %d holds %q, which no append reported writing there", pos, tok)
		}
		pos += end + 1
	}
}

// ForgetSession must be safe to call repeatedly and on a session that was never
// used: it is a teardown hook, and teardown runs on paths where nothing was
// necessarily written.
func TestForgetSession_IdempotentAndSafeWhenUnused(t *testing.T) {
	mgr := newManager(t)

	mgr.ForgetSession("never-used") // must not panic or block
	mgr.ForgetSession("never-used")

	if _, err := mgr.AppendOutput("s1", "sh1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	mgr.ForgetSession("s1")
	mgr.ForgetSession("s1") // second call must not block waiting for a joined writer
}

// A mark-only append and an output append to the same shell are serialized, so a
// zero-byte input mark cannot cover output bytes that were appended in between.
// The writer goroutine performs both halves of each append, which is what makes
// the offset a mark records true at the moment it is written.
func TestAppendMarkOnly_OffsetMatchesTheLogAtThatMoment(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	if _, err := mgr.AppendOutput(sess, shell, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AppendMarkOnly(sess, shell, api.LogAPIInput); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AppendOutput(sess, shell, []byte("defg")); err != nil {
		t.Fatal(err)
	}

	marks, err := mgr.Marks(sess, shell)
	if err != nil {
		t.Fatal(err)
	}
	// The input mark must point at offset 3 - the end of the log when it was
	// written - and not at 0 or 7.
	var input *api.LogMark
	for i := range marks {
		if marks[i].Status == api.LogAPIInput {
			input = &marks[i]
		}
	}
	if input == nil {
		t.Fatalf("no input mark was written: %+v", marks)
	}
	if input.Offset != 3 {
		t.Errorf("input mark offset = %d, want 3 (the log size when it was written)", input.Offset)
	}
}

// The writer is created per session and runs until stopped, so the two failure
// modes of that design are a deadlock and a leak. Forgetting a session must join
// its writer (or the goroutine, and the entry it owns, outlive the session), and
// the shutdown path must be able to complete while appends are arriving (or the
// process stops making progress).
//
// These are checked here rather than left to review because the cost of getting
// either wrong is not a wrong byte - it is a process that stops making progress,
// or one that accumulates a goroutine per session for its lifetime.

// TestWriter_ConcurrentAppendsAndForgetsDoNotDeadlock drives concurrent first-use
// creation alongside repeated ForgetSession calls. A retired writer answers what is
// already queued and then exits, so a caller waiting on a reply always gets one -
// either the result or ErrSessionForgotten - and neither side can wait forever.
func TestWriter_ConcurrentAppendsAndForgetsDoNotDeadlock(t *testing.T) {
	mgr := newManager(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					_, _ = mgr.AppendOutput("s1", "sh1", []byte("x"))
					_ = mgr.AppendMarkOnly("s1", "sh1", api.LogAIInput)
				}
			}()
		}
		for i := 0; i < 30; i++ {
			mgr.ForgetSession("s1")
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: a writer channel was never released")
	}
}

// TestWriter_GoroutineReleasedOnForget pins that ForgetSession joins the writer
// instead of abandoning it, so a long-lived process does not accumulate one
// goroutine per session it has ever deleted.
func TestWriter_GoroutineReleasedOnForget(t *testing.T) {
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	mgr := NewManager(store)

	base := runtime.NumGoroutine()
	const sessions = 100
	for i := 0; i < sessions; i++ {
		if _, err := mgr.AppendOutput(sessName(i), "sh1", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if peak := runtime.NumGoroutine() - base; peak < sessions/2 {
		t.Fatalf("expected roughly %d writer goroutines, saw %d", sessions, peak)
	}

	for i := 0; i < sessions; i++ {
		mgr.ForgetSession(sessName(i))
	}
	// Exiting a goroutine is asynchronous; give the scheduler a chance to run them.
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine()-base <= 2 {
			break
		}
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - base; delta > 2 {
		t.Errorf("leak: %d goroutines still alive after forgetting every session", delta)
	}
}

// sessName returns a distinct session id per index, so the leak test creates
// genuinely separate writers rather than reusing one.
func sessName(i int) string {
	return fmt.Sprintf("sess-%03d", i)
}

// The one-writer-per-session invariant is maintained by an agreement between two
// operations: creation stores an entry only where none exists, and the writer's own
// goroutine removes its entry as its last act. The second half is what makes the
// design immune to call ordering - there is no "delete the entry" call for a caller
// to get wrong - so it is worth pinning directly rather than inferring it from
// other tests passing.
//
// What must hold: after ForgetSession returns, the entry is gone (otherwise a later
// append would reuse a writer whose goroutine has exited, and every append would
// block forever waiting for a reply that can no longer be produced).
func TestWriter_ForgetRemovesTheEntryItsGoroutineOwned(t *testing.T) {
	mgr := newManager(t)
	const sess = "s1"

	if _, err := mgr.AppendOutput(sess, "sh1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.sessions.Load(sess); !ok {
		t.Fatal("expected an entry while the writer is running")
	}

	mgr.ForgetSession(sess)

	if v, ok := mgr.sessions.Load(sess); ok {
		t.Fatalf("entry survived ForgetSession: %T", v)
	}
}

// A writer that has exited must not be reachable, because a request sent to a
// writer whose goroutine is gone is never answered. This is the failure the
// invariant above prevents, so it is asserted through the public API: appends after
// a forget must still complete rather than hang.
func TestWriter_AppendAfterForgetCompletes(t *testing.T) {
	mgr := newManager(t)
	const sess, shell = "s1", "sh1"

	for i := 0; i < 10; i++ {
		mgr.ForgetSession(sess)
		done := make(chan error, 1)
		go func() {
			_, err := mgr.AppendOutput(sess, shell, []byte("y"))
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: append after forget never returned", i)
		}
	}
}
