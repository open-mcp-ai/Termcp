package mcp

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// TestMain shortens the one budget the suite only has to OUTLAST.
//
// shellReadyBudget is a wait whose result its caller ignores: `open` hands over the
// handle whether or not the shell spoke. So nothing here asserts on the real three
// seconds, and paying them once per open that never speaks was most of this package's
// runtime — 240s of a 240s timeout, measured, which is why a slow shuffle order could
// push the package over the wall and fail a run that had no defect in it.
//
// The budget stays at its shipped value in production. Only the tests move it, and they
// move it here, once, rather than in the tests that happen to be slow today.
// treeHelperArg marks this test binary as the session's program: TestMain sees the
// argument, prints its own pid and then sits still, which is what a program that
// ignores a closed channel looks like. Defined here rather than beside the Windows
// test that uses it, because TestMain is compiled on every platform and a constant
// behind a build tag is a build failure on the other two.
const treeHelperArg = "-tree-kill-helper"

func TestMain(m *testing.M) {
	// The test binary doubles as a helper process for the tests that need a program
	// which outlives its parent: TestClosingAChannelEndsItsProcessTree runs this same
	// binary and needs it to report its pid and then sit still. Intercepted before the
	// flag parser, so the marker argument never reaches the testing package.
	for _, a := range os.Args[1:] {
		if a == treeHelperArg {
			_, _ = os.Stdout.WriteString("PID=" + strconv.Itoa(os.Getpid()) + "\n")
			time.Sleep(60 * time.Second)
			os.Exit(0)
		}
	}
	shellReadyBudget = 300 * time.Millisecond
	os.Exit(m.Run())
}
