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

// STATE TRANSITIONS HAVE ONE OWNER EACH. A deferred close is two separate pieces of
// state, and each is written from exactly one place:
//
//   - closeLater is the REQUEST. It is set only here, after the workload has been
//     checked, and it is cleared only when closeIfLastShellEnded fulfils it. It is
//     what refuses a new channel (see CreateChildShell) and what the MCP surface
//     reads to say "this session is closing".
//   - closing is the TEARDOWN that is already underway. beginClosing owns it; this
//     function never writes it, because the only honest way to leave a teardown
//     would be to unwind side effects (deliberateClose marks on every shell, an
//     approval queue already disabled) that have no matching reopen. A refused
//     request must mutate nothing, so a refused request never reaches beginClosing.
//
// Keeping the two apart is what makes the refusal side-effect-free: an earlier draft
// set closing first and stored it back on the failure path, which silently undid the
// CAS while leaving every other consequence of beginClosing in place.
//
// THE CHECK AND THE MARK ARE ONE CRITICAL SECTION. CreateChildShell takes the same
// shellStateMu and refuses on closeLater, so a pty cannot slip in between "this
// session has no pty" and "this session refuses new pty-less work". Everything inside
// the lock is memory-only; beginClosing runs after it, and a caller that arrives then
// finds closeLater already set.

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
// WHAT IT DOES NOT DO. It does not terminate anything. The channels that are already
// running keep running and their output keeps arriving; every NEW channel is refused
// from this moment. When the last one ends, closeIfLastShellEnded is what actually
// ends the container.
func (s *Session) CloseLater() error {
	if s == nil {
		return errors.New("no session")
	}
	s.mu.RLock()
	running := s.Status == api.SessionRunning
	s.mu.RUnlock()

	// AN ALREADY-FINISHED SESSION IS SATISFIED, not an error: the caller asked for a
	// state ("this ends without me") that is already true. Reporting failure would
	// make a retry after a race look like a broken request.
	if !running {
		return nil
	}

	s.shellStateMu.Lock()
	if s.closeLater.Load() {
		s.shellStateMu.Unlock()
		return nil // idempotent: what the first call established is still in effect
	}
	if unbounded := s.unboundedWorkload(); unbounded != "" {
		s.shellStateMu.Unlock()
		return fmt.Errorf("%w: %s", ErrUnboundedWorkload, unbounded)
	}
	s.closeLater.Store(true)
	s.shellStateMu.Unlock()

	// The request is in force: new channels are refused whether or not the teardown
	// flag below has been set yet.
	s.beginClosing()

	// FULFIL IT NOW IF THERE IS NOTHING LEFT TO WAIT FOR. The deferred close is
	// normally fulfilled by the last shell's exit watcher (closeIfLastShellEnded),
	// but a session with no channels has no watcher that could ever run: a borrowed
	// container records finished commands into its history and holds no live shell at
	// all, so --later on it would hang forever. Same rule, checked at the moment it
	// is asked rather than only at the moment work ends.
	if s.noWorkOutstanding() {
		s.closeIfLastShellEnded()
	}
	return nil
}

// borrowedWork counts commands running on this session's behalf that own no channel
// — the short-circuit runs a `use:<host>` container records (#255). They are the
// session's outstanding work for a deferred close, which otherwise has no shell exit
// to wait for.
//
// ATOMIC RATHER THAN A MUTEX: a pure counter with no companion invariant, read as a
// snapshot by the close decision. The session may terminate while an increment is in
// flight; that increment lands on a DEAD container, which is what refusing a late
// command in the MCP layer is for.
func (s *Session) beginBorrowedWork() { s.borrowedWork.Add(1) }

func (s *Session) endBorrowedWork() {
	if s.borrowedWork.Add(-1) <= 0 && s.closeLater.Load() {
		// The last outstanding command ended on a session that was asked to wind
		// down: the promise is due now.
		s.closeIfLastShellEnded()
	}
}

// noWorkOutstanding reports whether nothing is left that a deferred close would wait
// for: no live channel and no borrowed command in flight.
func (s *Session) noWorkOutstanding() bool {
	if s.borrowedWork.Load() > 0 {
		return false
	}
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()
	live := false
	s.shells.Range(func(_, v any) bool {
		if v.(*ChildShell).statusValue() == api.SessionRunning {
			live = true
			return false
		}
		return true
	})
	return !live
}

// unboundedWorkload names the first thing in this session that will never end by
// itself, or "" when everything running is bounded.
//
// The sentence is the product: a caller that gets 400 has to know WHICH channel has
// no end and what to do about it, because "some channel cannot be deferred" is not
// actionable in a session holding ten of them.
//
// CALLERS HOLD shellStateMu, which is what makes the answer stay true while the
// caller marks the request.
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
		if info.Name != "" {
			found = fmt.Sprintf("channel %q is an interactive pty (mode=pty), and a pty ends when someone types exit — not by itself", info.Name)
		} else {
			found = fmt.Sprintf("channel termcp://#%s:%d is an interactive pty (mode=pty), and a pty ends when someone types exit — not by itself", s.ID, sh.Index)
		}
		return false
	})
	return found
}

// Closing reports whether a close has been requested or is underway. Both pieces of
// state mean the same thing to a caller asking "may I start work here": no.
func (s *Session) Closing() bool {
	return s != nil && (s.closeLater.Load() || s.closing.Load())
}

// DeferredCloseRequested reports the --later request specifically, without the
// teardown flag, for surfaces that distinguish "asked to wind down" from "already
// being closed".
func (s *Session) DeferredCloseRequested() bool {
	return s != nil && s.closeLater.Load()
}

// CloseLaterError reports whether err is the bounded-workload refusal.
func CloseLaterError(err error) bool {
	return errors.Is(err, ErrUnboundedWorkload)
}

// BeginBorrowedWork / EndBorrowedWork are the exported half of the counter: the MCP
// layer brackets a short-circuit run with them so a deferred close on the container
// waits for the command that owns no channel on it.
func (s *Session) BeginBorrowedWork() { s.beginBorrowedWork() }
func (s *Session) EndBorrowedWork()   { s.endBorrowedWork() }
