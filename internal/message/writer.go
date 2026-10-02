package message

import (
	"sync"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// sessionWriter is the single writer for one session's shells.
//
// Every mutation of a session's transcript goes through its request channel: the
// byte append together with the mark that describes it, and the size read that a
// mark-only append needs. One goroutine performs each request start to finish, so
// there is no critical section to get right and no lock whose lifetime has to be
// reasoned about - the two halves of an append cannot be separated, because
// nothing else writes.
//
// This is what a mutex in a map could not give. That mutex had to be deleted when
// a session was forgotten, and a writer holding the old mutex could then run
// concurrently with one holding the new. Ownership removes the possibility rather
// than narrowing the window: the owner is created and destroyed with the session,
// and ForgetSession joins it before the id can be reused.
type sessionWriter struct {
	mgr *Manager
	// sessionID is fixed for the writer's lifetime, so requests carry only what
	// varies per call.
	sessionID string

	reqs chan request
	// stopped is closed by stop(); run drains what is already queued and exits.
	stopped chan struct{}
	// exited is closed when run has returned, so stop can join it.
	exited chan struct{}

	// status is the last mark written per shell, so a mark is only appended when
	// the status actually changes. Without it the index would grow one line per
	// read chunk. It is owned by run: nothing else reads or writes it, and stop
	// joins run before anything can observe it - which is why it needs no lock.
	status map[string]api.LogStatus

	stopOnce sync.Once
}

// request is one unit of work. data selects an append; status selects a mark.
type request struct {
	// shell whose log this request concerns.
	shell string
	// data, when non-nil, is appended.
	data []byte
	// status, when true, records mark at the current end of the log.
	status bool
	mark   api.LogStatus

	// reply carries the outcome back. It is buffered so run can answer even if
	// the caller has already given up on the request.
	reply chan result
}

// result is the outcome of one request. A mark-only request reports the offset it
// recorded, which is the log size at that moment.
type result struct {
	offset int64
	err    error
}

// writer returns the session's writer, creating it on first use.
//
// A session whose in-memory state was released gets a fresh writer for the same
// id, which is the documented contract: forgetting is not deleting, so the files
// stay usable and a later append continues the same log.
//
// LoadOrStore is what makes concurrent first use safe without a lock: exactly one
// caller stores a writer and starts its goroutine, and the rest reuse it. The
// losers return the winner's writer and drop theirs - it never ran, so there is no
// goroutine to stop.
//
// The entry is stored before its goroutine starts, and removed by that goroutine as
// it exits, so the entry spans exactly the writer's lifetime. That is what the
// one-writer invariant rests on: there is no moment where a live writer has no
// entry for a newcomer to fill, and no moment where a gone writer still blocks one.
func (m *Manager) writer(sessionID string) *sessionWriter {
	// Read-mostly path: after the first append this is a plain map read, which is
	// what every append after the first costs.
	if v, ok := m.sessions.Load(sessionID); ok {
		return v.(*sessionWriter)
	}

	w := newSessionWriter(m, sessionID)
	actual, loaded := m.sessions.LoadOrStore(sessionID, w)
	if loaded {
		return actual.(*sessionWriter)
	}
	go w.run()
	return w
}
func newSessionWriter(m *Manager, sessionID string) *sessionWriter {
	return &sessionWriter{
		mgr:       m,
		sessionID: sessionID,
		reqs:      make(chan request),
		stopped:   make(chan struct{}),
		exited:    make(chan struct{}),
		status:    make(map[string]api.LogStatus),
	}
}

// append writes data at the end of the log and records a mark when the status
// changed.
func (w *sessionWriter) append(shellID string, data []byte) (int64, error) {
	res := w.do(request{shell: shellID, data: data})
	return res.offset, res.err
}

// markOnly records a status change that produced no bytes. The mark points at the
// current end of the log, read on the same goroutine that writes the mark, so an
// output append cannot land in between and be covered by a zero-byte span.
func (w *sessionWriter) markOnly(shellID string, st api.LogStatus) error {
	return w.do(request{shell: shellID, mark: st, status: true}).err
}

// do sends one request and waits for run to perform it.
//
// The select is what makes a retired writer harmless. If stop has been called,
// sending is no longer the only option available, so the request is refused with
// ErrSessionForgotten rather than left waiting for a receiver that has gone. A
// request already handed to run is still performed: run answers what is queued
// before it exits, so an accepted request is never dropped unanswered.
func (w *sessionWriter) do(req request) result {
	req.reply = make(chan result, 1)
	select {
	case w.reqs <- req:
		// Accepted by run, or queued for its exit loop to drain. Either way a
		// reply is coming.
		return <-req.reply
	case <-w.stopped:
		// Retired before the request was accepted, so it was never performed: the
		// caller is told the session is gone rather than getting a partial result.
		return result{err: ErrSessionForgotten}
	}
}

// stop asks the writer to finish and waits for it to release its state.
// Idempotent.
func (w *sessionWriter) stop() {
	w.stopOnce.Do(func() { close(w.stopped) })
	<-w.exited
}

// run performs requests until stopped. It is the only goroutine that touches this
// session's status map, and the only one appending to its logs.
//
// The deferred removal is what lets the entry stand for the writer's lifetime: the
// writer is the only thing that can take its own entry out of the map, and it does
// so once it can no longer append. Removing it here rather than in ForgetSession
// also means the invariant does not depend on a caller ordering a delete against a
// stop - there is no delete for a caller to order.
func (w *sessionWriter) run() {
	defer func() {
		w.mgr.sessions.CompareAndDelete(w.sessionID, w)
		close(w.exited)
	}()
	for {
		select {
		case req := <-w.reqs:
			req.reply <- w.handle(req)
		case <-w.stopped:
			// Answer whatever is already queued - a caller may be waiting on it -
			// then leave without accepting anything new. After this returns, a
			// send on reqs has no receiver, so do's select takes the stopped case
			// and the request is refused rather than left hanging.
			for {
				select {
				case req := <-w.reqs:
					req.reply <- w.handle(req)
				default:
					return
				}
			}
		}
	}
}

// handle performs one request. Runs on run's goroutine.
func (w *sessionWriter) handle(req request) result {
	if req.data != nil {
		off, err := w.mgr.store.AppendLog(w.sessionID, req.shell, req.data)
		if err != nil {
			return result{err: err}
		}
		// A mark failure is reported but does not undo the bytes: a missing mark
		// makes the span read as a continuation of the previous status, which is a
		// labelling inaccuracy rather than a lost byte or a moved offset.
		return result{offset: off, err: w.recordMark(req.shell, api.LogOutput, off)}
	}
	if req.status {
		size, err := w.mgr.store.LogSize(w.sessionID, req.shell)
		if err != nil {
			return result{err: err}
		}
		return result{offset: size, err: w.recordMark(req.shell, req.mark, size)}
	}
	return result{}
}

// recordMark appends a mark when the shell's status differs from the last one
// recorded, so log.jsonl holds one line per transition rather than one per write.
// Runs on run's goroutine, which is why it touches status without a lock.
func (w *sessionWriter) recordMark(shellID string, st api.LogStatus, offset int64) error {
	key := w.sessionID + "\x00" + shellID
	if prev, seen := w.status[key]; seen && prev == st {
		return nil
	}
	w.status[key] = st
	return w.mgr.store.AppendMark(w.sessionID, shellID, api.LogMark{
		Status: st,
		Time:   clock.Now(),
		Offset: offset,
	})
}
