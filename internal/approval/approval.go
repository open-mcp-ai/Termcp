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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// State is the lifecycle state of one submitted command line.
type State string

const (
	// Pending is awaiting approvals. It is the only state that can transition
	// further; everything else is terminal.
	Pending State = "pending"
	// Approved was accepted by the reviewer.
	Approved State = "approved"
	// Rejected was refused by any single approver: one veto is enough, because
	// approval is a gate on dangerous input, not a vote on a proposal.
	Rejected State = "rejected"
	// Expired ran out of time before reaching the threshold. It is distinct from
	// Rejected so an audit can tell "someone said no" from "nobody looked".
	Expired State = "expired"
	// Cancelled was dropped by the server rather than by a decision: the session
	// died, the shell closed, or approval mode was turned off while it pending.
	Cancelled State = "cancelled"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool { return s != Pending }

// ErrTimeout is returned by Wait when a request expires.
var ErrTimeout = errors.New("approval timed out")

// LocalReviewer is what a decision records as its author.
//
// It is a constant, not a name typed in: under a single deployment token the
// server cannot verify who clicked, and a self-declared name would read as an
// identity in the audit trail while proving nothing. A deployment that needs
// real attribution issues a token per reviewer and records the authenticated
// subject here instead.
const LocalReviewer = "local"

// ErrNotFound is returned when an id is unknown to this queue.
var ErrNotFound = errors.New("approval request not found")

// ErrNotPending is returned when a decision arrives for a request that already
// reached a terminal state.
var ErrNotPending = errors.New("approval request is no longer pending")

// ErrAlreadyApproved is returned when the same approver approves twice. The
// threshold counts distinct people, so a repeat is not a second vote.
var ErrAlreadyApproved = errors.New("approver has already approved this request")

// Kind discriminates what a queued request will DO when approved.
//
// Review started as a gate on terminal input, where every request was a command
// line (text plus the keys that end it). File transfers and port forwards are
// the same kind of decision — an irreversible change to a host a human may not
// be watching — so they ride the same queue rather than growing a second one.
// The payload differs, hence this field: the reviewer sees a summary and the
// executor dispatches on the kind.
//
// A string, not an enum, so a new operation needs no schema change and an older
// reader renders an unknown kind instead of failing to parse the record.
type Kind string

const (
	// KindShellInput is a command line: Text plus the Keys that end it. The
	// original and still most common case.
	KindShellInput Kind = "shell_input"
	// KindFileWrite writes bytes to a remote path.
	KindFileWrite Kind = "file_write"
	// KindFileDelete removes a remote file or directory.
	KindFileDelete Kind = "file_delete"
	// KindFileRename moves a remote path.
	KindFileRename Kind = "file_rename"
	// KindFileMkdir creates a remote directory.
	KindFileMkdir Kind = "file_mkdir"
	// KindFilePerm changes ownership, mode, or timestamps.
	KindFilePerm Kind = "file_perm"
	// KindFileLink creates a symlink or hard link.
	KindFileLink Kind = "file_link"
	// KindFileTruncate changes a remote file's size.
	KindFileTruncate Kind = "file_truncate"
	// KindForwardOpen opens a port forward (local, remote, or dynamic).
	KindForwardOpen Kind = "forward_open"
	// KindForwardClose closes one.
	KindForwardClose Kind = "forward_close"
)

// Request is one submitted operation awaiting a decision, plus its outcome.
//
// The payload (Kind, Text, Keys, Payload) is immutable after Submit: only the
// bookkeeping fields move. Snapshots deep-copy the slices, so a caller cannot
// mutate a queued item through a returned value.
type Request struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	ShellID   string `json:"shell_id"`
	// Source records which entrance submitted this: "mcp" or "rest". It is a
	// string rather than an enum so a future entrance needs no schema change.
	Source string `json:"source"`

	// Kind says what approving this will do. Empty means KindShellInput: the
	// records written before file and forward operations were gated carry no
	// kind, and they are all command lines.
	Kind Kind `json:"kind,omitempty"`
	// Summary is the one-line description a reviewer reads. For a command line
	// it is derived from Text/Keys; for a file or forward operation it is the
	// operation spelled out ("write 1.2 KB to /etc/hosts"), because the payload is
	// not something a human can read back.
	Summary string `json:"summary,omitempty"`

	// Text is the literal text submitted; Keys are named keys the caller
	// attached (from the approval form, or a shell_key call). Together they are
	// the "one command line" this request stands for.
	Text string   `json:"text"`
	Keys []string `json:"keys,omitempty"`

	// Payload is the kind-specific operation to replay on approval, opaque to
	// this package: it stores and hands back what the executor that registered
	// the kind understands. It is what makes the queue general without teaching
	// it about SFTP or port forwarding.
	Payload []byte `json:"payload,omitempty"`

	CreatedAt  int64 `json:"created_at"` // Unix ms
	ExpiresAt  int64 `json:"expires_at,omitempty"`
	ResolvedAt int64 `json:"resolved_at,omitempty"`

	State State `json:"state"`
	// Approvals lists the approvers who accepted, in order. Its length must
	// reach Need; NeedsMore is the live remainder.
	Approvals []string `json:"approvals,omitempty"`
	Need      int      `json:"need"`
	// DecidedBy is the approver who rejected, when State is Rejected.
	DecidedBy string `json:"decided_by,omitempty"`
	// Reason carries the rejection reason or the cancellation cause.
	Reason string `json:"reason,omitempty"`

	// done is closed exactly once when the request reaches a terminal state.
	// It is the signal Wait blocks on, so an approval is observed immediately
	// instead of on the next poll tick.
	done chan struct{}
}

// NeedsMore is the number of additional distinct approvers required.
func (r Request) NeedsMore() int {
	if n := r.Need - len(r.Approvals); n > 0 {
		return n
	}
	return 0
}

// clone returns a copy safe to hand to callers: the slices are copied so a
// reader cannot append into the queue's backing array, and the signal channel
// is not exposed.
func (r *Request) clone() Request {
	c := *r
	c.Keys = append([]string(nil), r.Keys...)
	c.Approvals = append([]string(nil), r.Approvals...)
	// Payload is copied too: a shallow copy of a slice shares the backing array,
	// so a caller mutating what it got back would edit the queued operation.
	c.Payload = append([]byte(nil), r.Payload...)
	c.done = nil
	return c
}

// Queue holds the pending requests for one session. A session's shells share
// one queue, which matches the scope of the approval switch: turning approval
// mode on for a session covers every shell in it.
type Queue struct {
	mu       sync.Mutex
	session  string
	need     int
	timeout  time.Duration
	byID     map[string]*Request
	byShell  map[string][]string // shellID -> request ids, oldest first
	onChange func(Request)       // invoked without the lock held
	stopped  bool
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
	return &Queue{
		session:  sessionID,
		need:     need,
		timeout:  opts.Timeout,
		byID:     map[string]*Request{},
		byShell:  map[string][]string{},
		onChange: opts.OnChange,
	}
}

// SessionID reports which session this queue gates.
func (q *Queue) SessionID() string { return q.session }

// Need reports the configured approver threshold.
func (q *Queue) Need() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.need
}

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
		ID:        uuid.NewString(),
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

	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return "", errors.New("approval queue is closed")
	}
	if q.timeout > 0 {
		r.ExpiresAt = now + q.timeout.Milliseconds()
	}
	r.Need = q.need
	q.byID[r.ID] = r
	if sub.ShellID != "" {
		q.byShell[sub.ShellID] = append(q.byShell[sub.ShellID], r.ID)
	}
	// Need is clamped to >= 1 in NewQueue, so NeedsMore() is never 0 here.
	// That matters: a request that already met its threshold could never gain
	// another state change, and Wait would block forever.
	snap := r.clone()
	q.mu.Unlock()

	q.notify(snap)
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
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.byID[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	return r.clone(), nil
}

// List returns every request, pending first, so the API can show a worklist
// with the actionable entries at the top.
func (q *Queue) List() []Request {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Request, 0, len(q.byID))
	for _, r := range q.byID {
		if r.State == Pending {
			out = append(out, r.clone())
		}
	}
	for _, r := range q.byID {
		if r.State != Pending {
			out = append(out, r.clone())
		}
	}
	return out
}

// ListForShell returns one shell's requests, oldest first.
func (q *Queue) ListForShell(shellID string) []Request {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := q.byShell[shellID]
	out := make([]Request, 0, len(ids))
	for _, id := range ids {
		if r, ok := q.byID[id]; ok {
			out = append(out, r.clone())
		}
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
	q.mu.Lock()
	r, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return Request{}, ErrNotFound
	}
	if r.State != Pending {
		snap := r.clone()
		q.mu.Unlock()
		return snap, ErrNotPending
	}
	r.Approvals = append(r.Approvals, LocalReviewer)
	if r.NeedsMore() == 0 {
		q.resolveLocked(r, Approved, "", "")
	}
	snap := r.clone()
	q.mu.Unlock()

	q.notify(snap)
	return snap, nil
}

// Reject refuses the request. One rejection is final: the request gates a
// command line, and saying no is enough to stop it.
func (q *Queue) Reject(id, reason string) (Request, error) {
	q.mu.Lock()
	r, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return Request{}, ErrNotFound
	}
	if r.State != Pending {
		snap := r.clone()
		q.mu.Unlock()
		return snap, ErrNotPending
	}
	q.resolveLocked(r, Rejected, "", reason)
	snap := r.clone()
	q.mu.Unlock()

	q.notify(snap)
	return snap, nil
}

// Cancel drops a request without a human decision (session died, shell closed,
// approval mode switched off). It is a no-op on an already-terminal request.
func (q *Queue) Cancel(id, reason string) {
	if id == "" {
		return
	}
	q.mu.Lock()
	r, ok := q.byID[id]
	if !ok || r.State != Pending {
		q.mu.Unlock()
		return
	}
	q.resolveLocked(r, Cancelled, "", reason)
	snap := r.clone()
	q.mu.Unlock()

	q.notify(snap)
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
	q.mu.Lock()
	q.stopped = true
	q.mu.Unlock()
	for _, r := range q.cancelPending(reason) {
		q.notify(r)
	}
}

// cancelPending cancels every pending request and returns their snapshots.
func (q *Queue) cancelPending(reason string) []Request {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Request
	for _, r := range q.byID {
		if r.State == Pending {
			q.resolveLocked(r, Cancelled, "", reason)
			out = append(out, r.clone())
		}
	}
	return out
}

// Wait blocks until the request reaches a terminal state or its deadline
// passes, then returns the final snapshot.
func (q *Queue) Wait(id string) (Request, error) {
	q.mu.Lock()
	r, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return Request{}, ErrNotFound
	}
	if r.State.Terminal() {
		snap := r.clone()
		q.mu.Unlock()
		return snap, nil
	}
	done := r.done
	var wait time.Duration
	if r.ExpiresAt > 0 {
		wait = time.Duration(r.ExpiresAt-clock.Now()) * time.Millisecond
		if wait < 0 {
			wait = 0
		}
	}
	q.mu.Unlock()

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

// expire transitions a still-pending request whose deadline passed. It reports
// the truth rather than overwriting a decision that won the race.
func (q *Queue) expire(id string) (Request, error) {
	q.mu.Lock()
	r, ok := q.byID[id]
	if !ok {
		q.mu.Unlock()
		return Request{}, ErrNotFound
	}
	if r.State != Pending || r.ExpiresAt == 0 || clock.Now() < r.ExpiresAt {
		snap := r.clone()
		q.mu.Unlock()
		return snap, nil
	}
	q.resolveLocked(r, Expired, "", "")
	snap := r.clone()
	q.mu.Unlock()

	q.notify(snap)
	return snap, ErrTimeout
}

// resolveLocked performs the one-way transition to a terminal state and signals
// waiters. The queue lock must be held.
func (q *Queue) resolveLocked(r *Request, state State, by, reason string) {
	r.State = state
	r.DecidedBy = by
	r.Reason = reason
	r.ResolvedAt = clock.Now()
	if r.done != nil {
		close(r.done)
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

// String renders the request as a one-line audit entry.
func (r Request) String() string {
	return fmt.Sprintf("%s %s/%s %s text=%q keys=%v approvals=%v/%d",
		r.State, r.SessionID, r.ShellID, r.Source, r.Text, r.Keys, r.Approvals, r.Need)
}

// ShellInputSummary renders a command line for a reviewer.
//
// It lives here rather than in the web UI because the summary travels with the
// queued request: an MCP caller, a REST script, and a browser tab must all see
// the same description of what they are deciding.
func ShellInputSummary(text string, keys []string) string {
	switch {
	case text != "" && len(keys) > 0:
		return text + " + " + strings.Join(keys, ",")
	case text != "":
		return text
	case len(keys) > 0:
		return "keys: " + strings.Join(keys, ",")
	default:
		return "(empty)"
	}
}
