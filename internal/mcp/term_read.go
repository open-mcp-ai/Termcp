package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
	"github.com/open-mcp-ai/termcp/term/locator"
)

// directiveRead reads a window of a stream, honouring the three axes.
//
// A comma-separated target reads each channel independently and returns one slot
// per entry, which is the same batch shape input already had — a fleet is one
// question with N answers, not N round trips.
func (s *Server) directiveRead(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if ids, isBatch := splitBatchIDs(stmt.Target); isBatch {
		slots := make([]envelope.Slot, 0, len(ids))
		for _, id := range ids {
			single := stmt
			single.Target = id
			slots = append(slots, s.directiveReadOne(ctx, single, win))
		}
		return slots
	}
	return []envelope.Slot{s.directiveReadOne(ctx, stmt, win)}
}

// directiveReadOne reads one channel, waiting first when the caller named evidence.
//
// `--until` ON A READ IS THE SAME PREDICATE AS ON A RUN, and its absence is the old
// behaviour exactly: the read happens now and the reply says nothing about completion.
// A read is a cursor over the stream, so the honest reading of "wait until X appears" is
// "wait for the bytes that arrive from the channel's current end" — the predicate is
// about what happens NEXT, not about the scrollback that is already there.
//
// A CALL THAT ASKS FOR NOTHING DOES NOT WAIT. bytes_limit 0 means "show me nothing and
// consume nothing", and a wait followed by an empty reply is the worst of both: the
// caller spent its window and learned nothing it could not have had at once.
func (s *Server) directiveReadOne(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	if boolFlag(stmt, "screen") {
		return s.readScreen(ctx, stmt, win)
	}
	// A FLAG THAT CANNOT APPLY IS REFUSED, NOT DROPPED. `--top` and `--rows` describe a
	// window of the screen model; the byte read has no rows, so the pair used to be parsed,
	// ignored, and answered with the whole log — a caller who asked for nine rows could not
	// tell it had been served everything.
	if _, ok := stmt.Flag("top"); ok {
		return badRequest(stmt, "`--top` pages the screen model, so it needs `--screen`: `read "+stmt.Target+" --screen --top 0 --rows 20`")
	}
	if _, ok := stmt.Flag("rows"); ok {
		return badRequest(stmt, "`--rows` sizes a screen page, so it needs `--screen`: `read "+stmt.Target+" --screen --top 0 --rows 20`")
	}
	watch := s.untilWatchFrom(stmt)
	base, observable := s.outputSizeFor(stmt.Target)
	if watch == nil || !observable || win.BytesLimit == 0 {
		// A peer or file target has no channel stream, and its own refusal explains
		// itself better than a wait that could never end.
		return s.readChannelNow(ctx, stmt, win)
	}
	began := time.Now()
	s.awaitUntil(ctx, stmt.Target, win, base, watch)
	slot := s.readChannelNow(ctx, stmt, remaining(win, time.Since(began)))
	watch.headers(&slot)
	if !watch.matched && slot.Code < 400 {
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Hint"] = "`--until` did not match within the window, so nothing here says the bytes you asked for have arrived. " +
			"read again, or widen the window."
	}
	return slot
}

// readChannelNow is the read itself: the cursor over the stream, with no waiting.
func (s *Server) readChannelNow(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// PEER FLAGS ON A CHANNEL READ ARE REFUSED, NOT IGNORED (issue #159).
	//
	// --since and --at are declared on the `read` verb, but their meaning belongs to
	// a peer's frame log: --since pages forward through frames, --at slices ONE
	// frame's bytes. A shell or pipe channel has neither, so on this path they name
	// nothing — and the old code never looked, which made
	//
	//	read <channel> --since 10
	//
	// a confident answer about a different question: the default cursor read, with
	// the cursor advanced past bytes the caller asked to filter. Accepting a
	// parameter only to ignore it is the one path the project forbids.
	//
	// The check runs FIRST, before the window and before any tool call, because the
	// refusal must consume nothing: a rejected call that already moved the cursor
	// has answered by side effect.
	for _, notOnChannel := range []struct{ flag, where, scope string }{
		{"since", "`read <session> cdp|mcp|jrpc --since N` (the frames that arrived after N)", "a peer's frame log"},
		{"at", "`read <session> cdp|mcp|jrpc --at FRAME [--offset B] [--bytes N]` (one frame's bytes)", "a peer's frame log"},
		{"bytes", "`read <session> cdp|mcp|jrpc --at FRAME --bytes N` (how much of that one frame)", "a peer's frame log"},
		// --mode AND --lines ARE FILE-READ FLAGS, for the same reason and with the same
		// failure mode: they are declared on `read`, so a channel read parses them and
		// then has nowhere to apply them. A caller who wrote `read <channel> --mode hex`
		// asked for bytes and would have been handed text — the answer would look right
		// and be the wrong projection.
		{"mode", "`read <session> file <path> --mode text|hex|file` (how to render a file)", "a file read"},
		{"lines", "`read <session> file <path> --lines` (number the lines of a file)", "a file read"},
	} {
		if stmt.FlagState(notOnChannel.flag) == directive.FlagAbsent {
			continue
		}
		return badRequest(stmt, "`--"+notOnChannel.flag+"` is for "+notOnChannel.scope+
			", not a channel: use "+notOnChannel.where+". A channel is a byte stream; page it with `--cursor` / `--offset`.")
	}
	// bytes_limit 0 means "hand back the channel, consume nothing". readSlotAfter
	// honours it; this path has to as well, or the one directive whose whole point
	// is reading would be the one place the caller cannot decline to read.
	// The offset is the stream's current end, so the reply still says where the
	// next read starts.
	if win.BytesLimit == 0 {
		start, _ := s.outputSizeFor(stmt.Target)
		return envelope.Slot{
			Code:    204,
			Headers: map[string]string{"Path": stmt.Path, "Content-Range": envelope.UnsatisfiedRange(start)},
		}
	}
	// --grep takes its own path, because it changes what the window counts.
	// `lines_limit` normally bounds raw lines; with a pattern it bounds MATCHES,
	// and the filter therefore has to run before the window rather than after it.
	if _, ok := stmt.Flag("grep"); ok {
		return s.readGrep(ctx, stmt, win)
	}
	// THE TARGET IS NORMALISED TO A REAL SHELL ID HERE, ONCE (issue #247).
	//
	// Every path below hands this value to `shell_output`, which resolves ids and
	// nothing else — so a name-addressed channel (`dogfood:1`) or a name-addressed
	// session (`dogfood`) reached the tool as an unknown id and answered
	// `session "dogfood" not found`. Resolving here rather than in each tool keeps
	// one spelling rule for the whole verb, and the same resolver the other
	// directives use (`requireShell` / `sessionByName`) so a name means the same
	// thing everywhere.
	if cs, bad := s.requireShell(stmt.Target); bad == nil && cs != nil {
		stmt.Target = cs.ID
	}
	args := argsOf("shell_id", stmt.Target)
	// --offset IS THE SAME BYTE POSITION AS --cursor, and on a channel read it used to
	// be accepted and IGNORED (dogfood finding on #159): the flag is declared for the
	// peer `--at` path, so the parser took it and this function never looked. A caller
	// who wrote `read <channel> --offset 0` therefore got the default cursor read — on
	// a closed channel, an empty one — and nothing said the flag had not been used.
	//
	// Asking twice is refused rather than silently resolved: both name where the read
	// starts, and a caller who wrote both is not describing a position.
	if stmt.FlagState("cursor") != directive.FlagAbsent && stmt.FlagState("offset") != directive.FlagAbsent {
		return badRequest(stmt, "`--cursor` and `--offset` name the same byte position; pass one of them.")
	}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"cursor", "offset"},
		[2]string{"offset", "offset"},
		[2]string{"from-line", "from_line"},
		[2]string{"tail", "tail_lines"},
		[2]string{"reader", "reader_id"},
	); bad {
		return slot
	}
	// A REGISTERED READER IS A CURSOR THE CALLER NAMED (issue #286). `reader <channel>`
	// answers with an id and the buffer keeps a position for it; without this the id had
	// no consumer on this surface, so registering one was a way to pin the compaction
	// watermark and never read anything. Absent stays absent: reader_id 0 IS the default
	// reader, and sending it explicitly would be a different request from saying nothing.
	if stmt.FlagState("reader") == directive.FlagAbsent {
		delete(args, "reader_id")
	}
	if v, ok := stmt.Flag("stream"); ok {
		args["stream"] = v
	}
	if boolFlag(stmt, "raw") {
		args["strip_ansi"] = false
	}
	// THE SESSION'S ENCODING, when the caller knows it. A terminal is not a UTF-8
	// device: a zh-CN Windows console writes GBK, so without this the reply was a
	// refusal the caller could not act on (issue #149).
	if v, ok := stmt.Flag("encoding"); ok {
		args["charset"] = v
	}
	// A TAIL READ ON A LIVE PIPE MUST NOT ANSWER BEFORE THE COMMAND DOES.
	//
	// `read <channel>` waits — the plain path spends the caller's window on the pipe's
	// exit — while `--tail`/`--from-line` took a different branch that scanned the buffer
	// immediately. So the same channel answered the same question two ways: the plain
	// read returned the whole output after a second, and
	//
	//	read <pipe> --tail 20        ->  204 No Content, Waited: 0ms
	//
	// which is byte-identical to "the command printed nothing". The workaround in the
	// issue (`Start-Sleep 10; read --tail 20`) exists because of this branch (#245).
	//
	// PIPE ONLY, and deliberately not a pty: a pty's shell outlives every command, so
	// "wait for completion" has no ending there and every glance at the screen would cost
	// the whole window. A pty tail is a snapshot on purpose; `run` and the plain cursor
	// read are the paths that wait. `timeout:0` still means "do not wait" for either,
	// because awaitExit returns at once for a zero limit.
	waited := time.Duration(0)
	if s.completionWait(stmt.Target) &&
		(stmt.FlagState("tail") != directive.FlagAbsent || stmt.FlagState("from-line") != directive.FlagAbsent) {
		start, _ := s.outputSizeFor(stmt.Target)
		began := time.Now()
		s.awaitExit(ctx, stmt.Target, win, start)
		waited = time.Since(began)
	}
	slot, _ := s.readSlot(ctx, stmt, win, args, waited)
	if slot.Code == 204 {
		// Nothing new arrived: whatever the last run's Still-Running said has expired.
		s.forgetStaleBusy(stmt.Target)
	}
	return slot
}

// awaitRunOutput waits for the command a `run` just typed, using the same signals the
// read half uses, and returns how long it waited.
//
// ONE PLACE DECIDES WHEN A COMMAND'S OUTPUT IS THERE, because two places deciding it
// separately is how the plain half and the searched half of one verb end up disagreeing:
// `run --grep` read with a zero window while `run` read with the caller's, so a search
// over bytes that had not arrived answered "no line matched … in the 0 lines scanned".
//
// A LIVE PIPE HAS NO SIGNAL AT ALL — no exit to wait for and no pty to go quiet — so this
// returns immediately and the caller's window is spent by the read that follows, which
// blocks for it. That is the division the plain path makes, and it is why the return is a
// duration rather than a bool: what is LEFT of the window is what the read may spend.
func (s *Server) awaitRunOutput(ctx context.Context, stmt directive.Statement, win termArgs, start int64) time.Duration {
	began := time.Now()
	if watch := s.untilWatchFrom(stmt); watch != nil {
		s.awaitUntil(ctx, stmt.Target, win, start, watch)
	} else if s.completionWait(stmt.Target) {
		s.awaitExit(ctx, stmt.Target, win, start)
	} else if s.ptyRunning(stmt.Target) {
		s.awaitWindow(ctx, stmt.Target, win, start)
	}
	return time.Since(began)
}

// readSlotAfter reads the window a command produced, starting where it was
// submitted.
//
// Completion differs by mode, and neither kind ends on one signal:
//
//   - A PIPE channel's process is a child this server started, so its exit is
//     observed — the stronger fact, since the channel closes and the exit code
//     arrives with the bytes. A resident process never exits, so the stream
//     standing still after this call's bytes (pipeQuiet) ends that wait too.
//   - A PTY channel has no exit to observe — a shell does not report that a
//     command finished — so the stream standing still (ptyQuiet) is the only
//     signal there is, and only a command that has printed nothing has to wait
//     out the window.
//
// `timeout` bounds both and defaults to two seconds.
func (s *Server) readSlotAfter(ctx context.Context, stmt directive.Statement, win termArgs, start int64) envelope.Slot {
	args := argsOf("shell_id", stmt.Target, "offset", float64(start))
	if v, ok := stmt.Flag("encoding"); ok {
		args["charset"] = v
	}
	// WHERE THIS COMMAND'S OUTPUT BEGINS (issue #194).
	//
	// The reply is a SAMPLE of the channel's stream, and the one number that turns the
	// sample into something a caller can act on is the offset the input landed at: it is
	// what `read --offset <n>` takes to get the rest, and it is what tells an empty reply
	// apart from a caller whose cursor is already past the output. The number was already
	// computed — it is the offset this window was scoped to — it just was not said.
	//
	// It is the offset BEFORE the input, not the marker's: the window starts where the
	// command's bytes begin, so re-reading from Input-At gives back this same window
	// (marker included) rather than skipping the echo.
	inputAt := strconv.FormatInt(start, 10)
	// A byte limit of 0 means "hand back the channel, consume nothing": the
	// directive must not even read, or a JSON-RPC frame destined for `mcp recv`
	// would be eaten by the envelope.
	if win.BytesLimit == 0 {
		return envelope.Slot{
			Code: 204,
			Headers: map[string]string{
				"Path":          stmt.Path,
				"Content-Range": envelope.UnsatisfiedRange(start),
				"Input-At":      inputAt,
			},
		}
	}
	waitStarted := time.Now()
	// windowExpired is a FACT the wait already computed and then threw away: a pty has
	// no completion signal, so "the window ran out while bytes were still arriving" is
	// the closest thing to "this command is still running" the server can honestly say —
	// and without it the caller cannot tell a finished command from a window that ended
	// (issue #161, P1-3).
	windowExpired := false
	// `--until` REPLACES THE WAIT, IT DOES NOT ADD TO IT. A caller that named the
	// evidence it wants is not waiting for a threshold as well: the predicate or the
	// deadline, whichever comes first, and neither of them is a verdict on its own.
	watch := s.untilWatchFrom(stmt)
	if watch != nil {
		if !s.awaitUntil(ctx, stmt.Target, win, start, watch) {
			// The caller's window was spent without seeing the caller's evidence.
			// That is the same shape as an expired pty window — the budget ran out
			// with nothing to report — and it is reported the same way: as what the
			// wait did, never as what the command did.
			windowExpired = true
		}
	} else if wait := s.completionWait(stmt.Target); wait {
		s.awaitExit(ctx, stmt.Target, win, start)
	} else if s.ptyRunning(stmt.Target) {
		windowExpired = s.awaitWindow(ctx, stmt.Target, win, start)
	}
	// DID ANYTHING HAPPEN IN THIS WINDOW? It is the fact that separates the two things
	// `Still-Running` used to be asked about (issue #308), and it is measured on the
	// channel rather than on the reply: a folded window shows no body, but the bytes
	// existed, so the reply is the wrong place to ask.
	sawBytes := false
	if now, ok := s.outputSizeFor(stmt.Target); ok && now > start {
		// NOT OUR OWN ECHO. A pty echoes the line it was handed, so "bytes arrived since
		// Input-At" was true before the command had done anything — and on a Linux runner
		// that turned "your line is queued behind a program holding the terminal" into a
		// 200 with no explanation at all. What the caller wants to know is whether the
		// command produced anything, so the echo is subtracted before answering.
		sawBytes = !s.onlyEcho(stmt.Target, start, now, stmt.Payload)
	}
	slot, channelEnd := s.readSlot(ctx, stmt, win, args, time.Since(waitStarted))
	if slot.Code == 204 {
		// Nothing new arrived: the channel is not producing output any more, so what the
		// last run's Still-Running said about it has expired.
		s.forgetStaleBusy(stmt.Target)
	}
	if slot.Code < 400 {
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Input-At"] = inputAt
	}
	// ONLY EVIDENCE PRODUCES `Completion`. A wait that ended because the stream went
	// quiet produces none: a finished command and one that is thinking look identical
	// on a pty, so the header used to say `quiet` — a measurement dressed as a verdict,
	// and callers acted on it by typing the next command into a program that still held
	// the terminal. Absence now means what it says: not known.
	watch.headers(&slot)
	if watch != nil && !watch.matched && slot.Code < 400 {
		// A CONTRADICTION IN ONE REPLY: if the shell exited and printed its code (e.g.
		// Windows ConPTY child exit), saying "this reply says nothing about whether the
		// command finished" contradicts the `Exit-Code:` printed above it. The predicate
		// did not match, but the answer IS known.
		if slot.Headers != nil && slot.Headers["Exit-Code"] != "" {
			// An exit code was observed, so the command finished: --until did not match its
			// OSC 133 marker, but the process has reported its status.
			slot.Headers["Completion"] = "exit"
		} else {
			slot.Headers["Hint"] = "`--until` did not match within the window, so this reply says nothing about whether the command finished. " +
				"read " + stmt.Target + " --tail 20 looks at the stream, and key " + stmt.Target + " ctrl+c interrupts whatever holds the terminal."
		}
	}
	// NOT GATED ON THE WINDOW EXPIRING. A queued line echoes and then goes quiet, so the
	// wait ends on quiet — the same ending a finished command has — and gating here meant
	// the one case that most needs the explanation (nothing but your own echo came back)
	// was the one case that got none. What separates the cases is whether any bytes came
	// from the command, which is `sawBytes`; `windowExpired` still decides Still-Running,
	// because only a window that ended mid-stream is evidence of work.
	if watch == nil && slot.Code < 400 && s.ptyRunning(stmt.Target) && slot.Headers["Still-Running"] == "" {
		// Same header the pipe path uses, same meaning: the command was accepted and has
		// not finished. It is set only when the window expired, so it does not become the
		// permanently-true flag that kept it off the pty path in the first place.
		//
		// AND ONLY WITH EVIDENCE (issue #308). The window expiring is not evidence that
		// THIS line is executing: the pty driver queues what it is given until the
		// foreground process reads, so a line typed while a program holds the terminal
		// sits in that queue and produces nothing — byte for byte what a command that is
		// running silently produces. The caller cannot tell those apart, and neither can
		// this server ("is a foreground command running" is unanswerable from here). What
		// it CAN tell apart is "bytes arrived and were still arriving when the window
		// ended", which is evidence of work, from "nothing arrived at all", which is only
		// evidence of silence. So the no-bytes case names the ambiguity instead of
		// asserting a state, and `Still-Running` stays the evidence-based claim it was.
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		if sawBytes && windowExpired {
			slot.Headers["Still-Running"] = "true"
		} else if !sawBytes {
			slot.Headers["Hint"] = "no bytes since this line. A pty gives no signal that a line was read, " +
				"so a silent command and a line still queued behind a program holding the terminal look " +
				"identical here: read again, or key " + stmt.Target + " ctrl+c to interrupt what holds it."
		}
	}
	// A WINDOW I SHOWED IS A WINDOW I READ. The read above is positional (it names an
	// offset), and a positional read moves no cursor — so `read <channel>`'s documented
	// default, "the new bytes since the last read", was false for the flow callers
	// actually use: after three `run`s the first plain read replayed the channel from
	// byte zero (issue #162, S2). The caller had seen those bytes; the cursor had not.
	//
	// Only when bytes were actually handed over: `fold` and `bytes_limit: 0` reply
	// without showing anything, and advancing past bytes the caller never saw would
	// turn a withheld window into a skipped one.
	if slot.Code < 400 && slot.Body != "" {
		// channelEnd, not Content-Range's end: a `--stream e|o` reply numbers its own
		// stream, and this cursor walks the channel (issue #202).
		if cs, bad := s.requireShell(stmt.Target); bad == nil {
			cs.AdvanceDefaultReader(channelEnd)
		}
	}
	return slot
}

// humanTyped reports whether bytes entered this channel through the non-MCP entrance
// since start.
//
// THIS IS THE ONE FACT ABOUT "WHO IS ON THE OTHER END" THAT TERMCP ACTUALLY HAS.
// The tempting question — "is something in the foreground waiting for input?" — cannot
// be answered from here (the process group lives on the far side of the SSH connection,
// and a Windows terminal has no such notion), and guessing it from the byte stream is
// impossible: a program blocked at a prompt and a finished command are byte-identical.
//
// But the transcript already records who typed. Every input span carries a status —
// `i` the HTTP/WebSocket surface, `a` an agent through MCP — because the log has to be
// replayable and auditable. So "someone else is writing here" is a recorded fact, not an
// inference, and a caller about to type into a shared channel deserves to know first.
//
// The value reported is therefore the entrance (`api`), not a guess about who is behind
// it: a browser and a script driving /api/term are the same status in the log, and
// claiming "human" for both would be wrong for one of them.
//
// The question asked is "at or after the start of this window", and the answer comes from
// the store's sparse seek table (MarksWindow) rather than the whole index: a reader that
// needs one screen of a long log should not pay for the log.
func (s *Server) humanTyped(handle string, start, end int64) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	if end < start {
		end = start
	}
	marks, _, err := s.sessMgr.MarksWindow(cs.ParentSessionID(), cs.ID, start, end)
	if err != nil {
		return false
	}
	for _, m := range marks {
		if m.Offset < start {
			continue
		}
		if m.Status == api.LogAPIInput {
			return true
		}
	}
	return false
}

// ptyRunning reports whether the handle is a live pty channel. The handle may
// be a shell id OR a session id naming its primary channel, so it resolves
// through the same door every other directive uses.
func (s *Server) ptyRunning(handle string) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	info := cs.Info()
	return info.Mode == api.ModePTY && info.Status == api.SessionRunning
}

// awaitWindow waits out a pty command, bounded by the caller's time window.
//
// A PTY HAS NO COMPLETION SIGNAL, so the wait used to be a blind sleep of the
// whole window: `run` on a 20s window took 20s on a command that finished in one,
// and the reply said nothing about how long the command itself took. The window
// is the caller's budget, not the command's duration, so the wait now watches the
// stream: it ends when the stream has stood still for ptyQuiet, and only falls
// back to the window when there is nothing to watch (a command that has printed
// nothing yet has no other signal). timeout=0 stays a snapshot, not a wait.
//
// The cost of the trade: a command that thinks silently for longer than ptyQuiet
// after printing comes back with what it printed so far, and the caller reads on.
// That is the same contract a terminal has — you cannot tell "done" from "quiet"
// either — and it is the one direction the old blind sleep got wrong.
//
// IT RETURNS WHICH OF THE TWO HAPPENED, because the caller can act on the difference:
// the window running out means the command has not finished, which is a fact worth a
// header. Ending on quiet means the command stopped talking, which is as close to "done"
// as a terminal ever gets.
func (s *Server) awaitWindow(ctx context.Context, handle string, win termArgs, start int64) (windowExpired bool) {
	limit := time.Duration(win.TimeoutSeconds * float64(time.Second))
	if limit <= 0 {
		return false
	}
	// THE THRESHOLD IS READ ONCE, AT ENTRY.
	//
	// A gap observed during this wait must not shorten this wait: the command that
	// produced it may still have a chunk coming, and ending on the evidence of the
	// first two chunks is exactly the truncation the threshold exists to avoid. What
	// was learned here is for the NEXT command on this channel, which is the whole
	// point of a per-channel learner — the second `run` is cheaper than the first.
	tracker := s.quietTracker(handle)
	quiet := tracker.threshold()
	tracker.noteUsed(quiet)
	deadline := time.Now().Add(limit)
	last := start
	var still time.Time
	var prevChunk time.Time
	ticker := time.NewTicker(ptyPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		// THE WINDOW IS CHECKED FIRST, because after it the two outcomes are not
		// equally worth reporting: the deadline is a fact, "quiet" is an inference.
		// The quiet branch sat above this line, so a tick that woke after the window
		// and saw a still stream answered "quiet" — the header that means "my window
		// ran out" was suppressed by exactly the race it exists to resolve, and on a
		// loaded machine the last chunk can be an entire quiet window old.
		if !time.Now().Before(deadline) {
			return true
		}
		now, ok := s.outputSizeFor(handle)
		if !ok {
			// The channel went away under the wait; the read that follows reports it
			// in the caller's own vocabulary instead of "nothing arrived".
			return false
		}
		switch {
		case now != last:
			if !prevChunk.IsZero() {
				tracker.observe(time.Since(prevChunk))
			}
			prevChunk = time.Now()
			last, still = now, time.Time{}
			// THE REPLY CANNOT GROW, SO WAITING BUYS NOTHING.
			//
			// The space window is a limit on how much enters the caller's context, so once
			// the byte window is full every further byte is discarded: the wait would sit
			// there for the rest of the window to collect output the reply cannot carry.
			// A noisy command (a scan, a brute force) never goes quiet, so this was the
			// only thing that could end the wait early — the reporting caller's 320s
			// window over a command that filled the reply in its first second is exactly
			// this case (issue #161, P1-3).
			if win.BytesLimit > 0 && now-start >= int64(win.BytesLimit) {
				return true
			}
		case now <= start:
			// Nothing has been said yet. Silence before the first byte is not
			// completion, it is a command that has not started talking; only the
			// window can end this wait.
		default:
			if still.IsZero() {
				still = time.Now()
			} else if time.Since(still) >= quiet {
				return false
			}
		}
	}
}

// pipeRunning reports whether the handle is a live PIPE channel — the only kind
// whose command has an observable end, and therefore the only kind that can be
// "still running" in a way worth reporting.
func (s *Server) pipeRunning(handle string) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	info := cs.Info()
	return info.Mode == api.SessionMode("pipe") && info.Status == api.SessionRunning
}

// completionWait reports whether this channel's completion is observable, which
// is true exactly for a pipe channel that has not exited yet.
func (s *Server) completionWait(handle string) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	info := cs.Info()
	return info.Mode == api.SessionMode("pipe") && info.Status == api.SessionRunning
}

// awaitExit waits for a pipe channel's process to exit, bounded by the caller's
// timeout. It returns as soon as the channel closes — that is the whole point of
// the pipe half of the completion rule.
//
// A RESIDENT PROCESS HAS NO EXIT TO WAIT FOR, and this used to wait for one
// anyway: `adb shell`, a REPL, a daemon — anything that reads a line and stays
// alive — made every `run` burn the caller's ENTIRE window on a command that had
// answered in twenty milliseconds. Measured on a resident echo process: a 1s
// window took 1.0065s, a 3s window took 3.0068s, linear in the window and not in
// the command (issue #207).
//
// SO THERE ARE TWO ENDINGS NOW, and the exit stays first because it is the
// stronger fact:
//
//   - the process exited — completion, reported as a status exactly as before
//   - the stream went still for pipeQuiet after this call's bytes — the command
//     answered, and the process is merely still resident
//
// `start` IS THE WHOLE REASON THE SECOND ENDING IS SAFE. Quiet before the first
// byte is not an answer, it is a command that has not started talking; waiting
// for the window is the only honest thing to do there, which is what
// `now <= start` preserves. Without it a resident process would answer every
// silent command instantly with an empty 204 — the failure mode this change
// would otherwise introduce while fixing the slow one.
func (s *Server) awaitExit(ctx context.Context, handle string, win termArgs, start int64) {
	limit := time.Duration(win.TimeoutSeconds * float64(time.Second))
	if limit <= 0 {
		return
	}
	deadline := time.Now().Add(limit)
	ticker := time.NewTicker(ptyPoll)
	defer ticker.Stop()
	last := start
	var still time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cs, bad := s.requireShell(handle)
		if bad != nil || cs.Info().Status != api.SessionRunning {
			return
		}
		now, ok := s.outputSizeFor(handle)
		if !ok {
			return
		}
		switch {
		case now != last:
			last, still = now, time.Time{}
		case now <= start:
			// Nothing said yet. Only the window can end this wait.
		default:
			if still.IsZero() {
				still = time.Now()
			} else if time.Since(still) >= pipeQuiet {
				return
			}
		}
		if !time.Now().Before(deadline) {
			return
		}
	}
}

// readSlot performs one read and renders it. This is where the three axes are
// applied for every directive that returns bytes.
//
// `waited` IS HOW MUCH OF THE CALLER'S WINDOW IS ALREADY SPENT. The wait before this
// read (awaitWindow / awaitExit) burns the caller's budget, and the read that follows
// then passed the FULL timeout again — so a `run` with timeout=60 could block for 120
// seconds, and the second half of that was invisible in the reply. The window is one
// budget, not two: the read gets what is left.
//
// That is also what makes `timeout` mean one thing. `readTimeout` capped a plain read at
// 60 and left `run` uncapped, so the same parameter bounded two directives differently
// and nothing in the reply said which had happened (issue #161, P1-3).
// readSlot reads one window and also reports where that window ends IN THE CHANNEL.
//
// The second return is separate from the slot's Content-Range on purpose: that header
// carries the caller's addresses, and on a `--stream e|o` read those are the stream's,
// while the reader cursor is a CHANNEL position (issue #202). Advancing the cursor by
// the reply's own end would walk it into the wrong numbering the moment a caller
// filtered the stream.
func (s *Server) readSlot(ctx context.Context, stmt directive.Statement, win termArgs, args map[string]any, waited time.Duration) (envelope.Slot, int64) {
	started := time.Now()
	args["max_bytes"] = float64(win.BytesLimit)
	args["max_lines"] = float64(win.LinesLimit)
	// A pipe channel that has not exited yet must not block for the whole timeout
	// on a read that has nothing new; a pty read waits for the quiet window
	// instead. Both are upper bounds, so the smaller of the two wins.
	budget, quietCapped := s.readBudget(stmt, args, win, waited)
	args["timeout"] = budget

	res, fail := s.callTool(ctx, "shell_output", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail), 0
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)}), 0
	}
	// WAITED IS THE WHOLE CALL, not just this read.
	//
	// The header exists to tell "I waited and heard nothing" from "I did not wait" —
	// the distinction that keeps an empty reply from looking like a broken channel. It
	// was measured from the READ, so the wait before it (where the window is actually
	// spent) was missing: a call that sat on a 1s window answered `Waited: 0ms`, and the
	// reporting caller read that as "the window never happened" (issue #161, P1-3).
	// end_offset is the CHANNEL's end even when the reply's own addresses are the
	// stream's: the two groups are always both present (see windowResult).
	slot := s.windowSlot(stmt, win, doc, waited+time.Since(started))
	if quietCapped && slot.Code == 204 && s.ptyRunning(stmt.Target) {
		// HOW THE WAIT ENDED IS A FACT, and here it ended on the quiet window rather than
		// on the caller's number. Same header `run` uses for the same ending, and same
		// reach: a pty's completion is not observable, so what is reported is the
		// evidence (the stream stood still), never a conclusion. Without it, `Waited:
		// 512ms` under a 60s window reads as "the server ignored my window".
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Completion"] = "quiet"
	}
	return slot, int64(getFloatAny(doc, "end_offset", 0))
}

// remaining is the part of the caller's window that the wait did not spend.
//
// A negative or zero remainder is zero: the wait already used the budget, so the read
// that follows must not block at all. That is the difference between "the window ended"
// and "the window ended, and then we waited again".
func remaining(win termArgs, waited time.Duration) termArgs {
	left := win.TimeoutSeconds - waited.Seconds()
	if left < 0 {
		left = 0
	}
	win.TimeoutSeconds = left
	return win
}

// windowSlot renders a shell_output document as an envelope slot.
//
// Every byte fact comes from the handler's own reply: the offsets, the total, the
// session status and the channel's exit code. The envelope adds the HTTP shape and
// nothing else — no second read, no second source of truth.
func (s *Server) windowSlot(stmt directive.Statement, win termArgs, doc map[string]any, waited time.Duration) envelope.Slot {
	start := int64(getFloatAny(doc, "start_offset", 0))
	end := int64(getFloatAny(doc, "end_offset", 0))
	total := int64(getFloatAny(doc, "total_bytes", 0))
	// THE ADDRESSES IN THIS REPLY ARE THE ONES THE CALLER PAGES WITH (issue #202).
	//
	// On a `--stream e|o` read the handler reports both numberings: start/end/total
	// stay the CHANNEL's (the cursor walks that) and stream_* are the selected stream's.
	// Everything the caller reads has to come from the second group — a Content-Range
	// that counts stdout, next to a body that does not contain it, is an address the
	// caller cannot resume from.
	streamed := getString(doc, "stream", "") != ""
	if streamed {
		start = int64(getFloatAny(doc, "stream_start", 0))
		end = int64(getFloatAny(doc, "stream_end", 0))
		total = int64(getFloatAny(doc, "stream_total_bytes", 0))
	}
	// Trimmed is a CHANNEL-space count: the withheld bytes are the channel window's
	// tail, so the arithmetic below has to be done in the coordinates it was measured in.
	trimStart := start
	trimEnd := end
	trimTotal := total
	if streamed {
		trimStart = int64(getFloatAny(doc, "start_offset", 0))
		trimEnd = int64(getFloatAny(doc, "end_offset", 0))
		trimTotal = int64(getFloatAny(doc, "total_bytes", 0))
	}
	_ = trimStart
	body := getString(doc, "output", "")
	headers := map[string]string{"Path": stmt.Path}

	// A WINDOW THAT ROLLED BACK SAYS SO.
	//
	// The rule is that a window never ends inside a character, so the bytes that would
	// have been cut are withheld and the next read delivers them whole. That is lossless
	// and the Content-Range already says where the window stopped — but "I got fewer
	// bytes than bytes_limit" and "a character straddled the end" are the same bytes on
	// the wire, and a caller paging by its own arithmetic (offset += bytes_limit) would
	// skip the withheld bytes and then land INSIDE a character on the next read, where
	// the reply is a 400 naming a charset the session does not write. One header turns
	// that into a fact the caller can act on.
	if n := int64(getFloatAny(doc, "trimmed_bytes", 0)); n > 0 {
		// TWO CASES, because the remedy differs and only one of them has one.
		//
		// Mid-stream, the withheld bytes are simply early: the next read starts at them
		// and gets them whole. At the END of the stream there is no next read that can
		// complete them — they are a fragment of a character that the writer never
		// finished — so promising "read --cursor A continues there" would send the caller
		// into a loop that returns nothing forever. Say which one it is.
		//
		// Both are said in terms of the CHARSET, because that is what decided the cut:
		// the boundary came from the encoding, not from the size limit the caller passed.
		if trimEnd+n >= trimTotal {
			headers["Trimmed"] = fmt.Sprintf("bytes %d-%d withheld: they are the end of the stream and do not form a character in this encoding, so no read returns them as text",
				trimEnd, trimEnd+n-1)
		} else {
			headers["Trimmed"] = fmt.Sprintf("bytes %d-%d withheld: the window end fell inside a character in this encoding; read --cursor %d continues there",
				trimEnd, trimEnd+n-1, trimEnd)
		}
	}

	// A FILTERED READ SAYS WHAT IT PASSED OVER.
	//
	// `--stream o` answers "stdout only" by selecting a subset of the span the window
	// covers, and the span still runs to End — the cursor has to move by the range, or a
	// poll loop would re-read the same bytes forever. So the bytes of the other stream
	// leave the caller's cursor without ever being shown, and nothing used to say so.
	// The worst case was not a partial body: `--stream e` on a stdout-only channel
	// answered 200 with an EMPTY body and `Content-Range: bytes 0-7/8` — "eight bytes
	// here" and zero delivered — and the plain read that followed returned 204, so eight
	// bytes of output were consumed by a reply that displayed nothing at all.
	//
	// The count and the flag that shows them turn that into a fact the caller can act on.
	// It is deliberately NOT a refusal and NOT a rewind: the caller asked for a view, and
	// the bytes are still in the channel for whoever asks for the other view.
	if n := int64(getFloatAny(doc, "stream_omitted_bytes", 0)); n > 0 {
		// The stream is a flag on the statement, not a window parameter: the filter runs
		// inside the read, and only the statement knows which side was asked for.
		want, _ := stmt.Flag("stream")
		other := "e"
		if want == "e" {
			other = "o"
		}
		headers["Omitted"] = fmt.Sprintf("%d bytes in this range are not in stream %s and went past unseen; read --stream %s (or --stream all) shows them, and --offset reaches back",
			n, want, other)
	}

	// WHO ELSE IS TYPING. The reply can say that a person wrote into this channel,
	// because the transcript records the source of every input span (`i` human through
	// the HTTP/WebSocket surface, `a` an agent through MCP) — the log has to be
	// replayable, so the attribution is a recorded fact rather than an inference. It is
	// the one part of "this channel's state is not mine alone" that termcp can prove: a
	// program blocked at a prompt and a finished command are byte-identical, so the
	// foreground question is unanswerable here, but "a human typed" is in the index.
	//
	// Only when it is at or after this window's start: a person who typed before the
	// bytes being shown is history the caller has already accounted for.
	if s.humanTyped(stmt.Target, start, total) {
		// "api", NOT "human". The transcript records which ENTRANCE the bytes came
		// through (`i` the HTTP/WebSocket surface, `a` MCP), and the HTTP surface is used
		// by a person in a browser AND by any script that drives /api/term directly. An
		// agent calling the REST entrance was being told "a human typed" about its own
		// command — the header has to name the fact termcp holds, not the one it is
		// guessing at.
		headers["Typed-By"] = "api"
	}

	// THE HANDLER ALREADY APPLIED THE LIMITS, so this pass must decide from
	// has_more, not from the body's size.
	//
	// It used to test `len(body) > win.BytesLimit` and `lines > win.LinesLimit`,
	// which the handler had just made false: it returns at most bytes_limit bytes and
	// lines_limit lines, so the comparison never held and the fold policy never ran.
	// The observable result was that `on_lines_overflow: fold` and
	// `on_bytes_overflow: fold` returned the truncated window anyway — the parameter
	// was accepted, validated, and then ignored.
	//
	// has_more is the fact the handler DOES report: "the window ended before the
	// stream did". That is exactly the condition the fold policy is about.
	more := boolAny(doc, "has_more")
	truncated := false
	if more && win.BytesLimit > 0 && win.OnBytesOverflow == "fold" {
		body = ""
		truncated = true
	}
	if more && win.LinesLimit > 0 && win.OnLinesOverflow == "fold" {
		body = ""
		truncated = true
	}
	// A FOLD THAT DOES NOT SAY WHAT IT FOLDED IS INDISTINGUISHABLE FROM NOTHING THERE.
	//
	// `on_*_overflow: fold` is the right default for a caller that would rather have
	// a handle than a wall of bytes, but it used to answer 206 with an empty body and
	// a range — and an empty body is also what "no new output" looks like. The caller
	// had to trust that bytes existed somewhere it could not name.
	//
	// The Folded header names the gap: which byte span this reply withheld, and the
	// exact flag that fetches it. That is the difference between "folded on purpose"
	// and "nothing to show", and it costs one header line.
	if truncated && total >= start {
		headers["Folded"] = fmt.Sprintf("bytes %d-%d of %d; read --cursor %d", start, total-1, total, start)
	}

	code := 200
	hasBytes := len(body) > 0 || end > start
	switch {
	case hasBytes && (truncated || boolAny(doc, "has_more")):
		code = 206
	case !hasBytes:
		code = 204
	}

	// WAITED ANSWERS "DID YOU ACTUALLY WAIT OR DID YOU SNAP-FAIL?" (桩 5).
	//
	// A 204 that arrives in 0.03ms is a failure (the socket died, the reader broke,
	// the window never ran); a 204 that arrives in 2000ms is legitimate silence.
	// Both used to look identical. The header states the actual time elapsed so a
	// caller can distinguish "waited and nothing arrived" from "dropped out
	// instantly", which is the one difference that decides whether to retry.
	//
	// SAMPLED NAMES THE PTY CONTRACT (误读 3).
	//
	// An empty read of a pty is a sample of a live shell that outlives every command;
	// it does not mean the command exited and produced nothing. Only on 204: on a 200
	// the output speaks for itself, and on pipe the exit state is observable.
	if waited > 0 {
		headers["Waited"] = fmt.Sprintf("%dms", waited.Milliseconds())
	}
	// A 204 WITH NO WAIT IS THE ONE REPLY THAT LOOKS LIKE A BUG.
	//
	// An empty read of a live shell is legitimate — the command may simply not have
	// printed yet — but `Waited: 0ms` says that in a technical voice, in the header
	// block a caller skims last. The reported reaction was to suspect the input had
	// been dropped and spend a call proving it had not. Naming the flag is what turns
	// that into an obvious next move.
	//
	// A HEADER, not a body sentence: this reply is a 204, and 204 must not carry a
	// body — the REST entrance would drop it and only MCP would show the hint, which
	// is the silent divergence between the two entrances this repo keeps paying for.
	// It appears only on the empty-and-unwaited case, so a read with output never pays.
	if code == 204 && waited == 0 && s.ptyRunning(stmt.Target) {
		headers["Hint"] = "nothing yet — a pty is still running. Pass timeout=<ms> to wait for output"
	}
	if code == 204 && s.ptyRunning(stmt.Target) {
		headers["Sampled"] = "true"
	}
	if total >= 0 && (code == 206 || code == 204) {
		if code == 204 {
			headers["Content-Range"] = envelope.UnsatisfiedRange(total)
		} else {
			headers["Content-Range"] = envelope.ContentRange(start, end-1, total)
		}
	} else if code == 200 && total > 0 {
		headers["Content-Range"] = envelope.ContentRange(start, end-1, total)
	}

	// Status and Exit-Code only when they are NEWS: a channel that has closed shows
	// its exit state once and stays that way, so every later read of it repeats the
	// same two lines consistently. "still running" is what an ordinary reply would
	// say, which is exactly what makes it noise — and a live channel has no exit
	// code to report, because a shell never tells the pipe outside it that a
	// command inside it finished.
	// "DID IT FINISH?" IS THE QUESTION A LONG CALL HAS, AND ABSENCE WAS THE ANSWER.
	//
	// A window that expires while the program is still running used to produce a
	// reply identical to one where the program finished immediately and printed
	// nothing: 204, no status, no hint. A caller polling for a 180-second nvme
	// self-test had no way to tell "still working" from "done, silent" and kept
	// re-reading a channel that was already finished.
	//
	// still_running is that fact, stated once. It is not an error — the command was
	// accepted and is running — so the code stays whatever the window said.
	if status, exit, closed := s.channelExitState(stmt.Target); closed {
		headers["Status"] = status
		if exit != nil {
			headers["Exit-Code"] = fmt.Sprint(*exit)
			// 126 AND 127 ARE TWO ANSWERS, AND THE SHELL'S OWN WORDS DO NOT TELL THEM
			// APART (issue #191). "permission denied" covers both "this file has no
			// execute bit" and "this filesystem is mounted noexec", and the caller who
			// reads it starts chmod-ing a file that is already 755. "command not
			// found" is a PATH answer. Neither is a fact about the terminal, so it is
			// stated rather than guessed at: the two causes are named, and the mode is
			// one command away (`--op stat` reports it).
			if *exit == 126 {
				headers["Hint"] = "exit 126: the program exists but could not be executed. Check its mode " +
					"(`get <session> file <path> --op stat` reports mode/uid/gid); a filesystem mounted noexec " +
					"refuses execution even at 755, which `mount` on the remote will show."
			} else if *exit == 127 {
				headers["Hint"] = "exit 127: the shell could not find that program — check the name and the " +
					"remote PATH, not the file's permissions."
			}
			// WHO ASKED DECIDES WHETHER A NON-ZERO EXIT IS THIS REPLY'S FAILURE.
			//
			// `run` reports the command it just sent, so a non-zero exit is news
			// about THIS call and 500 is right. `read` reports a transcript, and the
			// transcript is readable whether or not the program in it succeeded —
			// turning that into 500 made a successful read look broken, and dragged
			// every other statement in the same cmd down with it (isError is a
			// property of the whole call).
			//
			// The exit code stays in its header either way, and Error-Code still names
			// it, so a caller that wants to treat it as a failure has the fact. What
			// it no longer gets is a status that says the READ failed.
			if code < 400 && *exit != 0 {
				// The command ran and the stream was read successfully. Its non-zero
				// status is a failing slot fact, not a failed request: callers can
				// inspect Exit-Code / Error-Code, while `;` and `&&` continue to work
				// according to the documented request-level status contract (#240).
				headers["Error-Code"] = "child_exit"
				if stmt.Op != "read" {
					headers["Error-Code"] = "command_failed"
				}
			}
		}
	} else if (stmt.Op == "run" || stmt.Op == "type") && s.pipeRunning(stmt.Target) {
		// PIPE ONLY, because only a pipe has a completion signal to miss.
		//
		// A pipe channel's run waits for its process to exit, so returning while it
		// is still running means the window expired mid-command: the caller's 180
		// second job is still going and the bytes so far are all there is. A pty
		// channel's run always waits its window and the shell outlives every command,
		// so the same flag would be permanently true and say nothing.
		//
		// NOT an Error-Code: nothing failed. The command was accepted and is running.
		headers["Still-Running"] = "true"
	}
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// channelExitState reports a handle's terminal state.
//
// closed is false while any part of the channel is still running, which is the
// common case and the one that must stay silent.
func (s *Server) channelExitState(handle string) (status string, exit *int, closed bool) {
	if s.sessMgr == nil {
		return "", nil, false
	}
	// A handle is a raw shell id, a session id, a locator naming either, or the
	// short `sid:N` that ps prints — every spelling locator.SplitHandle knows,
	// because a handle the caller had to copy out of a reply must not be silent
	// here: returning "still running" for a channel this simply failed to find is
	// how a dead channel keeps reporting itself alive.
	h := locator.SplitHandle(handle)
	if h.Index > 0 {
		// A channel was named explicitly, so that channel IS the question — the
		// session-level fallbacks below would answer about a different one.
		if sess := s.sessMgr.Get(h.SessionID); sess != nil {
			if cs, err := s.shellFromIndex(sess, h.Index); err == nil && cs != nil {
				info := cs.Info()
				if info.Status == "exited" || info.Status == "error" {
					return string(info.Status), info.ExitCode, true
				}
			}
		}
		return "", nil, false
	}
	if cs := s.sessMgr.GetChildShell(handle); cs != nil {
		info := cs.Info()
		if info.Status == "exited" || info.Status == "error" {
			return string(info.Status), info.ExitCode, true
		}
		return "", nil, false
	}
	if sess := s.sessMgr.Get(handle); sess != nil {
		info := sess.Info()
		if info.Status == "exited" || info.Status == "error" {
			return string(info.Status), info.ExitCode, true
		}
		// A session's exit is the session's; we do not look into its primary
		// shell. Every channel that needs its exit reported passes a channel handle
		// above (`sid:N`), which resolves on its own.
		return "", nil, false
	}
	return "", nil, false
}

// foldOrTruncate applies an overflow policy to a byte-truncated body.
func foldOrTruncate(body string, limit int, policy string) string {
	if policy == "fold" {
		return ""
	}
	return body[:limit]
}

// foldOrTruncateLines applies an overflow policy to a line-truncated body.
func foldOrTruncateLines(body string, limit int, policy string) string {
	if policy == "fold" {
		return ""
	}
	var b strings.Builder
	count := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		if count == limit {
			break
		}
		b.WriteString(line)
		if strings.HasSuffix(line, "\n") {
			count++
		}
	}
	return b.String()
}

// readTimeout is the blocking wait a read gets.
//
// IT USED TO CAP AT 60 SILENTLY, which made one parameter mean two things: `run` waited
// the caller's full window and `read` waited at most a minute, with nothing in the reply
// saying which bound had applied (issue #161, P1-3). The cap is gone: the budget passed
// here is already what is LEFT of the caller's window (see readSlot), so the number is
// the caller's own and no second, hidden one is needed.
//
// Zero stays zero: a non-blocking read really is non-blocking.
func readTimeout(win termArgs) float64 {
	if win.TimeoutSeconds <= 0 {
		return 0
	}
	return win.TimeoutSeconds
}

// cursorRead reports whether this read is the live-cursor form — no byte offset and no
// line selector — which is the only form that WAITS for new bytes. The others copy a
// range that is already on the stream, so a shorter budget would change nothing there.
func cursorRead(args map[string]any) bool {
	for _, key := range []string{"offset", "from_line", "tail_lines"} {
		if _, ok := args[key]; ok {
			return false
		}
	}
	return true
}

// readBudget is how long this read may block: what is left of the caller's window,
// bounded by the channel's own quiet window.
//
// A READ WITH NOTHING TO SHOW DOES NOT SPEND THE WHOLE WINDOW (issue #307). `read <ch>`
// on a channel with no new bytes blocked for the caller's timeout to the millisecond —
// 20001ms for a 20s window, 45001ms for 45s, measured — and a caller whose client
// enforces a watchdog at that same number got "Request timed out" instead of the 204 the
// server was about to send, losing the cursor and the state headers with it. Waiting is a
// state, not a duration: once this channel's stream has stood still for the quiet window
// the server already uses to call a pty command done, more waiting answers nothing.
//
// `--wait` asks for the duration anyway: a caller that really is long-polling says so,
// and spends its own ceiling rather than having one picked for it. The reply's `Waited`
// reports what was spent either way.
//
// Only the cursor form is capped. A positional read answers immediately, and a tail on a
// live pipe waits for the process itself (see awaitExit), so neither has a window to
// shorten.
func (s *Server) readBudget(stmt directive.Statement, args map[string]any, win termArgs, waited time.Duration) (float64, bool) {
	budget := readTimeout(remaining(win, waited))
	if budget <= 0 || !cursorRead(args) || boolFlag(stmt, "wait") {
		return budget, false
	}
	quiet := s.quietTracker(stmt.Target).threshold().Seconds()
	if quiet >= budget {
		return budget, false
	}
	return quiet, true
}

// onlyEcho reports whether the bytes in [start, now) are nothing but the pty's echo of the
// line we just typed.
//
// THE ECHO IS A COPY OF INPUT THIS SERVER ALREADY HAS, so recognising it needs no
// heuristics: strip the line endings the terminal adds and compare with what went out. A
// wrapped echo (a payload wider than the terminal) does not compare equal and is reported
// as output — the conservative answer, because guessing wrong there would suppress a
// Still-Running header that was real.
func (s *Server) onlyEcho(handle string, start, now int64, payload string) bool {
	if payload == "" {
		return false
	}
	text, ok := s.peekRaw(context.Background(), handle, start, now)
	if !ok {
		return false
	}
	// A COPY OF THE PAYLOAD SPLIT BY THE TERMINAL'S LINE ENDINGS. Rejoining on the two
	// characters a tty inserts is what makes a canonical-mode echo compare equal without
	// letting unrelated output through.
	joined := strings.NewReplacer("\r\n", "", "\r", "", "\n", "").Replace(text)
	return joined == strings.NewReplacer("\r\n", "", "\r", "", "\n", "").Replace(payload)
}
