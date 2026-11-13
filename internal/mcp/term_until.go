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
	// outFrom is the absolute offset where the output begins: the byte after the echoed
	// line. Zero means "not located yet".
	//
	// IT IS AN OFFSET, NOT A ONE-TIME SLICE. Skipping the echo by slicing the text once
	// only skipped it for that poll; the next poll re-read the tail, which still
	// contained the echo, and matched the caller's own command. On a Linux runner that
	// is what made `--until` answer "matched" 0.08s after typing, before the command had
	// printed anything.
	outFrom  int64
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
	if !w.echo || w.outFrom == 0 {
		from = start
	} else if from < w.outFrom {
		from = w.outFrom
	}
	text, ok := s.peekRaw(ctx, handle, from, now)
	if !ok {
		return false, false
	}
	if w.echo && w.outFrom == 0 {
		// COUNT THE ECHO LINES, DO NOT ASSUME THERE IS ONE. A Windows ConPTY echoes the
		// line twice — the pty's copy, then the line editor's repaint — so skipping "the
		// first line" left the second copy sitting in the matchable text and `--until`
		// answered "matched" on the caller's own command. Every leading line that
		// contains what we typed is the echo; the first one that does not is output.
		off, decided := echoSkip(text, w.echoText)
		if !decided {
			// The deciding line is still arriving. Matching the echo, or guessing where
			// it ends, would both be wrong — so there is nothing to answer yet.
			return false, true
		}
		w.outFrom = from + int64(off)
		text = text[off:]
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
	args["raw"] = true
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

// echoSkip reports where the terminal's echo of payload ends: the byte offset after the
// last leading line that contains it.
//
// THE SECOND RETURN IS WHETHER THE BOUNDARY IS KNOWN. A trailing line with no newline yet
// may still be another echo; committing to a boundary there would either match the echo or
// discard output, so the caller waits for the byte that decides it.
func echoSkip(text, payload string) (int, bool) {
	if payload == "" {
		return 0, true
	}
	off := 0
	for {
		nl := strings.IndexByte(text[off:], '\n')
		line := text[off:]
		if nl >= 0 {
			line = text[off : off+nl]
		}
		if !strings.Contains(ansi.Strip(line), payload) {
			// This line is not the echo — and if it is incomplete, it still is not the
			// echo, because an echo line contains the payload from its first bytes.
			return off, true
		}
		if nl < 0 {
			// The echo is still arriving: what comes after it is not known yet.
			return off, false
		}
		off += nl + 1
	}
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
