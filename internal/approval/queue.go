// Package approval gates terminal input behind N-person approval.
//
// The model is one-way: every input reaches a shell as a *command line* that
// already passed approval. A caller writes text and may attach named keys
// (ctrl+c, enter, …); the queue holds the whole submission until Need distinct
// approvers accept it, and only then does the gate hand the bytes to the shell.
// Raw character streams are not representable here on purpose — they cannot be
// reviewed byte by byte, so the WebSocket input path is dropped instead of
// being squeezed through this queue.
//
// Nothing in this package fails open: an expired request, a rejected request, a
// queue that lost its session, and a session whose approval mode was turned off
// mid-flight all end with zero bytes written.
package approval

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/uid"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

var (
	ErrNotApproved             = errors.New("approval request is not approved")
	ErrExecutionAlreadyStarted = errors.New("approval execution has already started")
	ErrExecutionNotRunning     = errors.New("approval execution is not running")
	ErrInvalidExecutionState   = errors.New("invalid approval execution state")
)

// Queue holds the pending requests for one session. A session's shells share
// one queue, which matches the scope of the approval switch: turning approval
// mode on for a session covers every shell in it.
type Queue struct {
	session  string
	need     int
	timeout  time.Duration
	onChange func(Request)

	// state is the whole queue — both indexes and the stopped flag — published as
	// an immutable snapshot and advanced by CAS.
	//
	// THE TWO INDEXES ARE ONE VALUE. byID answers "what is this request" and byShell
	// answers "what did this shell ask for"; every transition has to move both, and a
	// reader that saw them disagree would show a shell a request its own list does not
	// have. A snapshot makes that impossible instead of making it a locking rule.
	state atomic.Pointer[queueState]
}

type queueState struct {
	byID    map[string]Request
	byShell map[string][]string // shellID -> request ids, oldest first
	stopped bool
}

// maxRetainedTerminal bounds how much DECIDED history a snapshot carries.
//
// A SNAPSHOT COSTS WHAT IT CONTAINS, and this queue's contents only grow: every
// decided request was kept forever so an auditor could read it back. Measured with
// every decided request retained, one transition at a 10 000-request backlog cost
// 2.9 ms and 3.7 MB of garbage (the whole map copied, every time) and List cost
// 5.4 ms and 6.4 MB. Unbounded history made an O(1) decision walk into an O(n²)
// session.
//
// The bound is the same rule the frame log follows (jrpc maxEvents): keep the
// recent past, which is what a reviewer or a harness reads, and let the rest go.
// PENDING requests are never dropped — they are the work list, and a dropped one
// would be a request nobody can decide.
const maxRetainedTerminal = 256

// evict drops the oldest decided requests beyond maxRetainedTerminal. It runs on
// the private copy inside mutate, so no reader sees the queue mid-eviction, and it
// has no side effect that a retried mutation could repeat.
func (st *queueState) evict() {
	terminal := 0
	for _, r := range st.byID {
		if r.State.Terminal() {
			terminal++
		}
	}
	if terminal <= maxRetainedTerminal {
		return
	}
	type decided struct {
		id, shell string
		at        int64
	}
	old := make([]decided, 0, terminal)
	for id, r := range st.byID {
		if r.State.Terminal() {
			old = append(old, decided{id: id, shell: r.ShellID, at: r.ResolvedAt})
		}
	}
	sort.Slice(old, func(i, j int) bool {
		if old[i].at != old[j].at {
			return old[i].at < old[j].at
		}
		return old[i].id < old[j].id // deterministic across runs
	})
	for _, d := range old[:terminal-maxRetainedTerminal] {
		delete(st.byID, d.id)
		ids := st.byShell[d.shell]
		for i, v := range ids {
			if v == d.id {
				st.byShell[d.shell] = append(append([]string(nil), ids[:i]...), ids[i+1:]...)
				break
			}
		}
		if len(st.byShell[d.shell]) == 0 {
			delete(st.byShell, d.shell)
		}
	}
}

func (st *queueState) clone() *queueState {
	next := &queueState{
		byID:    make(map[string]Request, len(st.byID)),
		byShell: make(map[string][]string, len(st.byShell)),
		stopped: st.stopped,
	}
	for k, v := range st.byID {
		next.byID[k] = v
	}
	for k, v := range st.byShell {
		next.byShell[k] = append([]string(nil), v...)
	}
	return next
}

// load returns the current snapshot, creating the first one for a zero Queue.
func (q *Queue) load() *queueState {
	if p := q.state.Load(); p != nil {
		return p
	}
	fresh := &queueState{byID: map[string]Request{}, byShell: map[string][]string{}}
	if q.state.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return q.state.Load()
}

// mutate publishes a new snapshot built from the current one. fn edits a private
// copy, so a reader holding the old snapshot never sees a half-applied change.
//
// WAKING A WAITER IS NOT PART OF THE EDIT. fn may run several times — each failed
// CAS throws its copy away — and closing a request's done channel twice panics, so
// the channel is only closed by the attempt that actually wins, through the `wake`
// callback it collected during that attempt.
func (q *Queue) mutate(fn func(st *queueState, wake func(chan struct{}))) {
	for {
		cur := q.load()
		next := cur.clone()
		var waking []chan struct{}
		fn(next, func(ch chan struct{}) { waking = append(waking, ch) })
		next.evict()
		if q.state.CompareAndSwap(cur, next) {
			for _, ch := range waking {
				close(ch)
			}
			return
		}
	}
}

// Options configures a Queue.
type Options struct {
	// Need is retained for the JSON shape but is always 1: review has one
	// decision, and the field is kept so a stored manifest from the multi-reviewer
	// version still parses. A caller that wants no gate must not create a queue.
	Need int
	// Timeout expires a pending request. Zero means no timeout, which is
	// intended for tests only — a request that never expires holds a waiter
	// until the process ends.
	Timeout time.Duration
	// OnChange, when set, is called after every state transition (including
	// submission) with a snapshot of the request. It runs without the queue
	// lock, so it may call back into the queue. Used to push approval events to
	// waiting agents and open browser tabs.
	OnChange func(Request)
}

// NewQueue creates the queue for one session.
func NewQueue(sessionID string, opts Options) *Queue {
	// One decision resolves a request. Need is not taken from opts: review mode
	// has a single reviewer, and honouring a stale count would leave requests that
	// no one can ever resolve.
	const need = 1
	q := &Queue{
		session:  sessionID,
		need:     need,
		timeout:  opts.Timeout,
		onChange: opts.OnChange,
	}
	q.state.Store(&queueState{byID: map[string]Request{}, byShell: map[string][]string{}})
	return q
}

// SessionID reports which session this queue gates.
func (q *Queue) SessionID() string { return q.session }

// Need reports the configured approver threshold.
//
// NO LOCK: need is written once by NewQueue and never again, so a reader cannot see
// a torn or half-applied value.
func (q *Queue) Need() int { return q.need }

// Submit enqueues a command line and returns its id. The caller either polls
// Get or blocks in Wait.
//
// A submission with neither text nor keys is rejected: an empty approval would
// let an agent spend an approver's attention on nothing.
// Submission is one operation waiting for a decision. It generalises the
// original command line so file transfers and port forwards use the same queue.
type Submission struct {
	// Kind says what approving this does. Empty is treated as KindShellInput.
	Kind Kind
	// ShellID anchors the request to a shell when one applies (terminal input).
	// File and forward operations are session-scoped, so it may be empty.
	ShellID string
	Source  string
	// Summary is the reviewer-facing one-liner. Required: a request a human
	// cannot read is one they cannot decide.
	Summary string
	// Text and Keys carry a command line (KindShellInput).
	Text string
	Keys []string
	// Payload is the kind-specific operation for the executor to replay.
	Payload []byte
}

// Submit queues one operation and returns its id. The caller either polls Get
// or blocks in Wait.
//
// An empty submission is rejected: an approval with nothing in it would let an
// agent spend a reviewer's attention on nothing.
func (q *Queue) Submit(sub Submission) (string, error) {
	if sub.Kind == "" {
		sub.Kind = KindShellInput
	}
	if sub.Kind == KindShellInput && sub.Text == "" && len(sub.Keys) == 0 {
		return "", errors.New("nothing to approve: both text and keys are empty")
	}
	if sub.Kind != KindShellInput && len(sub.Payload) == 0 {
		return "", fmt.Errorf("nothing to approve: a %s request needs a payload", sub.Kind)
	}
	if strings.TrimSpace(sub.Summary) == "" {
		return "", errors.New("nothing to approve: a request needs a summary a reviewer can read")
	}

	now := clock.Now()
	r := &Request{
		ID:        uid.UUID(),
		SessionID: q.session,
		ShellID:   sub.ShellID,
		Source:    sub.Source,
		Kind:      sub.Kind,
		Summary:   sub.Summary,
		Text:      sub.Text,
		Keys:      append([]string(nil), sub.Keys...),
		Payload:   append([]byte(nil), sub.Payload...),
		CreatedAt: now,
		State:     Pending,
		done:      make(chan struct{}),
	}

	if q.timeout > 0 {
		r.ExpiresAt = now + q.timeout.Milliseconds()
	}
	r.Need = q.need

	closed := false
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		if st.stopped {
			closed = true
			return
		}
		st.byID[r.ID] = *r
		if sub.ShellID != "" {
			ids := append([]string(nil), st.byShell[sub.ShellID]...)
			st.byShell[sub.ShellID] = append(ids, r.ID)
		}
	})
	if closed {
		return "", errors.New("approval queue is closed")
	}
	// Need is clamped to >= 1 in NewQueue, so NeedsMore() is never 0 here.
	// That matters: a request that already met its threshold could never gain
	// another state change, and Wait would block forever.
	q.notify(r.clone())
	return r.ID, nil
}

// SubmitShellInput is the command-line case, kept as its own method because it
// is the common one and its two-argument shape is what the terminal paths have.
func (q *Queue) SubmitShellInput(shellID, source, text string, keys []string) (string, error) {
	return q.Submit(Submission{
		Kind:    KindShellInput,
		ShellID: shellID,
		Source:  source,
		Summary: ShellInputSummary(text, keys),
		Text:    text,
		Keys:    keys,
	})
}

// Get returns a snapshot of one request.
func (q *Queue) Get(id string) (Request, error) {
	var expired Request
	due := false
	var snap Request
	var found bool
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		r, ok := st.byID[id]
		if !ok {
			return
		}
		found = true
		if expireIfDue(&r, wake) {
			st.byID[id] = r
			expired, due = r.clone(), true
		}
		snap = r.clone()
	})
	if !found {
		return Request{}, ErrNotFound
	}
	if due {
		q.notify(expired)
	}
	return snap, nil
}

// List returns every request, pending first, so the API can show a worklist
// with the actionable entries at the top.
func (q *Queue) List() []Request {
	var due []Request
	var out []Request
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		// A worklist that still shows an expired request as actionable is a worklist
		// that lies about what can be approved (issue #216).
		for id, r := range st.byID {
			if expireIfDue(&r, wake) {
				st.byID[id] = r
				due = append(due, r.clone())
			}
		}
		out = make([]Request, 0, len(st.byID))
		for _, r := range st.byID {
			if r.State == Pending {
				out = append(out, r.clone())
			}
		}
		for _, r := range st.byID {
			if r.State != Pending {
				out = append(out, r.clone())
			}
		}
	})
	for _, snap := range due {
		q.notify(snap)
	}
	return out
}

// ListForShell returns one shell's requests, oldest first.
func (q *Queue) ListForShell(shellID string) []Request {
	var due []Request
	var out []Request
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		ids := st.byShell[shellID]
		out = make([]Request, 0, len(ids))
		for _, id := range ids {
			r, ok := st.byID[id]
			if !ok {
				continue
			}
			if expireIfDue(&r, wake) {
				st.byID[id] = r
				due = append(due, r.clone())
			}
			out = append(out, r.clone())
		}
	})
	for _, snap := range due {
		q.notify(snap)
	}
	return out
}

// Approve accepts the request: one reviewer is enough.
//
// The decision is the reviewer's act, recorded by the server, not a name typed
// into a box. A typed name neither proves nor grants anything under a single
// deployment token, so requiring one was friction that produced a claim rather
// than a fact.
func (q *Queue) Approve(id string) (Request, error) {
	var snap Request
	var err error
	notify := false
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		r, ok := st.byID[id]
		if !ok {
			snap, err = Request{}, ErrNotFound
			return
		}
		// An expired request is not pending, whatever the map still says (issue #216).
		if expireIfDue(&r, wake) {
			st.byID[id] = r
			snap, err, notify = r.clone(), ErrTimeout, true
			return
		}
		if r.State == Expired {
			snap, err = r.clone(), ErrTimeout
			return
		}
		if r.State != Pending {
			snap, err = r.clone(), ErrNotPending
			return
		}
		r.Approvals = append(append([]string(nil), r.Approvals...), LocalReviewer)
		if r.NeedsMore() == 0 {
			resolve(&r, Approved, "", "", wake)
			notify = true
		}
		st.byID[id] = r
		snap = r.clone()
	})
	if notify {
		q.notify(snap)
	}
	return snap, err
}

// Reject refuses the request. One rejection is final: the request gates a
// command line, and saying no is enough to stop it.
func (q *Queue) Reject(id, reason string) (Request, error) {
	var snap Request
	var err error
	notify := false
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		r, ok := st.byID[id]
		if !ok {
			snap, err = Request{}, ErrNotFound
			return
		}
		// A rejection after the deadline is still a refusal, but the honest state is
		// Expired: the window closed before a human decided (issue #216).
		if expireIfDue(&r, wake) {
			st.byID[id] = r
			snap, err, notify = r.clone(), ErrTimeout, true
			return
		}
		if r.State == Expired {
			snap, err = r.clone(), ErrTimeout
			return
		}
		if r.State != Pending {
			snap, err = r.clone(), ErrNotPending
			return
		}
		resolve(&r, Rejected, "", reason, wake)
		st.byID[id] = r
		snap, notify = r.clone(), true
	})
	if notify {
		q.notify(snap)
	}
	return snap, err
}

// Cancel drops a request without a human decision (session died, shell closed,
// approval mode switched off). It is a no-op on an already-terminal request.
func (q *Queue) Cancel(id, reason string) {
	if id == "" {
		return
	}
	var snap Request
	cancelled := false
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		r, ok := st.byID[id]
		if !ok || r.State != Pending {
			return
		}
		resolve(&r, Cancelled, "", reason, wake)
		st.byID[id] = r
		snap, cancelled = r.clone(), true
	})
	if cancelled {
		q.notify(snap)
	}
}

// CancelAll drops every pending request, for the same reasons as Cancel. Used
// when a session ends or approval mode is turned off, where a request left
// pending could otherwise be approved later and write into a shell that is gone.
func (q *Queue) CancelAll(reason string) {
	for _, r := range q.cancelPending(reason) {
		q.notify(r)
	}
}

// Close permanently stops the queue: pending requests are cancelled and further
// submissions are refused. The session calls this on release.
func (q *Queue) Close(reason string) {
	q.mutate(func(st *queueState, wake func(chan struct{})) { st.stopped = true })
	for _, r := range q.cancelPending(reason) {
		q.notify(r)
	}
}

// cancelPending cancels every pending request and returns their snapshots.
func (q *Queue) cancelPending(reason string) []Request {
	var out []Request
	now := clock.Now()
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		out = out[:0]
		for id, r := range st.byID {
			if r.State == Pending {
				resolve(&r, Cancelled, "", reason, wake)
				st.byID[id] = r
				out = append(out, r.clone())
				continue
			}
			// Approval and execution are separate. Closing the gate cannot undo an
			// approval that already started, but it must not leave a running
			// execution looking live after its session is gone.
			if r.State == Approved && r.Execution != nil && r.Execution.State == ExecutionRunning {
				exec := *r.Execution
				exec.State = ExecutionUnknown
				exec.FinishedAt = now
				exec.Error = reason
				r.Execution = &exec
				st.byID[id] = r
				out = append(out, r.clone())
			}
		}
	})
	return out
}

// Wait blocks until the request reaches a terminal state or its deadline
// passes, then returns the final snapshot.
func (q *Queue) Wait(id string) (Request, error) {
	st := q.load()
	r, ok := st.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	if r.State.Terminal() {
		return r.clone(), nil
	}
	done := r.done
	var wait time.Duration
	if r.ExpiresAt > 0 {
		wait = time.Duration(r.ExpiresAt-clock.Now()) * time.Millisecond
		if wait < 0 {
			wait = 0
		}
	}

	if r.ExpiresAt == 0 {
		// No deadline: the only way out is a decision.
		<-done
		return q.Get(id)
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return q.Get(id)
	case <-timer.C:
		return q.expire(id)
	}
}

// BeginExecution records that replaying an approved request has started. It
// does not notify listeners: the approval itself was already announced, and an
// intermediate "running" update would only flicker in the reviewer UI.
func (q *Queue) BeginExecution(id string) (Request, error) {
	var snap Request
	var err error
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		r, ok := st.byID[id]
		if !ok {
			snap, err = Request{}, ErrNotFound
			return
		}
		if r.State != Approved {
			snap, err = r.clone(), ErrNotApproved
			return
		}
		if r.Execution != nil {
			snap, err = r.clone(), ErrExecutionAlreadyStarted
			return
		}
		r.Execution = &Execution{State: ExecutionRunning, StartedAt: clock.Now()}
		st.byID[id] = r
		snap = r.clone()
	})
	return snap, err
}

// FinishExecution records the terminal result of replaying an approved
// request. Shell input may end submitted (bytes written) or failed; non-shell
// operations may end succeeded or failed. Unknown is valid for either kind
// when the executor cannot trust the observation.
func (q *Queue) FinishExecution(id string, state ExecutionState, detail string) (Request, error) {
	if state != ExecutionSubmitted && state != ExecutionSucceeded &&
		state != ExecutionFailed && state != ExecutionUnknown {
		return Request{}, ErrInvalidExecutionState
	}

	var snap Request
	var err error
	notify := false
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		r, ok := st.byID[id]
		if !ok {
			snap, err = Request{}, ErrNotFound
			return
		}
		if r.State != Approved || r.Execution == nil || r.Execution.State != ExecutionRunning {
			snap, err = r.clone(), ErrExecutionNotRunning
			return
		}
		isShellInput := r.Kind == "" || r.Kind == KindShellInput
		if isShellInput && state == ExecutionSucceeded {
			snap, err = r.clone(), ErrInvalidExecutionState
			return
		}
		if !isShellInput && state == ExecutionSubmitted {
			snap, err = r.clone(), ErrInvalidExecutionState
			return
		}
		exec := *r.Execution
		exec.State = state
		exec.FinishedAt = clock.Now()
		exec.Error = strings.TrimSpace(detail)
		r.Execution = &exec
		st.byID[id] = r
		snap, notify = r.clone(), true
	})
	if notify {
		q.notify(snap)
	}
	return snap, err
}

// expire transitions a still-pending request whose deadline passed. It reports
// the truth rather than overwriting a decision that won the race.
func (q *Queue) expire(id string) (Request, error) {
	var snap Request
	var err error
	notify := false
	q.mutate(func(st *queueState, wake func(chan struct{})) {
		r, ok := st.byID[id]
		if !ok {
			snap, err = Request{}, ErrNotFound
			return
		}
		if r.State != Pending || r.ExpiresAt == 0 || clock.Now() < r.ExpiresAt {
			snap = r.clone()
			return
		}
		resolve(&r, Expired, "", "", wake)
		st.byID[id] = r
		snap, err, notify = r.clone(), ErrTimeout, true
	})
	if notify {
		q.notify(snap)
	}
	return snap, err
}

// expireIfDue moves a pending request to Expired when its deadline has passed, and
// reports whether it did. The queue lock must be held.
//
// EXPIRY IS A FACT ABOUT THE CLOCK, NOT AN EVENT ONLY Wait CAN DELIVER (issue #216).
// The transition used to be driven exclusively by Wait's timer, but the review flow
// answers 202 immediately and creates no waiter — so a request nobody blocked on
// stayed Pending forever, and a late Approve returned 200 and executed the write.
// That made timeout_seconds a promise the server did not keep, in exactly the case
// it exists for.
//
// Every reader and every decision calls this first, so a request is expired by the
// time anyone can observe or act on it.
func expireIfDue(r *Request, wake func(chan struct{})) bool {
	if r.State != Pending || r.ExpiresAt == 0 || clock.Now() < r.ExpiresAt {
		return false
	}
	resolve(r, Expired, "", "", wake)
	return true
}

// resolveLocked performs the one-way transition to a terminal state and signals
// waiters. The queue lock must be held.
// resolve moves one request to a terminal state and wakes its waiter.
//
// CLOSING done IS SAFE UNDER CAS: only the transition out of Pending reaches here,
// and a CAS loop can publish exactly one such transition — so the channel a waiter
// holds is closed once, by the writer that won.
func resolve(r *Request, state State, by, reason string, wake func(chan struct{})) {
	r.State = state
	r.DecidedBy = by
	r.Reason = reason
	r.ResolvedAt = clock.Now()
	if r.done != nil {
		wake(r.done)
		r.done = nil
	}
}

// notify runs the change hook outside the queue lock, so a hook that calls back
// into the queue cannot deadlock.
func (q *Queue) notify(r Request) {
	if q.onChange == nil {
		return
	}
	q.onChange(r)
}
