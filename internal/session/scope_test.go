package session

import (
	"sync"
	"sync/atomic"
	"testing"
)

// THE CONTRACT: every registered cleanup runs exactly once, and one registered
// while Release is in flight is not lost — it either lands on the list Release
// takes or runs immediately. The CAS transition is what makes both true at once.
func TestScopeRunsEveryCleanupExactlyOnce(t *testing.T) {
	const n = 200
	s := newResourceScope()

	var mu sync.Mutex
	order := make([]int, 0, n)
	run := make([]atomic.Int32, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Add(func() {
				run[i].Add(1)
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
			})
		}(i)
	}
	// Release races the registrations: half land before it, half after.
	s.Release()
	wg.Wait()

	for i := 0; i < n; i++ {
		if got := run[i].Load(); got != 1 {
			t.Fatalf("cleanup %d ran %d times, want exactly 1", i, got)
		}
	}
	// LIFO for the ones Release took; the immediate ones ran at their own time, so
	// only the count is asserted for order beyond "no duplicates".
	if len(order) != n {
		t.Fatalf("%d cleanups recorded, want %d", len(order), n)
	}
}

// A registration after release runs immediately rather than being dropped.
func TestScopeRunsALateCleanupImmediately(t *testing.T) {
	s := newResourceScope()
	s.Release()

	ran := false
	s.Add(func() { ran = true })
	if !ran {
		t.Fatal("a cleanup registered after Release did not run")
	}
}

// Release is idempotent: a second call runs nothing again.
func TestScopeReleaseIsIdempotent(t *testing.T) {
	s := newResourceScope()
	var calls atomic.Int32
	s.Add(func() { calls.Add(1) })
	s.Release()
	s.Release()
	if got := calls.Load(); got != 1 {
		t.Fatalf("cleanup ran %d times across two releases, want 1", got)
	}
}
