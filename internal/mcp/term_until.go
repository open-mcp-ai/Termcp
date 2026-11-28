package mcp

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// `--until` is the caller naming the evidence that ends a wait.
//
// WITHOUT IT A PTY WAIT ENDS ON A THRESHOLD. ptyQuiet is how long the stream must
// stand still before the wait gives up waiting, and standing still is a measurement
// of the stream, not a statement about the command: a finished command and one that
// is thinking look identical. The reply used to say `Completion: quiet` anyway, which
// turned a measurement into a verdict — and a caller acting on that verdict typed the
// next command into a program that still held the terminal.
//
// `--until` replaces "I waited long enough" with "I saw the thing I was waiting for".
// The caller knows what a finished command looks like on its own host (a prompt, a
// sentinel, the shell's own completion report); the server does not, and no amount of
// silence makes it know. So the predicate is the caller's, and the server's job is to
// observe it and report exactly what it saw.

const (
	// osc133Done is the shell's own completion report: OSC 133 ; D ; <exit> — the
	// sequence FinalTerm defined and iTerm2, kitty, WezTerm, Ghostty, fish 4.x,
	// Starship, oh-my-posh and the Windows Terminal profile emit.
	//
	// IT IS THE ONLY MARKER THAT CARRIES A VERDICT FROM THE EXECUTOR ITSELF: the
	// process that ran the command is the one saying it finished, and the exit status
	// comes with the claim. Two terminators exist (`ESC \` and BEL) and neither is
	// matched here on purpose — the digits end the sequence either way, so a shell
	// that picks the other terminator does not silently miss.
	//
	// A program can print this too. A terminal is a byte stream and this is a claim
	// made in it, which is why the header it produces says `exit`, not "succeeded".
	osc133Done = `\x1b\]133;D;(\d+)`

	// promptEnd is the fallback: a line that ends on a shell prompt character. Every
	// shell has one and none of them announce it, so it is a guess about a shape —
	// but it is a shape the caller can also override with its own regex.
	//
	// THE GLYPH SET IS THE ONE PEOPLE ACTUALLY USE. `$ # >` alone missed the themed
	// prompts — starship, oh-my-posh, powerlevel10k all end on ❯ ➜ λ — and a preset that
	// cannot match the prompt in front of it is a preset that costs the whole timeout on
	// every call (found by the dogfood pass: 10s burned, no match, on a live shell).
	//
	// A TRAILING SGR RESET IS PART OF THE PROMPT. Those themes colour the glyph and then
	// reset, so the line ends `❯ \x1b[m` — the reset is after the space, not before it,
	// which is why allowing only `[ \t]` at the end never matched.
	// TWO FAMILIES OF PROMPT, BOTH MATCHED. A bash-ish prompt puts the glyph last
	// (`user@box:~$ `); a themed one puts it first and ends on a space or a colour reset
	// (`\x1b[38;2;1;2;3m➜  repo `). Matching only the first family meant every
	// `--until prompt` on a starship/p10k shell burned its whole timeout.
	promptEnd = `(?m)(?:[$#>%❯➜λ\x{e0a0}-\x{e0b3}\x{2500}-\x{257f}\x{f400}-\x{f4ff}][ \t]*(?:\x1b\[[0-9;]*m)*\r?$)` +
		// `|(?:^(?:SGR)*[❯➜λ][^\r\n]*[ \t]+(?:SGR)*\r?$)` — a themed prompt puts the glyph
		// first and ends on a space or a colour reset (`➜  repo `).
		`|(?:^(?:\x1b\[[0-9;]*m)*[❯➜λ][^\r\n]*[ \t]+(?:\x1b\[[0-9;]*m)*\r?$)` +
		// A GLYPH IN THE MIDDLE OF THE LINE IS STILL A PROMPT. Themes label the glyph with a
		// path and a git status, so the line really reads `@anonymous ➜ repo git(main) ` — it
		// ends on `)`, and requiring the glyph at either edge recognised only stripped-down
		// themes. The third dogfood round burned a whole `--until prompt` window on exactly
		// this shape. After the glyph only "what it labels" is allowed: no second glyph, and
		// at most a line's worth of it.
		`|(?m)^(?:\x1b\[[0-9;]*m)*[^\r\n]{0,120}?[❯➜λ›»][^❯➜λ›»\r\n]{0,120}[ \t]*(?:\x1b\[[0-9;]*m)*\r?$`
)

// osc133Re is osc133Done compiled once, for the passive scan (#310 P3): a run that did
// not ask for `--until exit` still gets the shell's own report if the shell sent one.
var osc133Re = regexp.MustCompile(osc133Done)

// oscScanTail bounds the passive scan, and it is small on purpose: the shell emits its
// completion report when the command ENDS, so the marker is the last thing in the window
// (nothing but the prompt follows it). A `cat` of a large file must not cost a second
// copy of itself to find a marker that is already at the end, and a marker further back
// than this belongs to a command the window did not cover. Not finding one in the tail
// means "no claim" — the same answer a full scan that missed would give, so the cap
// cannot produce a false statement.
const oscScanTail = 8 << 10

// noteShellCompletion adds the shell's own completion report to a run's reply, when the
// shell sent one and the reply has no verdict yet (#310 P3.1).
//
// ONE FUNCTION FOR BOTH HALVES OF A RUN. `run --grep` reads through readGrepFrom and the
// plain form through readSlotAfter; a machine with shell integration must not answer
// "did it finish, with what code" only when the caller happened to search the output.
func (s *Server) noteShellCompletion(ctx context.Context, stmt directive.Statement, slot *envelope.Slot, start, end int64) {
	if slot.Code >= 400 || s.isPipe(stmt.Target) {
		return
	}
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	} else if slot.Headers["Completion"] != "" {
		// A caller's own `--until`, or an observed process exit, already answered.
		return
	}
	if code, ok := s.shellExitInWindow(ctx, stmt.Target, start, end); ok {
		slot.Headers["Completion"] = "exit"
		slot.Headers["Exit-Code"] = strconv.Itoa(code)
	}
}

// isPipe reports whether the channel is a process rather than a terminal: a pipe's
// completion is its exit, observed directly, and a marker in its bytes would be the
// program talking rather than a shell reporting.
func (s *Server) isPipe(handle string) bool {
	cs, bad := s.requireShell(handle)
	return bad == nil && cs != nil && !s.ptyRunning(handle)
}

// shellExitInWindow looks for the shell's own completion report in a run's window and
// returns the status it carries (#310 P3.1).
//
// IT IS THE SAME EVIDENCE `--until exit` USES, READ WITHOUT BEING ASKED. A shell with
// integration enabled (bash/zsh/fish, starship, oh-my-posh, VS Code) emits
// `OSC 133 ; D ; <exit>` when a command ends; a caller that does not know the marker
// exists never names it, and until now that meant the one machine that could answer
// "did it finish, with what code" precisely was the one where `run` reported nothing.
//
// ABSENCE IS NOT A VERDICT, so nothing here turns a missed scan into a claim: the header
// is set only when a marker was actually read, and `Completion` keeps meaning "here is
// the evidence that produced this".
func (s *Server) shellExitInWindow(ctx context.Context, handle string, from, to int64) (int, bool) {
	if to <= from {
		return 0, false
	}
	if to-from > oscScanTail {
		from = to - oscScanTail
	}
	raw, ok := s.peekRaw(ctx, handle, from, to)
	if !ok {
		return 0, false
	}
	locs := osc133Re.FindAllStringSubmatch(raw, -1)
	if len(locs) == 0 {
		return 0, false
	}
	// THE LAST ONE IS THIS COMMAND'S. A window can hold the markers of commands run
	// before it (a `read`-shaped window, or a run whose start was set earlier), and the
	// newest report is the one that answers "the command I just started".
	code, err := strconv.Atoi(locs[len(locs)-1][1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// untilSpec is one compiled `--until`.
type untilSpec struct {
	raw     string
	re      *regexp.Regexp
	verdict bool // capture group 1 is an exit status: the reply may say more than "matched"
}

// untilFrom compiles the statement's `--until`, or reports why it cannot.
//
// THE COMPILE HAPPENS BEFORE ANYTHING IS TYPED, for the same reason every other run
// flag is checked there (issue #179): a 400 that arrives after the command ran reads
// as "nothing happened", and the caller retries, and the command runs twice.
func (s *Server) untilFrom(stmt directive.Statement) (*untilSpec, *toolFailure) {
	raw, ok := stmt.Flag("until")
	if !ok {
		return nil, nil
	}
	switch raw {
	case "exit":
		return &untilSpec{raw: raw, re: regexp.MustCompile(osc133Done), verdict: true}, nil
	case "prompt":
		return &untilSpec{raw: raw, re: regexp.MustCompile(promptEnd)}, nil
	}
	re, err := regexp.Compile(raw)
	if err != nil {
		return nil, &toolFailure{code: CodeInvalidArgument,
			msg: "`--until` is a Go regular expression (or the preset `exit` / `prompt`) and this one does not compile: " + err.Error()}
	}
	return &untilSpec{raw: raw, re: re}, nil
}

// untilWatch carries the state of one wait's matching.
//
// THE ECHOED COMMAND LINE IS SKIPPED, and that is not a nicety: `run ch "echo DONE"`
// echoes `echo DONE` into the same stream it is about to produce `DONE` in, so a
// matcher that read from the start of the window would answer "matched" before the
// command had run. The first line of the window is always the echo, so the watch
// starts looking after its newline — and until that newline exists there is nothing
// to match, because the only line present is the one the caller typed.
type untilWatch struct {
	spec *untilSpec
	// echo says this call typed a line into a pty, so the first line of the window is
	// the terminal echoing it back and cannot be evidence about the command.
	//
	// IT IS NOT ALWAYS TRUE. A `read --until` types nothing: its window may start
	// anywhere, and the first line of THAT window is output the caller asked for.
	// Skipping it unconditionally made a read wait out its whole window while the
	// evidence sat on line one (found on a Linux runner, where the read's window began
	// exactly at the marker).
	echo bool
	// echoText is what this call typed, so the echo can be recognised instead of assumed
	// to be "the first line".
	echoText string
	matched  bool
	exitCode *int
}

// peekWindow bounds how much is fetched for one match attempt. A marker is a handful
// of bytes, and the echo of a command can be long, so a sliding tail covers both
// without re-reading a whole scrollback on every poll.
const peekWindow = 4096

// awaitUntil waits for the caller's evidence, bounded by the caller's window.
//
// IT RETURNS WHETHER THE EVIDENCE ARRIVED. Nothing else: an expired window is not a
// verdict about the command, so this reports no completion at all — the reply says
// what the wait did (and does not) know.
func (s *Server) awaitUntil(ctx context.Context, handle string, win termArgs, start int64, w *untilWatch) bool {
	limit := time.Duration(win.TimeoutSeconds * float64(time.Second))
	// A ZERO WINDOW IS A SNAPSHOT, not a wait: `timeout: 0` already means "do not
	// block" everywhere else, so it means the same here — look once, answer honestly.
	deadline := time.Now().Add(limit)
	for {
		if matched, ok := s.matchUntil(ctx, handle, start, w); ok && matched {
			return true
		}
		if limit <= 0 || !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(ptyPoll):
		}
	}
}

// matchUntil tests the bytes this command has produced so far.
func (s *Server) matchUntil(ctx context.Context, handle string, start int64, w *untilWatch) (bool, bool) {
	if w.matched {
		return true, true
	}
	now, ok := s.outputSizeFor(handle)
	if !ok || now <= start {
		return false, true
	}
	// THE ECHO PASS READS FROM THE START, EVERY LATER PASS READS THE TAIL. The line
	// the caller typed is at the start and only matters until its newline is seen;
	// after that the interesting bytes are always the newest ones, so the fetch stays
	// bounded however long the command runs.
	from := now - peekWindow
	if from < start {
		from = start
	}
	// BEFORE THE ECHO IS LOCATED THE WHOLE WINDOW IS READ, and afterwards the read
	// starts where the output does — never before it, or the echo comes back into view
	// on the next poll.
	if w.echo {
		// FROM THE START, EVERY POLL: a ConPTY repaints the echoed line on a later poll,
		// so a window that had already moved past the echo would see the repaint as output.
		from = start
	}
	text, ok := s.peekRaw(ctx, handle, from, now)
	if !ok {
		return false, false
	}
	// THE ECHO IS RECOGNISED BY ITS TEXT, NOT BY ITS BYTES, and that distinction is a
	// measured bug rather than a preference. A shell that COLOURISES the typed line —
	// PowerShell's PSReadLine, zsh syntax highlighting — interleaves SGR sequences
	// between the words, so the caller's line never appears as one contiguous
	// substring: the byte search found nothing, the echo stayed in the text, and
	// `--until` matched the caller's own command 0.08 s after typing. ConPTY also
	// repaints the echo on a later poll (after the command's output has arrived), so
	// two copies can be present, sometimes concatenated on one line. Comparing LINES,
	// with the colour removed for the comparison only, covers both — a line is what a
	// terminal echoes.
	if w.echo {
		text = dropEchoLines(text, w.echoText)
	}
	loc := w.spec.re.FindStringSubmatch(text)
	if loc == nil {
		return false, true
	}
	w.matched = true
	if w.spec.verdict && len(loc) > 1 {
		if code, err := strconv.Atoi(loc[1]); err == nil {
			w.exitCode = &code
		}
	}
	return true, true
}

// dropEchoLines removes every line that is the terminal drawing the caller's input.
//
// THE COLOUR IS REMOVED FOR THE COMPARISON, NOT FROM THE RESULT: the caller's regex may
// be looking for an ANSI sequence (the shell's own completion report is one), so the
// bytes that are kept stay exactly as they arrived.
func dropEchoLines(text, echo string) string {
	if echo == "" {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.Contains(ansi.Strip(line), echo) {
			// The terminal drawing our input, in whatever colour it chose. A partial
			// echo (no newline yet) is dropped too: nothing after it can be output.
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "")
}

// peekRaw reads [from,to) as the bytes that arrived, without consuming anything.
//
// `raw` is not optional here: the ordinary window strips ANSI, and the shell's own
// completion report IS an ANSI sequence. A matcher reading the stripped text would
// never see the marker and would never say so — the kind of silent never-matching a
// caller cannot debug.
func (s *Server) peekRaw(ctx context.Context, handle string, from, to int64) (string, bool) {
	if to <= from {
		return "", true
	}
	args := argsOf("shell_id", handle, "offset", float64(from), "max_bytes", float64(to-from), "timeout", float64(0))
	// THE READER STRIPS ANSI BY DEFAULT, and `"raw": true` was not the switch it looked
	// like: the shared read contract has no such field, so the flag was ignored and the
	// window came back stripped. The shell's completion report IS an ANSI sequence
	// (OSC 133), so `--until exit` could never match — it reported "did not match within
	// the window" on every shell that sends the marker (issue #313). The switch is
	// `strip_ansi`.
	args["strip_ansi"] = false
	res, fail := s.callTool(ctx, "shell_output", args)
	if fail != nil {
		return "", false
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return "", false
	}
	return getString(doc, "output", ""), true
}

// untilWatchFrom builds a watch from the statement's --until, if any.
func (s *Server) untilWatchFrom(stmt directive.Statement) *untilWatch {
	spec, bad := s.untilFrom(stmt)
	if bad != nil || spec == nil {
		return nil
	}
	// THE ECHO IS ONLY A HAZARD WHEN THIS CALL TYPED SOMETHING. `run` writes a line into a
	// pty and the terminal shows it back; `read` writes nothing, so its window begins
	// with whatever bytes the caller asked for. A pipe does not echo at all.
	return &untilWatch{spec: spec, echo: stmt.Payload != "" && s.ptyRunning(stmt.Target), echoText: stmt.Payload}
}

// `Completion` APPEARS ONLY WITH EVIDENCE, and this is the whole point of the change:
// the header used to be set to `quiet` when the stream simply stopped talking, which
// asserted a verdict the server did not have. A missing `Completion` now means what it
// says — not known — and the two values that exist name the evidence that produced them:
//
//	matched — the caller's own predicate was observed in the stream
//	exit    — the remote shell reported that the command ended, with a status
func (w *untilWatch) headers(slot *envelope.Slot) {
	if w == nil || !w.matched {
		return
	}
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	}
	if w.spec.verdict {
		slot.Headers["Completion"] = "exit"
		if w.exitCode != nil {
			slot.Headers["Exit-Code"] = strconv.Itoa(*w.exitCode)
		}
		return
	}
	slot.Headers["Completion"] = "matched"
	slot.Headers["Until"] = w.spec.raw
}
