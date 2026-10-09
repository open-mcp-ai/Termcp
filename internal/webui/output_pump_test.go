package webui

import (
	"strings"
	"testing"
)

// TestTerminalOutputPumpCoalescesWithoutHoldingAnEcho pins the two rules the
// WebSocket output pump runs on, because breaking either one is invisible in a
// unit test and expensive in a browser.
//
// Issue #93 ("卡顿") was measured, not guessed: a streaming command produced
// 11.7k WebSocket frames per second at 55 bytes each, taking 85% of the page's
// main thread, because the pump shipped one frame per PTY read. Coalescing them
// is what fixed it. But a naive coalescing rule — flush on a timer — adds that
// timer's period to every keystroke's echo, which was measured too (16ms hold on
// a 14ms round trip, i.e. more than doubling it). A terminal cannot spend that.
//
// So the pump must keep BOTH properties, and each is pinned separately below:
//
//	1. a batch leaves as soon as there is no backlog to merge into it, so an
//	   interactive echo is never held for the sake of a bound it will not reach;
//	2. a batch is still bounded, so a producer that never leaves a backlog cannot
//	   expand the frame rate without limit.
func TestTerminalOutputPumpCoalescesWithoutHoldingAnEcho(t *testing.T) {
	pump := readGoSource(t, "wswatch.go")
	body := between(t, pump, "func (c *uiWS) runWatch(", "\n}\n")

	// (1) The caught-up flush. `HasMoreOutput` is the only thing that knows the
	// producer stopped being ahead of us; without a branch on it, either nothing
	// is batched (one frame per read — the original bug) or an echo waits out a
	// timer (the regression this guards).
	if !strings.Contains(body, "shell.HasMoreOutput(rid)") {
		t.Error("the output pump no longer consults HasMoreOutput: without it there is no way to tell a " +
			"burst from a finished echo, so either every chunk ships alone (#93) or an echo waits on a timer")
	}
	caughtUp := between(t, body, "case !shell.HasMoreOutput(rid)", ":")
	if !strings.Contains(caughtUp, "terminalFlushMinGap") {
		t.Errorf("the caught-up branch must flush once the min-gap has elapsed: an echo whose bytes have all "+
			"arrived is what a person is waiting on; got %q", caughtUp)
	}
	switchBlock := between(t, body, "switch {", "\n\t\t}")
	if !strings.Contains(switchBlock, "flush()") {
		t.Errorf("the flush decision must call flush(); got %q", switchBlock)
	}
	if n := strings.Count(switchBlock, "flush()"); n != 2 {
		t.Errorf("a batch ends in exactly two ways — the byte bound, or the reader caught up with the "+
			"min-gap elapsed; got %d flush calls in %q", n, switchBlock)
	}

	// (2) The bounds. The byte bound is what a large replay needs; the min-gap is
	// what bounds a producer that stays level with the reader (a shell echoing a
	// loop line by line leaves no backlog for the test above to see — measured at
	// 100k frames/s before this bound existed).
	if !strings.Contains(body, "terminalFlushBytes") {
		t.Error("the output pump has no byte bound: a full-scrollback replay would ship as unbounded frames")
	}
	if !strings.Contains(body, "terminalFlushMinGap") {
		t.Error("the output pump has no min-gap bound: a producer that never leaves a backlog would still " +
			"produce one frame per write, which is the frame rate #93 is about")
	}

	// The pump must NOT be built on a bare periodic tick: that is the shape whose
	// cost is paid per echo. `time.Since` against the last flush is the only time
	// read allowed here.
	if strings.Contains(body, "time.Tick") || strings.Contains(body, "time.NewTicker") {
		t.Error("the output pump must flush on the backlog, not on a tick: a tick's period is added to every " +
			"keystroke's echo, which was measured at more than doubling the round trip")
	}

	// The coalescing budget itself has to stay small enough to be invisible: the
	// screen cannot repaint faster than a frame, so a hold well under one frame
	// cannot be seen. The value is pinned because raising it is the tempting way
	// to "improve" throughput, and it is paid in felt latency.
	ws := readGoSource(t, "ws.go")
	const decl = "terminalFlushMinGap = "
	i := strings.Index(ws, decl)
	if i < 0 {
		t.Fatal("ws.go no longer declares terminalFlushMinGap")
	}
	line := ws[i:strings.Index(ws[i:], "\n")+i]
	if !strings.Contains(line, "Millisecond") {
		t.Errorf("the min-gap should be sub-frame; got %q", line)
	}
	if strings.Contains(line, "16 * time.Millisecond") || strings.Contains(line, "Second") {
		t.Errorf("the min-gap must stay far below one frame (16ms), or a keystroke's echo becomes visible; got %q", line)
	}
}

// TestTerminalBytesAreNotFramedPerRead is the structural half of the same rule:
// the pump must accumulate before it sends. A `sendTerminalPayload` on the read
// path itself would mean one frame per chunk again, whatever the branch
// conditions above say.
func TestTerminalBytesAreNotFramedPerRead(t *testing.T) {
	pump := readGoSource(t, "wswatch.go")
	body := between(t, pump, "func (c *uiWS) runWatch(", "\n}\n")

	// The only send in the loop must be inside the flush closure, and the read
	// result must be appended to an accumulator rather than sent.
	readIdx := strings.Index(body, "shell.ReadTerminalStream(")
	if readIdx < 0 {
		t.Fatal("runWatch no longer reads the terminal stream")
	}
	tail := body[readIdx:]
	if !strings.Contains(tail, "batch = append(batch, out...)") {
		t.Error("the read result must be accumulated into the batch, not sent per read")
	}
	sendIdx := strings.Index(tail, "c.sendTerminalPayload(")
	appendIdx := strings.Index(tail, "batch = append(batch, out...)")
	if sendIdx >= 0 && appendIdx >= 0 && sendIdx < appendIdx {
		t.Error("the pump sends before accumulating: that is one frame per read, which is the frame rate #93 reports")
	}
	// And the batch must be reset, or the same bytes ride along in every later
	// frame and the terminal repeats itself.
	if !strings.Contains(body, "batch = batch[:0]") {
		t.Error("the batch is never reset after a flush: its bytes would be re-sent with every later frame")
	}
	// A final flush must immediately precede EVERY terminal_done marker, or the
	// tail of a shell's output is dropped at the moment the shell exits. The check
	// is adjacency, not "somewhere earlier": the loop already contains other
	// flush() calls (the switch's cases), so a containment test passes even after
	// the one that matters is deleted.
	var doneAt []int
	for at := 0; ; {
		i := strings.Index(body[at:], `"type": "terminal_done"`)
		if i < 0 {
			break
		}
		doneAt = append(doneAt, at+i)
		at += i + 1
	}
	if len(doneAt) == 0 {
		t.Fatal("runWatch no longer emits terminal_done")
	}
	for _, idx := range doneAt {
		// Walk back over the marshal/comment lines to the previous statement and
		// require it to be the flush.
		prev := strings.TrimRight(body[:idx], " \t\n")
		if nl := strings.LastIndex(prev, "\n"); nl >= 0 {
			prev = prev[:nl]
		}
		if !strings.HasSuffix(strings.TrimSpace(prev), "flush()") {
			t.Errorf("terminal_done must be preceded by flush() so the shell's last output is shipped first; "+
				"the statement before it is %q", strings.TrimSpace(prev))
		}
	}
}
