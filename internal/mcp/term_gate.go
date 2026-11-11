package mcp

import (
	"strings"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// A COMPOUND LINE WHOSE SEGMENTS DO NOT ALL ARRIVE IS A DIFFERENT COMMAND (#311).
//
// The hazard is not "the line is lost". A lost line is a lost line. The hazard is that
//     ping host; cd ~/work; rm -rf *
// is one meaning built out of three, and the safety of the last segment is a property of
// the FIRST one: lose the `cd` and the `rm` runs somewhere else, with no error and no
// sign, because `;` does not stop on failure either. Losing a segment does not shorten
// such a line, it RE-BINDS the remainder.
//
// THE FIRST VERSION OF THIS GUARD REFUSED EVERY COMPOUND LINE ON A CHANNEL WITHOUT A
// `Completion` HEADER, and that turned out to be a default rather than a boundary: a pty
// `run` never produces `Completion` on its own, so the rule became "compound lines need
// --force on a pty, forever". Five of this repository's own tests had to be edited to keep
// passing — the shape of a product default changing, not of an edge being covered.
//
// So the decision is made on the evidence the server actually has, in three tiers:
//
//	Alt-Screen: yes    the program owns the terminal. A shell line typed now is keys to
//	                   that program, so a compound line is refused — observation, not
//	                   inference: the grid exists because the program took the screen.
//	Still-Running      bytes were arriving when the last window ended: something is
//	                   executing, so a compound line is very likely to land half-delivered.
//	                   Also observation.
//	otherwise          nothing is known. The line is sent, and the reply carries a Warning:
//	                   a claim the server cannot make must not be a refusal the caller
//	                   cannot argue with.
//
// A caller who knows better always has `--force`.

// compoundSeparators are the bytes that turn one line into more than one meaning.
//
// The list over-approximates on purpose: a `;` inside quotes is not a separator, and
// telling the two apart needs a parser per target shell (POSIX, PowerShell, cmd) — which
// would be a second grammar to keep in sync with three hosts. Over-approximating costs a
// Warning on `echo "a;b"`, which is a line the caller can read past; under-approximating
// would let the `rm -rf *` case through, which costs the working directory.
func compoundSeparators(payload string) (string, bool) {
	for _, sep := range []string{"&&", "||", ";", "|", ">", "<", "\n", "\r"} {
		if strings.Contains(payload, sep) {
			return sep, true
		}
	}
	return "", false
}

// runProof is what a run's reply proved, recorded for the next run to consult.
type runProof struct {
	// confirmed: the reply carried a `Completion` header — the caller's own `--until`
	// matched, or the remote shell reported the command ended.
	confirmed bool
	// busy: the reply said Still-Running, which means bytes were still arriving when the
	// window ended. That is observation of work, not an inference about it.
	busy bool
}

// noteRunProof records what a run's reply proved.
func (s *Server) noteRunProof(handle string, confirmed, busy bool) {
	s.runEvidence.Store(handle, runProof{confirmed: confirmed, busy: busy})
}

// lastRunProof reports what this channel's last run proved, if anything.
//
// KNOWN IS FALSE UNTIL A RUN HAS HAPPENED: refusing the first compound line on a fresh
// channel would refuse the normal case to guard against a state that only exists after a run.
func (s *Server) lastRunProof(handle string) (runProof, bool) {
	v, ok := s.runEvidence.Load(handle)
	if !ok {
		return runProof{}, false
	}
	p, _ := v.(runProof)
	return p, true
}

// compoundVerdict is the whole decision, as a pure function: what to do about a compound
// line, given what is known. Kept apart from the I/O so the table is testable without a
// terminal.
func compoundVerdict(force, compound, altScreen, busy, confirmed, known bool) (refuse string, warn bool) {
	if force || !compound {
		return "", false
	}
	switch {
	case altScreen:
		return "a full-screen program owns the terminal", true
	case busy:
		return "the previous command was still producing output when its window ended", true
	case !known || !confirmed:
		// NOT A REFUSAL: nothing here says a program is holding the terminal, and a 4xx
		// that rests on "we could not tell" is the kind of guess this server does not make.
		return "", true
	}
	return "", false
}

// compoundGate runs the table above against the channel, before anything is typed (a 4xx
// must not arrive after the side effect — issue #179). It returns the warning to attach to
// the reply when it let the line through.
func (s *Server) compoundGate(stmt directive.Statement) (warn string, fail *toolFailure) {
	if stmt.FlagState("force") != directive.FlagAbsent {
		return "", nil
	}
	// PIPE CHANNELS ARE NOT THIS HAZARD: their bytes go to a process's stdin, not to a
	// terminal's line editor, so there is no queue that hands a half-typed line to the
	// shell afterwards.
	if !s.ptyRunning(stmt.Target) {
		return "", nil
	}
	sep, compound := compoundSeparators(stmt.Payload)
	if !compound {
		return "", nil
	}
	proof, known := s.lastRunProof(stmt.Target)
	reason, warned := compoundVerdict(false, compound, s.channelInAltScreen(stmt.Target), proof.busy, proof.confirmed, known)
	if reason != "" {
		return "", &toolFailure{code: CodeConflict,
			msg: "this line is compound (`" + sep + "`) and " + reason + ". Part of it would be taken as input " +
				"by whatever holds the terminal and the rest executed on its own — `cd dir; rm -rf *` loses the " +
				"`cd` and the rest runs somewhere else, silently. Send the segments separately, pass `--until` so " +
				"the channel has evidence, or `--force` to type it anyway."}
	}
	if warned {
		return "this line is compound (`" + sep + "`) and the previous command on this channel left no evidence " +
			"that it finished. Nothing observed says a program is holding the terminal, so the line was sent — " +
			"but if it is answered by something other than the shell, part of it was input, not a command.", nil
	}
	return "", nil
}
