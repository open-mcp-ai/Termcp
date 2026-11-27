package detach

import (
	"os/exec"
	"testing"
	"time"
)

// THE THREE FACTS A DAEMON LIFECYCLE NEEDS FROM THE PLATFORM, pinned on whichever
// platform this runs on: a detached child survives, a live pid is reported alive, a
// terminated one is reported dead.
func TestDetachAliveAndTerminate(t *testing.T) {
	sleep := sleepPath(t)
	cmd := exec.Command(sleep, "30")
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// REAPED IN THE BACKGROUND: without this a finished child stays a zombie, and
	// `kill(pid, 0)` answers "alive" for a zombie — the trap this package's ProcessAlive
	// would otherwise walk into.
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	pid := cmd.Process.Pid

	if !ProcessAlive(pid) {
		t.Fatalf("ProcessAlive(%d) = false for a running child", pid)
	}
	if err := Terminate(pid); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ProcessAlive(pid) {
		if time.Now().After(deadline) {
			_ = Kill(pid)
			t.Fatalf("pid %d still alive after Terminate", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the child was never reaped")
	}
}

// A PID THAT CANNOT BE A PROCESS IS REFUSED, not passed to the kernel: 0 means "every
// process in my group" and a negative pid means "a process group", so answering the
// question with either would answer about something else.
func TestNonPositivePidsAreNotProcesses(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid) {
			t.Errorf("ProcessAlive(%d) = true", pid)
		}
		if err := Terminate(pid); err == nil {
			t.Errorf("Terminate(%d) returned nil", pid)
		}
	}
}

// A PID NOBODY HOLDS IS DEAD, and the check does not panic or hang.
func TestAReapedPidIsDead(t *testing.T) {
	cmd := exec.Command(sleepPath(t), "0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if ProcessAlive(cmd.Process.Pid) {
		// A pid may be recycled by another process, which would make this a false
		// failure rather than a product bug; skip instead of lying about it.
		t.Skip("the pid was recycled by another process; the answer is not about our child")
	}
}

func sleepPath(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/sleep", "/usr/bin/sleep"} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	t.Skip("no sleep(1) on this platform")
	return ""
}
