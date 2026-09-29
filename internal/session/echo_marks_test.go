package session

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// span is a mark with the byte range a reader derives from it: a stored mark
// carries only its start offset, and its end is where the next mark begins (the
// last one ends at the current log size). This is the shape both the REST marks
// endpoint and the timeline rail work with.
type span struct {
	status api.LogStatus
	start  int64
	end    int64
}

func (s span) bytes() int64 { return s.end - s.start }

// spans turns a shell's mark list into the byte spans every reader of the index
// actually sees.
func spans(t *testing.T, mm *message.Manager, sessionID, shellID string) []span {
	t.Helper()
	marks, err := mm.Marks(sessionID, shellID)
	if err != nil {
		t.Fatal(err)
	}
	total, err := mm.OutputSize(sessionID, shellID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]span, 0, len(marks))
	for i, m := range marks {
		end := total
		if i+1 < len(marks) {
			end = marks[i+1].Offset
		}
		if end < m.Offset {
			end = m.Offset
		}
		out = append(out, span{status: m.Status, start: m.Offset, end: end})
	}
	return out
}

func countSpansWithStatus(marks []span, status api.LogStatus) int {
	n := 0
	for _, m := range marks {
		if m.status == status {
			n++
		}
	}
	return n
}

// waitForIdleBytes waits until the log's size has stopped changing, which is the
// only reliable way to know a shell has finished answering: the terminal echoes
// when it likes, so a fixed sleep races the very bytes under test.
func waitForIdleBytes(t *testing.T, mm *message.Manager, sessionID, shellID string) int64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last int64 = -1
	stable := 0
	for time.Now().Before(deadline) {
		size, err := mm.OutputSize(sessionID, shellID)
		if err != nil {
			t.Fatal(err)
		}
		if size == last && size > 0 {
			stable++
			if stable >= 5 {
				return size
			}
		} else {
			stable = 0
		}
		last = size
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// TestUnsubmittedTypingWritesNoMark pins the rule a partial line has to obey:
// **nothing is recorded in the log until the line is submitted**.
//
// The bug this covers was reported from a live session: typing four characters
// (and backspacing them) put an `i` span on the timeline before enter was ever
// pressed, so the rail showed "input" for a command still being composed. Worse,
// the span captured every byte written while the line was open, so an output
// span that overlapped the typing was swallowed into it and drawn far shorter
// than the bytes it actually held.
func TestUnsubmittedTypingWritesNoMark(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	before := spans(t, mm, s.ID, cs.ID)
	if countSpansWithStatus(before, api.LogAPIInput) != 0 {
		t.Fatalf("a fresh shell already has input marks: %+v", before)
	}

	// Type, and backspace over it, without ever pressing enter — the exact
	// sequence from the report.
	for _, ch := range []string{"s", "d", "f", "a", "f"} {
		if err := cs.SendTerminalBytes([]byte(ch), false); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		if err := cs.PressKey("backspace", 1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	after := spans(t, mm, s.ID, cs.ID)
	if got := countSpansWithStatus(after, api.LogAPIInput); got != 0 {
		t.Fatalf("typing without enter produced %d input spans, want 0: %+v", got, after)
	}
	// The echoed keystrokes stay output: they are what the terminal showed, and
	// no input was ever submitted.
	if countSpansWithStatus(after, api.LogOutput) == 0 {
		t.Fatalf("the echoed keystrokes vanished from the log: %+v", after)
	}
}

// TestSubmittedLineBecomesOneInputSpan pins the other half: pressing enter records
// the line exactly once, as a point at the moment the input ended.
//
// One mark per submitted line is what lets the log's de-duplication collapse a
// whole line into a single span; a mark per keystroke is what made the rail draw
// one bar per character. The mark is zero-length on purpose: the bytes it stands
// for are the *echo* of what was typed, and an echo is the terminal repeating the
// screen, so attributing it to the input would mean claiming bytes that were
// already output — which is how an output span gets swallowed and drawn short.
func TestSubmittedLineBecomesOneInputSpan(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	marker := "TC_SUBMIT_MARKER"
	// Type the command without submitting, then submit with enter — the normal
	// way a person runs a command, and the sequence the report came from.
	if err := cs.SendTerminalBytes([]byte(testInteractiveOutputCommand(marker)), false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := cs.PressKey("enter", 1); err != nil {
		t.Fatal(err)
	}

	// Wait for the command's own output (its second appearance: the first is the
	// echo of what was typed).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, _, err := cs.OutputByteRange(0, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(out), marker) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	marks := spans(t, mm, s.ID, cs.ID)
	if got := countSpansWithStatus(marks, api.LogAPIInput); got != 1 {
		t.Fatalf("one submitted line produced %d input spans, want 1: %+v", got, marks)
	}
	var in *span
	for i := range marks {
		if marks[i].status == api.LogAPIInput {
			in = &marks[i]
			break
		}
	}
	if in == nil {
		t.Fatal("no input span found")
	}
	// The mark is the moment enter was pressed, not a range of bytes: the echo of
	// what was typed is already in the log as output, and re-labelling it would
	// shorten the output span that holds it.
	if in.bytes() != 0 {
		t.Errorf("the submitted line's mark covers %d bytes, want a zero-length point: %+v", in.bytes(), marks)
	}
	// The command's own output is a separate, non-empty span after it.
	last := marks[len(marks)-1]
	if last.status != api.LogOutput {
		t.Fatalf("the last span is %q, want output: %+v", last.status, marks)
	}
	if last.bytes() == 0 {
		t.Fatalf("the command's output span is empty: %+v", marks)
	}
	// Every byte of the log belongs to a span: no gaps, no overlap.
	var cursor int64
	for _, m := range marks {
		if m.start != cursor {
			t.Errorf("span %q starts at %d, want %d (the spans must tile the log): %+v", m.status, m.start, cursor, marks)
		}
		cursor = m.end
	}
}

// TestInputMarkDoesNotSwallowEarlierOutput pins the ordering the report exposed:
// output produced *before* the typing must not be relabelled as input.
//
// The bug was an input span that began where typing began, so any output already
// in flight when a person started typing was captured into it, and the output
// span before it was drawn shorter than its bytes.
func TestInputMarkDoesNotSwallowEarlierOutput(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	// A command that prints a known amount of output, fully submitted and finished.
	beforeMarker := "TC_OUTPUT_BEFORE_TYPING"
	if err := cs.SendTerminalBytes([]byte(testShellInput(testInteractiveOutputCommand(beforeMarker))), false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, _, err := cs.OutputByteRange(0, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(out), beforeMarker) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	marksBefore := spans(t, mm, s.ID, cs.ID)
	sizeBefore := marksBefore[len(marksBefore)-1].end
	outSpanBefore := marksBefore[len(marksBefore)-1]
	if outSpanBefore.status != api.LogOutput {
		t.Fatalf("expected the settled state to end in output: %+v", marksBefore)
	}

	// Now type without submitting. The output span above must keep its bytes.
	for _, ch := range []string{"x", "y", "z"} {
		if err := cs.SendTerminalBytes([]byte(ch), false); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	marksAfter := spans(t, mm, s.ID, cs.ID)
	if got := countSpansWithStatus(marksAfter, api.LogAPIInput); got != 1 {
		t.Fatalf("the log should hold only the earlier submitted line's input span, got %d: %+v", got, marksAfter)
	}
	// The input span that existed before must not have grown into the new typing,
	// and no new input span may appear: the typing was never submitted.
	var inputEnd int64 = -1
	for _, m := range marksAfter {
		if m.status == api.LogAPIInput {
			inputEnd = m.end
		}
	}
	if inputEnd > sizeBefore {
		t.Errorf("an input span grew past the bytes that existed when the line was submitted (%d > %d): %+v",
			inputEnd, sizeBefore, marksAfter)
	}
}

// TestPipeShellKeepsOutputAsOutput guards the rule for a shell with no terminal:
// nothing echoes, so every byte it produces is its own output and no input span
// appears for a partial line.
func TestPipeShellKeepsOutputAsOutput(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})

	// cat/cmd with no tty: it echoes nothing back, it only prints what it is given.
	s, err := mgr.Create(testConfig(testPipeCommand(), nil, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}

	// A line with no submit: the program still prints it, and that is output.
	if err := cs.SendTerminalBytes([]byte("TC_PIPE_LINE"), false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, _, err := cs.OutputByteRange(0, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "TC_PIPE_LINE") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitForIdleBytes(t, mm, s.ID, cs.ID)

	marks := spans(t, mm, s.ID, cs.ID)
	if got := countSpansWithStatus(marks, api.LogAPIInput); got != 0 {
		t.Fatalf("an unsubmitted pipe write produced %d input spans, want 0: %+v", got, marks)
	}
	for _, m := range marks {
		if m.status != api.LogOutput {
			t.Fatalf("a pipe shell labelled shell output as %q: %+v", m.status, marks)
		}
	}
	if countSpansWithStatus(marks, api.LogOutput) == 0 {
		t.Fatalf("the pipe shell's reply was not recorded as output: %+v", marks)
	}
}
