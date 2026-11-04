package session

import (
	"errors"
	"fmt"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// ErrUnboundedWorkload is what CloseLater answers when a session holds something
// that will never end by itself. It is a sentinel so the caller can turn it into a
// 400 with its own wording rather than string-matching a sentence.
var ErrUnboundedWorkload = errors.New("close --later needs a session that ends by itself")

// CloseLater marks the session as closing and lets its existing work finish.
//
// WHAT "LATER" CAN MEAN. A deferred close is a promise that the session will end
// without anyone touching it again, and only a BOUNDED workload keeps that promise:
//
//   - a pipe channel runs a program that exits, so its end is an event;
//   - a file transfer moves a fixed number of bytes, so its end is an event.
//
// A pty channel has no end event — it is a login shell waiting for someone to type
// `exit`, and the person who would type it is the one asking to close. A port
// forward has no end event either: it lives exactly as long as the session it is
// carried on. Accepting --later for those would not defer the close, it would
// CANCEL it, and the caller would be told 204 while the session stayed up forever.
// So they are refused, and the refusal names the thing that has no end.
//
// WHAT IT DOES NOT DO. It does not terminate anything. The channels that are
// already running keep running and their output keeps arriving; every NEW channel is
// refused from this moment because s.closing is set. When the last one ends,
// OnExit("close") is what actually ends the container - the same mechanism that
// already closes a one-shot session when its single command finishes.
func (s *Session) CloseLater() error {
	if s == nil {
		return errors.New("no session")
	}
	s.mu.RLock()
	status := s.Status
	running := status == api.SessionRunning
	s.mu.RUnlock()

	// AN ALREADY-FINISHED SESSION IS SATISFIED, not an error: the caller asked for a
	// state ("this ends without me") that is already true. Reporting failure would
	// make a retry after a race look like a broken request.
	if !running {
		return nil
	}

	// IDEMPOTENT. A second --later asks for what the first already established; the
	// CAS is the whole implementation of "already closing is success".
	if !s.closeLater.CompareAndSwap(false, true) {
		return nil
	}
	s.beginClosing()

	// THE CHECK COMES AFTER THE CAS, so a session that is refused does not stay
	// marked. Undo it on the way out - the workload is not something the caller can
	// change from here, but a refused request must not leave a side effect behind
	// that makes the NEXT ordinary call fail on a "closing" session nobody closed.
	if unbounded := s.unboundedWorkload(); unbounded != "" {
		s.closeLater.Store(false)
		s.closing.Store(false)
		return fmt.Errorf("%w: %s", ErrUnboundedWorkload, unbounded)
	}
	return nil
}

// unboundedWorkload names the first thing in this session that will never end by
// itself, or "" when everything running is bounded.
//
// The sentence is the product: a caller that gets 400 has to know WHICH channel has
// no end and what to do about it, because "some channel cannot be deferred" is not
// actionable in a session holding ten of them.
func (s *Session) unboundedWorkload() string {
	var found string
	s.shells.Range(func(_, v any) bool {
		sh, ok := v.(*ChildShell)
		if !ok || sh.mode != api.ModePTY {
			return true
		}
		// A pty that has already exited is not holding anything open; only a live
		// one is the shell someone would have to type `exit` in.
		info := sh.Info()
		if info.Status != api.SessionRunning {
			return true
		}
		found = fmt.Sprintf("channel %s is an interactive pty (mode=pty), and a pty ends when someone types exit - not by itself", info.Name)
		if info.Name == "" {
			found = fmt.Sprintf("channel termcp://#%s:%d is an interactive pty (mode=pty), and a pty ends when someone types exit - not by itself", s.ID, sh.Index)
		}
		return false
	})
	if found != "" {
		return found
	}

	// Forwards are asked for at the manager level, not stored on the session; a
	// session that carries one is found by the caller (see mcp.directiveClose) and
	// reported there. This method answers only for what the session itself knows.
	return ""
}

// Closing reports whether a deferred close has been requested.
func (s *Session) Closing() bool {
	return s != nil && s.closeLater.Load()
}

// CloseLaterError reports whether err is the bounded-workload refusal.
func CloseLaterError(err error) bool {
	return errors.Is(err, ErrUnboundedWorkload)
}
