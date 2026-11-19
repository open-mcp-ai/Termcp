package detach

import (
	"context"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestPIDBounds(t *testing.T) {
	for _, pid := range []int{0, -1, -2} {
		if validPID(pid) || ProcessAlive(pid) {
			t.Errorf("accepted non-process ID %d", pid)
		}
	}
	if strconv.IntSize == 64 {
		upper := uint64(maxPID)
		if !validPID(int(upper)) || validPID(int(upper+1)) {
			t.Error("PID bounds do not match the native type")
		}
	}
}

// The detached helper stays alive until its parent closes stdin, including
// if the parent crashes. No external executable or global pidfile is needed.
func TestDetachedChild(t *testing.T) {
	const childEnv = "TERMCP_DETACH_TEST_CHILD"
	if os.Getenv(childEnv) == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDetachedChild$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	pid := cmd.Process.Pid
	if !ProcessAlive(pid) {
		t.Fatal("detached child is not alive")
	}
	// A regression can only target our private child/group. Never test
	// Terminate(-1) against a real kernel: kill(-1, SIGTERM) broadcasts.
	bad := []int{-pid}
	if strconv.IntSize == 64 {
		bad = append(bad, int(uint64(math.MaxUint32)+1+uint64(pid)))
	}
	for _, n := range bad {
		if ProcessAlive(n) || Terminate(n) == nil {
			t.Fatalf("accepted invalid PID %d", n)
		}
	}
	if err := Terminate(pid); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || ctx.Err() != nil {
		t.Fatalf("child did not exit via termination: wait=%v, context=%v", err, ctx.Err())
	}
}
