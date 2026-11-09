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
// SET-THEN-CHECK INSTEAD OF A MUTEX. closeLater is set first, so any channel that
// passes its own check during this decision is caught by createChildShell's re-check
// and torn down. An unbounded workload rolls the bit back, leaving the session as
// it was.

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

	// SET THE FLAG FIRST, THEN JUDGE, THEN ROLL BACK ON REFUSAL.
	//
	// The flag is what refuses new channels, so setting it before the judgement closes
	// the window the old lock closed: a channel that passes its own check afterwards is
	// re-checked when its SSH channel exists (createChildShell) and tears itself down.
	// Rolling back on refusal keeps the promise exact — a caller that gets 400 finds
	// the session exactly as it was, still accepting channels.
	if !s.closeLater.CompareAndSwap(false, true) {
		return nil // idempotent: what the first call established is still in effect
	}
	if unbounded := s.unboundedWorkload(); unbounded != "" {
		s.closeLater.Store(false)
		return fmt.Errorf("%w: %s", ErrUnboundedWorkload, unbounded)
	}

	// The request is in force: new channels are refused whether or not the teardown
	// flag below has been set yet.
	s.beginClosing()

	// FULFIL IT NOW IF THERE IS NOTHING LEFT TO WAIT FOR. The deferred close is
	// normally fulfilled by the last channel's exit watcher (closeIfLastShellEnded),
	// but a session holding no live channel has no watcher that could ever run, so
	// --later on it would hang forever. Same rule, checked at the moment it is asked
	// rather than only at the moment work ends.
	if s.noWorkOutstanding() {
		s.closeIfLastShellEnded()
	}
	return nil
}

// noWorkOutstanding reports whether no live channel remains, which is the whole of
// what a deferred close waits for.
func (s *Session) noWorkOutstanding() bool {
	// NO LOCK: the answer comes from a sync.Map walk, and a channel registered after
	// the walk is caught by the same re-check that refuses a channel after close.
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
// The answer is a snapshot: a channel that registers afterwards is refused by the
// re-check in createChildShell, so nothing can start after the request is in force.
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
