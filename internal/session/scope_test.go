package session

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type dummyCloser struct {
	closed bool
}

func (d *dummyCloser) Close() error {
	d.closed = true
	return nil
}

func TestResourceScope_LIFOCleanup(t *testing.T) {
	s := newResourceScope()
	var order []int
	s.Add(func() { order = append(order, 1) })
	s.Add(func() { order = append(order, 2) })
	s.Add(func() { order = append(order, 3) })

	s.Release()

	if len(order) != 3 || order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Fatalf("expected LIFO [3 2 1], got %v", order)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("expected Done() to be closed after Release")
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("expected Canceled, got %v", s.Err())
	}
}

func TestResourceScope_AddCloser(t *testing.T) {
	s := newResourceScope()
	c := &dummyCloser{}
	s.AddCloser(c)

	s.Release()

	if !c.closed {
		t.Fatal("expected closer to be closed on release")
	}
}

func TestResourceScope_LateRegistrationRunsImmediately(t *testing.T) {
	s := newResourceScope()
	s.Release()

	ran := false
	s.Add(func() { ran = true })

	if !ran {
		t.Fatal("expected registration on closed scope to run immediately")
	}
}

func TestResourceScope_Idempotent(t *testing.T) {
	s := newResourceScope()
	count := 0
	s.Add(func() { count++ })

	s.Release()
	s.Release()

	if count != 1 {
		t.Fatalf("expected cleanup once, ran %d times", count)
	}
}

func TestResourceScope_RecoversPanickingCleanup(t *testing.T) {
	s := newResourceScope()
	var ranAfter bool
	s.Add(func() { ranAfter = true })
	s.Add(func() { panic("boom") })

	s.Release()

	if !ranAfter {
		t.Fatal("panicking cleanup broke the rest of the teardown")
	}
}

func TestResourceScope_ConcurrentAddAndRelease(t *testing.T) {
	s := newResourceScope()
	var wg sync.WaitGroup
	var ran sync.Map

	for i := 0; i < 50; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Add(func() { ran.Store(idx, true) })
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Release()
	}()

	wg.Wait()
	s.Release() // second call should be harmless

	count := 0
	ran.Range(func(_, _ any) bool { count++; return true })
	if count != 50 {
		t.Fatalf("expected 50 cleanups executed, got %d", count)
	}
}
