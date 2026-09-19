package session

import (
	"context"
	"io"
	"sync"
)

// ErrScopeReleased is the cause stored on the scope context once Release runs.
var ErrScopeReleased = context.Canceled

// ResourceScope is a session-scoped lifecycle container: a cancelable context
// plus a LIFO stack of cleanup registrations. Everything that is born from a
// Session — forwards, notification rules, readers, minted credentials — is
// registered here at creation time, so tearing down the session tears down all
// of them without the session knowing their concrete types. This replaces the
// earlier "hard-coded cleanup list" anti-pattern where every subsystem had to
// be remembered and closed by name.
//
// Usage:
//
//	scope := newResourceScope()
//	scope.Add(func() { notifyMgr.Unregister(ruleID) })
//	scope.AddCloser(transcriptFile)
//	...
//	scope.Release() // cancels ctx, runs every registration LIFO, exactly once
type ResourceScope struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelCauseFunc
	cleanup []func()
	closed  bool
}

func newResourceScope() *ResourceScope {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &ResourceScope{ctx: ctx, cancel: cancel}
}

// Context returns the scope's cancelable context. Long-lived goroutines and
// network loops bound to this session select on ctx.Done() to stop themselves
// when the session is released.
func (s *ResourceScope) Context() context.Context {
	return s.ctx
}

// Done is closed once the scope has been released. It is the context-aware
// counterpart of Context().Done().
func (s *ResourceScope) Done() <-chan struct{} {
	return s.ctx.Done()
}

// Err returns the release cause, or nil while the scope is still alive.
func (s *ResourceScope) Err() error {
	select {
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	default:
		return nil
	}
}

// Add registers a cleanup function to run when the scope is released. If the
// scope is already released the function runs immediately (registration races
// with release must not leak). Functions run in LIFO order.
func (s *ResourceScope) Add(fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		fn()
		return
	}
	s.cleanup = append(s.cleanup, fn)
}

// AddCloser registers an io.Closer to be closed on release.
func (s *ResourceScope) AddCloser(c io.Closer) {
	if c == nil {
		return
	}
	s.Add(func() { _ = c.Close() })
}

// Release cancels the context and runs every registered cleanup in LIFO order.
// It is idempotent: subsequent calls are no-ops.
func (s *ResourceScope) Release() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	list := s.cleanup
	s.cleanup = nil
	s.mu.Unlock()

	s.cancel(ErrScopeReleased)
	for i := len(list) - 1; i >= 0; i-- {
		func() {
			defer func() { recover() }() // a misbehaving cleanup must not break teardown
			list[i]()
		}()
	}
}
