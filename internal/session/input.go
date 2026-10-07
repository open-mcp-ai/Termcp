package session

import (
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Lock ordering: shellStateMu -> mu -> stdinMu (and shellStateMu -> cs.mu).
// Never acquire in reverse order. s.mu and cs.mu are leaves; shellStateMu is
// only taken by session-level transitions (close/DEAD/terminate, new shells).
// The manager-assigned callbacks and the per-shell closed flag are atomics and
// need no lock (see field docs below).

// InputSource identifies who produced a keystroke sequence, which decides the
// status recorded for those bytes in the shell's log.
//
// The distinction is the reason the log carries a status at all: a transcript
// that cannot say "the agent typed this" versus "the human typed this" cannot be
// replayed or audited meaningfully.
type InputSource int

const (
	// InputFromAPI is a human typing through the HTTP/WebSocket API.
	InputFromAPI InputSource = iota
	// InputFromAI is an AI agent driving the shell through MCP.
	InputFromAI
)

// logStatus maps an input source onto the status stored in log.jsonl.
func (s InputSource) logStatus() api.LogStatus {
	if s == InputFromAI {
		return api.LogAIInput
	}
	return api.LogAPIInput
}

// appendEnter returns data with the line ending appropriate for the shell family.
func appendEnter(data []byte, crlf bool) []byte {
	if crlf {
		return append(append([]byte(nil), data...), '\r', '\n')
	}
	return append(append([]byte(nil), data...), '\n')
}

// logInput records that input happened, and reports it to the live status
// display.
//
// **Only a submitted line is recorded in the log.** A keystroke that leaves the
// line open changes nothing: the prefix of a line is not a span of the
// transcript, and its echo is not the shell's output either — the terminal is
// merely repeating what was typed. Marking the moment typing started would put an
// input bar on the timeline for a command the operator is still composing, and
// would capture the bytes in between: output from a command still running would
// be relabelled as part of the input, shortening the output span that follows.
// Waiting for the enter key makes one line exactly one mark, which is also what
// keeps the log from fragmenting: the echo of a long line arrives as dozens of
// separate reads, every one of them carries output status, so they extend the
// output span already open instead of cutting it into strips.
//
// The line's bytes reach log.bin through that echo, so nothing is written here:
// adding the bytes too would duplicate every keystroke on replay. `log.bin` stays
// byte-for-byte equal to what the screen showed.
//
// The live status display is told about *every* input event, not only submitted
// ones: whether a line was submitted is exactly what the tab's chip needs in order
// to stay steady while someone types. That event is reported before the write,
// since it must not be hostage to the write succeeding, and it is the only way
// the browser can learn that an agent — whose keystrokes never touch the page — is
// driving.
func (cs *ChildShell) logInput(src InputSource, submit bool) {
	p := cs.parent
	if p == nil {
		return
	}
	if fn := p.onInput.Load(); fn != nil {
		(*fn)(cs.ID, src, submit)
	}
	// A partial line is not a transcript span: report it to the chip and stop.
	if !submit {
		return
	}
	if p.msgMgr == nil {
		return
	}
	_ = p.msgMgr.AppendMarkOnly(p.ID, cs.ID, src.logStatus())
}

// submitsLine reports whether a write ends the line being typed, which is what
// the channel status chip needs and what the byte stream cannot tell it.
//
// It looks at the *last* byte rather than at "contains a line ending": a paste
// of several lines arrives as one write whose final byte is the only one that
// ends the line, and counting an interior newline as a submit would report a
// half-typed command as submitted.
//
// Besides the line endings themselves, the control characters that abandon a
// line count as ending it: ctrl+c aborts the command, ctrl+d sends EOF, ctrl+z
// suspends it. They matter beyond elegance — a chip that only ever left "typing"
// on an enter would sit on "typing" for the rest of the session after an
// interrupt, which is a common thing to do to a running command.
func submitsLine(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	switch payload[len(payload)-1] {
	case '\r', '\n', 0x03, 0x04, 0x1a:
		return true
	}
	return false
}
