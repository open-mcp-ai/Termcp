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
// THE GUARD IS STRUCTURAL, NOT A GUESS ABOUT THE PROCESS. "Is something in the
// foreground?" cannot be answered from here — the process group lives on the far side of
// the SSH connection and a Windows terminal has no such notion (see runDetail). What CAN
// be known is two facts the server already has:
//
//   - whether the last `run` on this channel ended with evidence (`Completion`) or not;
//   - whether the line about to be typed is simple or compound.
//
// So an unconfirmed channel refuses a compound line, and a confirmed one does not. A
// simple command still passes on an unconfirmed channel: if it is eaten, it is eaten,
// and nothing downstream is re-bound to a mistake.
//
// THIS IS DELIBERATELY NOT "REFUSE EVERYTHING WHEN UNSURE". A refusal that cannot see
// the reason would fire on every batch of `a; b`, and a tool that refuses the ordinary
// case is a tool people work around with `--force`, which removes the guard from the
// case it was built for. The escape hatch exists and says what it costs.

// compoundSeparators are the bytes that turn one line into more than one meaning.
//
// The list over-approximates on purpose: a `;` inside quotes is not a separator, and
// telling the two apart needs a parser per target shell (POSIX, PowerShell, cmd) — which
// would be a second grammar to keep in sync with three hosts. Over-approximating refuses
// a harmless `echo "a;b"` on an unconfirmed channel, which costs one `--force` or one
// `--until`; under-approximating would let the `rm -rf *` case through, which costs the
// working directory.
func compoundSeparators(payload string) (string, bool) {
	for _, sep := range []string{"&&", "||", ";", "|", ">", "<", "\n", "\r"} {
		if strings.Contains(payload, sep) {
			return sep, true
		}
	}
	return "", false
}

// noteRunCompletion records what a run's reply proved, for the next run to consult.
//
// CONFIRMED MEANS A `Completion` HEADER WAS THERE — `matched` (the caller's own
// `--until`) or `exit` (the remote shell said the command ended). Its absence means not
// known, which is the whole point of the header change: the reply no longer dresses a
// measurement up as a verdict, so this gate has exactly one bit to read and it is a real
// one.
func (s *Server) noteRunCompletion(handle string, confirmed bool) {
	s.runEvidence.Store(handle, confirmed)
}

// lastRunConfirmed reports whether this channel's last run proved completion.
//
// KNOWN IS FALSE UNTIL A RUN HAS HAPPENED. An unknown channel is not an unconfirmed one:
// refusing the first compound line on a fresh channel would refuse the normal case to
// guard against a state that only exists after a run.
func (s *Server) lastRunConfirmed(handle string) (confirmed, known bool) {
	v, ok := s.runEvidence.Load(handle)
	if !ok {
		return false, false
	}
	b, _ := v.(bool)
	return b, true
}

// refuseUnconfirmedCompound is the gate, checked BEFORE anything is typed.
//
// A refusal has to arrive before the bytes do, for the same reason every other run flag
// is validated up front (issue #179): a 4xx that arrives after the side effect reads as
// "nothing happened" and invites a retry that repeats it.
func (s *Server) refuseUnconfirmedCompound(stmt directive.Statement) *toolFailure {
	if stmt.FlagState("force") != directive.FlagAbsent {
		return nil
	}
	// PIPE CHANNELS ARE NOT THIS HAZARD. Their bytes go to a process's stdin, not to a
	// terminal's line editor, so there is no queue that hands a half-typed line to the
	// shell afterwards — and a resident pipe (`cat`, a REPL) is never "confirmed" by
	// design, so gating it would refuse the ordinary way to use one.
	if !s.ptyRunning(stmt.Target) {
		return nil
	}
	confirmed, known := s.lastRunConfirmed(stmt.Target)
	if !known || confirmed {
		return nil
	}
	sep, compound := compoundSeparators(stmt.Payload)
	if !compound {
		return nil
	}
	return &toolFailure{code: CodeConflict,
		msg: "this line is compound (`" + sep + "`) and the previous run on " + stmt.Target +
			" ended without evidence that it finished. If a program still holds the terminal, part of this " +
			"line is taken as input by it and the rest is executed on its own — `cd dir; rm -rf *` loses the " +
			"`cd` and the rest runs somewhere else, silently. " +
			"Pass `--until` (e.g. `--until exit` or a prompt pattern) so the channel has evidence before the " +
			"next line goes in, read the channel, or `--force` to type it anyway."}
}
