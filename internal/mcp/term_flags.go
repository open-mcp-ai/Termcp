package mcp

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/encoding"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// checkRunFlags validates everything a run reads from its own statement, before
// anything is typed or spawned.
//
// THE WINDOW IS CHECKED BEFORE ANYTHING RUNS, because a 400 must not be a lie. The
// window is validated where it is read — inside shell_output — and that read happens
// after the command has been typed and Enter pressed. So a refusal about the window
// used to arrive after the command had already run (issue #179), and a 4xx reads as
// "nothing happened": the caller retries, and the command runs twice.
//
// THE FILTERS ARE THE SAME KIND OF FACT, and they used to be checked in two places for
// two roads: a channel run let a broken `--grep` through until the read failed, while a
// use run had a private validator that caught it first. That is the definition of
// a rule with two implementations — one road refuses before the side effect, the other
// after it, and nobody compares them. One rule, both callers, before either acts.
//
// It builds the argument map readSlot would build and hands it to the same parser, so
// the two cannot drift. `timeout` here is the caller's whole window rather than what is
// left of it: the remainder is always smaller, so a window that passes here cannot be
// refused later, and one refused here would have been refused there.
func (s *Server) checkRunFlags(stmt directive.Statement, win termArgs) *toolFailure {
	args := argsOf("shell_id", stmt.Target)
	args["max_bytes"] = float64(win.BytesLimit)
	args["max_lines"] = float64(win.LinesLimit)
	args["timeout"] = win.TimeoutSeconds
	if _, bad := parseReadParams(args); bad != nil {
		return failureFromResult(bad)
	}
	if pattern, ok := stmt.Flag("grep"); ok {
		if _, err := regexp.Compile(pattern); err != nil {
			return &toolFailure{code: CodeInvalidArgument,
				msg: "`--grep` is a Go regular expression and this one does not compile: " + err.Error()}
		}
	}
	if _, bad := s.untilFrom(stmt); bad != nil {
		return bad
	}
	// ONE WORDING FOR ONE RULE. The run pre-flight rejected a non-positive limit as
	// "must be a positive integer" while the search said "must be a number", and a caller
	// reading both replies could not tell whether it had made one mistake or two.
	for _, flag := range []string{"grep-limit", "tail"} {
		if v, ok := stmt.Flag(flag); ok && v != "" {
			if n, err := strconv.Atoi(v); err != nil || n <= 0 {
				return &toolFailure{code: CodeInvalidArgument,
					msg: "`--" + flag + "` must be a positive integer, got " + strconv.Quote(v)}
			}
		}
	}
	// A CHARSET IS CHECKED ONCE, HERE: decoding is what later reads do to every byte, so
	// an unknown name is a mistake about the whole call, not about one reply.
	if charset, ok := stmt.Flag("encoding"); ok && charset != "" {
		if _, err := encoding.Decode(charset, nil); err != nil {
			return &toolFailure{code: CodeInvalidArgument, msg: err.Error()}
		}
	}
	return nil
}

// outputSizeFor reads a handle's current stream length, which is where a command's
// own output begins.
func (s *Server) outputSizeFor(handle string) (int64, bool) {
	sess, bad := s.requireShell(handle)
	if bad != nil {
		return 0, false
	}
	size, err := s.sessMgr.OutputSize(sess.ParentSessionID(), sess.ID)
	if err != nil {
		return 0, false
	}
	return size, true
}

// getFloatAny reads a number from a JSON document.
func getFloatAny(m map[string]any, key string, def float64) float64 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}

// numericFlagProblem is the 400 for a numeric flag whose value does not parse.
//
// The flag name and the offending text are both in the message, because the caller
// wrote a command line and needs to see WHICH word was wrong — "invalid argument"
// on a fifty-flag directive is not actionable.
func numericFlagProblem(stmt directive.Statement, flag string, err error) envelope.Slot {
	return badRequest(stmt, "--"+flag+" "+err.Error())
}

// boolAny reports whether a document field is the literal true.
func boolAny(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

// flagNumber parses a numeric flag for a handler's JSON arguments.//
// IT DOES NOT FALL BACK. The previous version returned 0 for anything it could not
// parse, and 0 is a legal value nearly everywhere it lands: `resize abc xyz` became
// rows=0/cols=0, `--tail abc` became "no tail", `--grace abc` became "no grace".
// Each of those then flowed into a handler whose own default (24x80, the default
// window, the default grace) turned the caller's typo into a DIFFERENT successful
// operation, reported as 2xx. The fallback was not protecting anything — it was
// deciding, silently, what the caller must have meant.
//
// So the failure is reported here, where the flag is still a flag and the message
// can still name it. The caller of this function is expected to turn the error into
// a 400 for the statement; see numericFlags.
func flagNumber(raw string) (float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("expected a number, got an empty value")
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a number, got %q", trimmed)
	}
	// NaN/Inf parse successfully but are never a meaningful port, size or offset,
	// and passing one on would make every downstream comparison quietly false.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("expected a finite number, got %q", trimmed)
	}
	return f, nil
}
