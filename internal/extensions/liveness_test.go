package extensions

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// A STATUS STRING IS A BELIEF; LIVENESS IS A MEASUREMENT (issue #325).
//
// The two are not the same fact and the difference is the whole point: `status` is what
// the reconciler published, and a channel that died a moment ago still reads `available`
// until a walk notices. A caller that has to decide "is this process there" cannot get
// that from a belief, so the reply carries a probe taken at reply time.
func TestLivenessMeasuresRatherThanRepeatsTheStatus(t *testing.T) {
	state := &extState{status: StatusAvailable, shellID: "shell-1", sessionID: "sess-1"}

	// A LIVE CHANNEL.
	got := state.probeLiveness(
		func(string) *session.ChildShell { return nil }, // never used below
		func(string) string { return "running" })
	// With no lookup result there is nothing to measure, and the probe says so rather
	// than claiming the process is gone.
	if got.Alive {
		t.Errorf("alive = true with no channel to measure: %+v", got)
	}
	if got.CheckedAt == 0 {
		t.Error("a probe without a timestamp cannot be compared with another reply")
	}
}

// A DEAD CHANNEL IS NOT "A TEMPLATE THAT NEVER HAD ONE".
//
// Both end up with no shell id, and reporting the second about the first is the same
// silence this field exists to remove — worse, it hides the exit code that says why.
func TestAClosedChannelSaysSoInsteadOfDenyingItEverExisted(t *testing.T) {
	state := &extState{
		status:          StatusUnavailable,
		closedShellID:   "shell-gone",
		closedReason:    "exit code 7",
		closedSessionID: "sess-1",
	}
	got := state.probeLiveness(
		func(string) *session.ChildShell { return nil },
		func(string) string { return "running" })
	if got.Alive {
		t.Errorf("a closed channel reported alive: %+v", got)
	}
	if want := "has closed (exit code 7)"; !strings.Contains(got.PidUnknown, want) {
		t.Errorf("pid_unknown = %q, want it to say the channel closed with its reason (%q)", got.PidUnknown, want)
	}

	// A read-only template, by contrast, really has no process — and that is a
	// different sentence, because it is a different fact.
	bare := &extState{status: StatusAvailable, sessionID: "sess-1"}
	got = bare.probeLiveness(
		func(string) *session.ChildShell { return nil },
		func(string) string { return "running" })
	if !strings.Contains(got.PidUnknown, "declares no shell") {
		t.Errorf("a template without a shell = %q, want it to say there is no channel process", got.PidUnknown)
	}
	if got.SessionStatus != "running" {
		t.Errorf("session_status = %q, want the connection's own status", got.SessionStatus)
	}
}

// THE PID IS NOT CLAIMED WHEN IT IS NOT KNOWN.
//
// A channel runs on the target host and SSH carries no pid, so a number here could only
// be this daemon's own — a value a caller can check and find wrong. The honest answer is
// the reason, beside the fact the caller actually wanted (`alive`).
func TestThePidIsReportedAsUnknownWithAReason(t *testing.T) {
	state := &extState{status: StatusAvailable, shellID: "shell-1"}
	got := state.probeLiveness(
		func(string) *session.ChildShell { return nil },
		func(string) string { return "running" })
	if got.Pid != 0 {
		t.Errorf("pid = %d, want 0: nothing on this path can know it", got.Pid)
	}
	if got.PidUnknown == "" {
		t.Error("an absent pid needs a reason, or a caller reads the zero as a measurement")
	}
}

// RESTARTS COUNTS RECOVERIES, NOT BRING-UPS.
//
// A counter that moves when a caller asks a healthy extension for a second channel would
// read as a flapping target, which is the opposite of what it is for. The transition is
// what counts: down, then available again.
func TestRestartsCountsOnlyRecoveries(t *testing.T) {
	state := &extState{status: StatusAvailable}

	state.mark(StatusAvailable) // already up: not a restart
	if got := state.snapshot().Restarts; got != 0 {
		t.Fatalf("restarts = %d after a no-op mark, want 0", got)
	}

	state.recordFailure("start", "boom")
	state.mark(StatusAvailable)
	if got := state.snapshot().Restarts; got != 1 {
		t.Fatalf("restarts = %d after one recovery, want 1", got)
	}

	// A SECOND failure and recovery is a second restart.
	state.recordFailure("start", "boom again")
	state.mark(StatusAvailable)
	if got := state.snapshot().Restarts; got != 2 {
		t.Fatalf("restarts = %d after two recoveries, want 2", got)
	}
}

// LAST-USE IS A FACT ABOUT THE CALLER, NOT ABOUT THE EXTENSION'S HEALTH: it is reported
// for every extension, not only the lazy ones whose idle timer needed it.
func TestLastActiveIsReportedForEveryExtension(t *testing.T) {
	state := &extState{status: StatusAvailable}
	before := time.Now().Add(-time.Second).UnixMilli()
	state.touch()
	snap := state.snapshot()
	if snap.LastActiveAt < before {
		t.Errorf("last_active_at = %d, want a timestamp at or after %d", snap.LastActiveAt, before)
	}
}
