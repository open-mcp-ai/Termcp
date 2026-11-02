package mcp

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// TestAliasTableIsSafeUnderConcurrency pins that the alias table can be written and
// read from several goroutines, which it must be: the MCP transport hands each
// request to its own goroutine and POST /api/term does the same, so `alias x …` in
// one call races the parse of every other statement.
//
// The table used to be a bare map. That is a concurrent map read and write, which
// the Go runtime panics on under load and -race reports as a hard error — and which
// shows up as an alias resolving to a mangled expansion long before it pansics,
// because the window is small and the symptom is wrong output, not a crash.
//
// Verified against the pre-fix shape: with the four lock/unlock pairs replaced by
// no-ops this test reports DATA RACE immediately. A test that only exercised Term
// end-to-end did NOT, because the map operations there are too widely spaced to
// interleave — which is why this drives the table directly.
func TestAliasTableIsSafeUnderConcurrency(t *testing.T) {
	a := newAliasTableForTest(map[string]string{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				name := fmt.Sprintf("a%d", n%4)
				a.set(name, "ls")
				_, _ = a.get(name)
				_ = a.snapshot()
				_, _ = a.Expand(directive.Statement{Op: name, Target: "h"})
				a.drop(name)
			}
		}()
	}
	wg.Wait()
}

// TestLearnModeUsageIsPerReply pins that learn_mode survives concurrent calls.
//
// The usage line used to be de-duplicated through a field on the Server
// (s.currentReply), assigned per call. Two calls in flight overwrote each other's
// bookkeeping — a data race that could ALSO drop a caller's Usage line, so the bug
// was invisible when it did not crash. The field is gone; the accumulator is local
// to runStatements, and each reply carries its own.
func TestLearnModeUsageIsPerReply(t *testing.T) {
	s := newTestServer(t)

	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.Term(t.Context(), "ls; info nosuchhandle", map[string]any{"learn_mode": true}).Text()
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		if !strings.Contains(got, "Usage:") {
			t.Errorf("reply %d has no Usage line; learn_mode must be attached per reply:\n%s", i, got)
		}
	}
}
