package mcp

import (
	"context"
	"encoding/json"
)

// CANCELLATION: notifications/cancelled has to reach the call it names.
//
// The protocol lets a client say "stop working on request N". Before this file the
// server read the notification and threw it away (`server.go`'s "nothing to do" case),
// so a cancelled 60-second read ran to completion, held its goroutine, and kept the
// SSH channel busy — the caller had already moved on, and nothing was watching.
//
// THE KEY IS (SESSION, ID), NOT ID ALONE. Request ids are chosen by the client and
// two clients routinely both use `1`. Keying on the id would let one client cancel
// another's call, which is the kind of cross-session leak a shared server must not
// have.
//
// THE TABLE IS sync.Map, and each entry is a context.CancelFunc: cancelling is a
// single map lookup and a function call, no lock held across a handler. The entry is
// removed by the same goroutine that added it, so a cancel that arrives after the
// call finished finds nothing and is IGNORED — which is what the specification asks
// for, and the alternative (an error) would report a race the client cannot fix.

type cancelKey struct {
	session string
	id      string
}

// registerCancel makes a running request cancellable, and returns the function that
// removes the entry. Callers defer it.
func (s *Server) registerCancel(sessionID, id string, cancel context.CancelFunc) func() {
	if sessionID == "" || id == "" {
		return func() {}
	}
	key := cancelKey{session: sessionID, id: id}
	s.cancels.Store(key, cancel)
	return func() { s.cancels.Delete(key) }
}

// cancelRequest cancels the call named by (session, id). It reports whether anything
// was found: false means the call already finished, and the notification is dropped
// as the specification requires.
func (s *Server) cancelRequest(sessionID, id string) bool {
	if sessionID == "" || id == "" {
		return false
	}
	v, ok := s.cancels.Load(cancelKey{session: sessionID, id: id})
	if !ok {
		return false
	}
	if cancel, ok := v.(context.CancelFunc); ok {
		cancel()
	}
	// The entry stays until the handler unwinds: a second cancel must be harmless, and
	// deleting here would let a follower register under the same key and be cancelled
	// by a stale notification.
	return true
}

// cancellation is the params of notifications/cancelled.
type cancellationParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// handleCancelled routes notifications/cancelled to the call it names.
//
// AN UNKNOWN ID IS SILENT. A client cancelling a call that has already answered is
// doing the right thing; answering "no such request" would send it looking for a
// mistake it did not make.
func (s *Server) handleCancelled(ctx context.Context, params json.RawMessage, sess Session) {
	if sess == nil || len(params) == 0 {
		return
	}
	var p cancellationParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	s.cancelRequest(sess.SessionID(), IDString(p.RequestID))
}
