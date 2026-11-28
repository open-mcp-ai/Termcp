package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/detach"
)

// A MANAGED START SPAWNS, WRITES A PIDFILE, WAITS FOR READINESS, AND REUSES WHAT IS
// ALREADY THERE (issue #309).
//
// The child is `sleep`, because the lifecycle is what this package owns: a real service
// would be tested by the project that knows what it serves. What is asserted here is the
// contract a caller depends on — the pid is alive, the pidfile names it, a second Start
// returns the same process instead of a second one, and Stop removes the file.
func TestStartSpawnsWaitsAndReuses(t *testing.T) {
	dir := t.TempDir()
	ready := make(chan struct{})
	var latch bool
	var latchMu sync.Mutex
	isReady := func() bool {
		latchMu.Lock()
		defer latchMu.Unlock()
		return latch
	}
	m := New(Config{
		Name:    "spawntest",
		DataDir: dir,
		BuildArgs: func() []string {
			return []string{"5"}
		},
		Executable: sleepPath(t),
		Probe: func(ctx context.Context) error {
			// A LATCHED PROBE, not a one-shot channel: readiness is a state that stays
			// true, and a probe that forgot that made the second Start look broken.
			if isReady() {
				return nil
			}
			return errors.New("not ready yet")
		},
		ReadyWait: 10 * time.Second,
		StopWait:  5 * time.Second,
	})

	// READY ARRIVES AFTER TWO POLLS, so Start is genuinely waiting rather than finding
	// the probe already satisfied — a probe that succeeds on the first call would not
	// prove the wait exists.
	go func() {
		time.Sleep(150 * time.Millisecond)
		latchMu.Lock()
		latch = true
		latchMu.Unlock()
		close(ready)
	}()
	inst, err := m.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	if inst.PID <= 0 || !detach.ProcessAlive(inst.PID) {
		t.Fatalf("Start returned a pid that is not alive: %+v", inst)
	}
	if inst.Reused {
		t.Error("the first Start reported a reused instance")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "spawntest.pid"))
	if err != nil {
		t.Fatalf("pidfile: %v", err)
	}
	if got := string(raw); got == "" || got[:len(got)-1] != itoa(inst.PID) {
		t.Errorf("pidfile holds %q, want pid %d", got, inst.PID)
	}
	// THE LOG FILE IS WHERE THE CHILD'S STREAMS GO, and it exists even though `sleep`
	// prints nothing: a caller debugging a silent child needs to open something.
	if _, err := os.Stat(inst.LogFile); err != nil {
		t.Errorf("log file %s: %v", inst.LogFile, err)
	}

	// A SECOND START REUSES: the pid is alive and the probe passes, so there is nothing
	// to spawn. Two daemons on one data dir is the failure this prevents.
	again, err := m.Start(t.Context())
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if !again.Reused || again.PID != inst.PID {
		t.Errorf("second Start = %+v, want reuse of pid %d", again, inst.PID)
	}

	st := m.Status(t.Context())
	if !st.Running || !st.Ready || st.PID != inst.PID {
		t.Errorf("Status = %+v, want running+ready pid %d", st, inst.PID)
	}
}

// STOP REMOVES THE PIDFILE AND ENDS THE PROCESS.
func TestStopEndsTheProcessAndClearsThePidfile(t *testing.T) {
	dir := t.TempDir()
	m := New(Config{
		Name: "stoptest", DataDir: dir, Executable: sleepPath(t),
		BuildArgs: func() []string { return []string{"30"} },
	})
	inst, err := m.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if detach.ProcessAlive(inst.PID) {
		t.Errorf("pid %d is still alive after Stop", inst.PID)
	}
	if _, err := os.Stat(filepath.Join(dir, "stoptest.pid")); !os.IsNotExist(err) {
		t.Errorf("pidfile survived Stop (%v)", err)
	}
	// AND A STOPPED INSTANCE IS NOT REUSED: the next Start spawns a new process.
	next, err := m.Start(t.Context())
	if err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	if next.Reused || next.PID == inst.PID {
		t.Errorf("Start after Stop reused pid %d", inst.PID)
	}
}

// A GRACEFUL STOP IS USED WHEN IT WORKS, and the signal is the fallback when it does not:
// that ordering is what makes HTTP shutdown the default on Windows rather than a special
// case, without losing the process if the request is ignored.
func TestStopPrefersTheConfiguredGracefulPath(t *testing.T) {
	dir := t.TempDir()
	var called bool
	m := New(Config{
		Name: "gracefultest", DataDir: dir, Executable: sleepPath(t),
		BuildArgs: func() []string { return []string{"30"} },
		Stop: func(ctx context.Context) error {
			called = true
			// The graceful path owns the shutdown: here it does nothing, so the fallback
			// has to finish the job after StopWait.
			return nil
		},
		StopWait: 300 * time.Millisecond,
	})
	inst, err := m.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !called {
		t.Error("Stop did not try the configured graceful path first")
	}
	if detach.ProcessAlive(inst.PID) {
		t.Errorf("pid %d survived Stop: the fallback after a no-op graceful stop did not run", inst.PID)
	}
}

// A STALE PIDFILE DOES NOT MAKE A DEAD INSTANCE LOOK RUNNING.
func TestAStalePidfileIsCleanedUp(t *testing.T) {
	dir := t.TempDir()
	// A pid that cannot be alive: a process we started and reaped.
	cmd := exec.Command(sleepPath(t), "0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("reap helper: %v", err)
	}
	dead := cmd.Process.Pid
	if err := os.WriteFile(filepath.Join(dir, "stale.pid"), []byte(itoa(dead)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Name: "stale", DataDir: dir, Executable: sleepPath(t), BuildArgs: func() []string { return []string{"30"} }})
	st := m.Status(t.Context())
	if st.Running {
		t.Fatalf("a dead pid was reported as running: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.pid")); !os.IsNotExist(err) {
		t.Errorf("the stale pidfile was left behind (%v)", err)
	}
}

// A CHILD THAT NEVER BECOMES READY IS CLEANED UP: Start must not leave an orphan holding
// the port or the data directory, because the caller's next move is to fix the problem and
// try again.
func TestAFailedReadyWaitLeavesNoChild(t *testing.T) {
	dir := t.TempDir()
	m := New(Config{
		Name: "nevertest", DataDir: dir, Executable: sleepPath(t),
		BuildArgs: func() []string { return []string{"30"} },
		Probe:     func(context.Context) error { return errors.New("never ready") },
		ReadyWait: 300 * time.Millisecond,
	})
	inst, err := m.Start(t.Context())
	if err == nil {
		_ = m.Stop(context.Background())
		t.Fatalf("Start succeeded with a probe that never passes: %+v", inst)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "nevertest.pid")); !os.IsNotExist(statErr) {
		t.Errorf("pidfile survived a failed Start (%v)", statErr)
	}
}

// sleepPath finds a `sleep` that takes a seconds argument, or skips.
func sleepPath(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/sleep", "/usr/bin/sleep"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no /bin/sleep on this platform")
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
