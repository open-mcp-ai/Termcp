package daemon

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestManagerRecognizesUnreapedLinuxExit(t *testing.T) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfds unavailable: %v", err)
	}
	unix.Close(fd)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait() // deliberately leave the exited child unreaped until the assertion
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := waitFor(ctx, func() (bool, error) { return processExited(cmd.Process.Pid), nil }); err != nil {
		t.Fatal("unreaped child still reported running", err)
	}
}
