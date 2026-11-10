package session

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Approval mode gates every input to a session behind N-person approval.
//
// The scope is the session, not the shell: turning it on for a session covers
// every shell channel in it, including channels opened afterwards. The switch
// is the session's own state rather than a server-wide flag because production
// access is per-target — one deployment can hold a scratch host and a prod host
// at the same time.
//
// Only two things can carry input into a shell while approval mode is on:
//
//   - a human acting in the Web UI, whose submissions are queued the same way an
//     agent's are, and
//   - nothing else. The WebSocket character stream is refused outright (see
//     webui.handleWSInput): a per-keystroke stream cannot be reviewed as a unit,
//     so it is dropped rather than squeezed through the queue.
type approvalState struct {
	queue  *approval.Queue
	need   int
	window time.Duration
	// scopes is the set of operation kinds this session gates. Nil means every
	// gated kind (see approval.AllKinds), which is what review mode has always
	// done and what an empty scope list in an old manifest resolves to.
	scopes map[approval.Kind]bool
	// pending holds text that has been submitted but not yet ended, keyed by
	// shell. An agent types a line and then presses enter, and those are two MCP
	// calls; holding the text until the newline arrives is what lets a reviewer
	// see one complete command instead of half a line followed by a bare enter.
	//
	// A sync.Map OF STRINGS, NOT A LOCKED MAP OF MUTABLE CELLS: the value is the
	// whole state of one shell, appending is a CompareAndSwap on that key, and no
	// rule spans two shells. Every other field of approvalState is written once when
	// the state is built.
	pending sync.Map // shellID string -> staged text string
	// onChange is atomic so a transition never holds a lock while calling out.
	onChange atomic.Pointer[func(sessionID string, req approval.Request)]
}

// SubmitStaged appends text for a shell and reports whether a reviewer needs to
// look at anything yet.
//
// It accumulates rather than queueing, because the unit a human reviews is a
// command line: `echo hi` and the enter that ends it are separate calls, and
// queueing each separately would ask for two decisions on one command (and allow
// approving the text while rejecting the newline, leaving a half-typed line).
func (s *Session) SubmitStaged(shellID, source, text string) error {
	st := s.approval.Load()
	if st == nil {
		return ErrApprovalOff
	}
	if text == "" {
		return nil
	}
	// APPEND BY CAS: two calls can stage on one shell at once, and the second must
	// see the first's text rather than replace it.
	for {
		cur, ok := st.pending.Load(shellID)
		if !ok {
			if _, loaded := st.pending.LoadOrStore(shellID, text); !loaded {
				return nil
			}
			continue
		}
		if st.pending.CompareAndSwap(shellID, cur, cur.(string)+text) {
			return nil
		}
	}
}

// CommitStaged turns whatever a shell has staged into a queued request, adding
// the key that ended it. With nothing staged, the key alone is queued: a bare
// ctrl+c or enter is a command line of its own and must still be reviewable.
func (s *Session) CommitStaged(shellID, source, key string, repeat int) (string, error) {
	st := s.approval.Load()
	if st == nil {
		return "", ErrApprovalOff
	}
	text := ""
	if cur, ok := st.pending.LoadAndDelete(shellID); ok {
		text, _ = cur.(string)
	}

	keys := make([]string, 0, repeat)
	for i := 0; i < repeat; i++ {
		keys = append(keys, key)
	}
	// The text and the ending key form one request, so a reviewer approves the
	// whole command line and cannot split it.
	return s.submitApproval(shellID, source, text, keys)
}

// discardStaged drops every shell's uncommitted text.
func (st *approvalState) discardStaged() {
	st.pending.Clear()
}

// approvalConfig is the resolved configuration for enabling approval mode.
type approvalConfig struct {
	need    int
	timeout time.Duration
}

// EnableApproval turns approval mode on for this session. Every shell in the
// session is covered, and the queue outlives individual shells.
//
// Enabling twice replaces the queue, which cancels anything still pending: a
// request approved under the previous threshold must not be replayed under a
// different one.
func (s *Session) EnableApproval(need int, timeout time.Duration) error {
	return s.EnableApprovalScoped(need, timeout, nil)
}

// EnableApprovalScoped is EnableApproval with an explicit set of gated kinds.
//
// A nil or empty set means every gated kind, which is the original behaviour and
// the default: review started as "hold everything the agent does to this host",
// and a caller that names no scopes is asking for exactly that. Naming a subset
// is the finer control issue #75 asks for — a session where the agent may read
// and write files but not open a tunnel, or one where only commands are held.
//
// Unknown kinds are refused rather than ignored: a typo in a config would
// otherwise silently drop a scope the operator believed was on, which is the one
// failure mode a gate must not have. The refusal names the valid set.
func (s *Session) EnableApprovalScoped(need int, timeout time.Duration, scopes []approval.Kind) error {
	if need < 1 {
		return fmt.Errorf("approval threshold must be at least 1, got %d", need)
	}
	if timeout < 0 {
		return fmt.Errorf("approval timeout cannot be negative, got %s", timeout)
	}
	set, err := approval.NormalizeScopes(scopes)
	if err != nil {
		return err
	}

	st := &approvalState{need: need, window: timeout, scopes: set}
	if sink := s.approvalSink.Load(); sink != nil {
		st.onChange.Store(sink)
	}
	// The queue's change hook re-reads the session's current state rather than
	// capturing the event sink here: the sink is installed by main and can be
	// replaced between enable and use.
	st.queue = approval.NewQueue(s.ID, approval.Options{
		Need:    need,
		Timeout: timeout,
		OnChange: func(r approval.Request) {
			// The audit mark goes first: a replay of log.jsonl should show the
			// decision point even if a UI subscriber is slow or absent.
			s.recordApprovalMark(r)
			if p := st.onChange.Load(); p != nil && *p != nil {
				(*p)(s.ID, r)
			}
		},
	})

	// s.closing is an atomic.Bool (AGENTS.md rule 5): no mutex, no lock hierarchy,
	// no nested locking. If the session has begun shutting down, reject immediately.
	if s.closing.Load() {
		st.discardStaged()
		st.queue.Close("session is closing")
		return ErrSessionClosing
	}

	prev := s.approval.Load()
	if s.closing.Load() {
		st.discardStaged()
		st.queue.Close("session is closing")
		return ErrSessionClosing
	}
	s.approval.Store(st)
	// A live queue supersedes the retained one: the cancelled history belongs to a
	// mode that is no longer in force.
	s.retainedQueue.Store(nil)

	if prev != nil {
		prev.discardStaged()
		prev.queue.Close("approval mode reconfigured")
	}

	// Catch shutdown that arrived while the previous queue was tearing down.
	if s.closing.Load() {
		s.DisableApproval()
		return ErrSessionClosing
	}
	return nil
}

// DisableApproval turns approval mode off and cancels anything pending.
//
// Cancelling is deliberate: an input that was submitted under approval mode but
// not yet approved must not become executable merely because the mode changed.
// It also releases callers blocked in Wait instead of leaving them until timeout.
func (s *Session) DisableApproval() {
	st := s.approval.Load()
	s.approval.Store(nil)
	if st != nil {
		// KEEP THE QUEUE READABLE (issue #217). Clearing it made every pending
		// request vanish from /api/approvals the moment the session closed: the
		// safety half was right (approving answered 404/409 and nothing executed),
		// but the promised terminal `cancelled` state could never be observed, so a
		// harness could not tell why a request ended.
		s.retainedQueue.Store(st.queue)
	}

	if st != nil {
		st.discardStaged()
		st.queue.Close("approval mode disabled")
	}
}

// ApprovalEnabled reports whether this session gates anything at all.
func (s *Session) ApprovalEnabled() bool {
	return s.approval.Load() != nil
}

// GatesKind reports whether this session holds operations of this kind.
//
// It is the question every gate has to ask, and it is separate from
// ApprovalEnabled because a session can be in review mode for one kind and not
// another: with scopes set to file writes, a port forward must go straight
// through. A caller that only checked ApprovalEnabled would hold the wrong
// things — which is the whole point of #75.
func (s *Session) GatesKind(kind approval.Kind) bool {
	st := s.approval.Load()
	if st == nil {
		return false
	}
	return st.gates(kind)
}

// ApprovalScopes lists the kinds this session holds, or nil when it gates
// everything. Reported so the UI can show what the switch currently covers
// instead of a bare on/off.
func (s *Session) ApprovalScopes() []approval.Kind {
	st := s.approval.Load()
	if st == nil {
		return nil
	}
	return st.scopeList()
}

// gates answers the per-kind question for one state, reading scopes under its own
// lock (EnableApproval swaps the whole state in, so the set never changes under a
// live pointer, but the lock keeps the read explicit).
func (st *approvalState) gates(kind approval.Kind) bool {
	if st.scopes == nil {
		return true
	}
	return st.scopes[kind]
}

// scopeList returns the gated kinds in a stable order, or nil for "everything".
func (st *approvalState) scopeList() []approval.Kind {
	if st.scopes == nil {
		return nil
	}
	return approval.ScopeList(st.scopes)
}

// ApprovalQueue returns the active queue, or nil when approval mode is off.
// Callers use it to submit, list, and decide requests.
func (s *Session) ApprovalQueue() *approval.Queue {
		if st := s.approval.Load(); st != nil {
		return st.queue
	}
	// A disabled or closed session retains its closed queue so readers can observe
	// the terminal cancelled state (issue #217). It gates nothing — ApprovalEnabled
	// and GatesKind both read s.approval — and approving from it answers
	// ErrNotPending, so a late click cannot execute anything.
	return s.retainedQueue.Load()
}

// SetApprovalChangeHandler installs the sink notified on every queue
// transition. It is called without any session lock held, so the handler may
// call back into the session (the UI broadcaster does exactly that).
//
// It works whether or not approval mode is already on: main installs the sink
// at startup for every session it creates, and mode is switched on later.
func (s *Session) SetApprovalChangeHandler(fn func(sessionID string, req approval.Request)) {
	s.approvalSink.Store(&fn)
	if st := s.approval.Load(); st != nil {
		st.onChange.Store(&fn)
	}
}

// submitApproval queues a command line for this session. It returns
// ErrApprovalOff when approval mode is off, which the caller reads as "write
// directly".
func (s *Session) submitApproval(shellID, source, text string, keys []string) (string, error) {
	st := s.approval.Load()
	if st == nil {
		return "", ErrApprovalOff
	}
	return st.queue.SubmitShellInput(shellID, source, text, keys)
}

// SubmitOperation queues a non-terminal operation (a file transfer, a port
// forward) for this session.
//
// It shares the queue with command lines on purpose: review is one gate with one
// reviewer and one worklist, and a second queue would mean a second place to
// look. What differs is the payload, which the caller describes with a summary
// (what a human reads) and a payload (what the executor replays).
func (s *Session) SubmitOperation(sub approval.Submission) (string, error) {
	st := s.approval.Load()
	if st == nil {
		return "", ErrApprovalOff
	}
	// The queue stamps the session id itself: it belongs to exactly one session,
	// so taking it from the caller would only create a way to get it wrong.
	return st.queue.Submit(sub)
}

// ErrApprovalOff reports that a session does not gate input, so the caller
// should write the bytes straight through.
var ErrApprovalOff = fmt.Errorf("approval mode is off")

// ErrSessionClosing reports that approval mode cannot be reconfigured while the
// session is shutting down (PR #226): installing a fresh queue on a closed session
// would hand back a gate whose requests can never execute.
var ErrSessionClosing = fmt.Errorf("session is closing")

// ChildShell-side view of the session's approval state.
//
// The query lives here because a handler holding only a *ChildShell must be
// able to ask "may I write?" without reaching for the session. The answer is
// always the parent session's: approval mode is a property of the connection,
// not of one channel on it.

// RequiresApproval reports whether input to this shell must be approved first.
// A detached shell (no parent) never gates: it is not attached to any session
// whose policy could apply.
func (cs *ChildShell) RequiresApproval() bool {
	p := cs.parentSession()
	// A command line is KindShellInput, so a session scoped to file operations
	// only must let typing through. Reading it as "is review mode on" would hold
	// the operator's own commands under a policy that never mentioned them.
	return p != nil && p.GatesKind(approval.KindShellInput)
}

// parentSession reads the parent pointer.
func (cs *ChildShell) parentSession() *Session {
	return cs.parent.Load()
}

// SubmitForApproval queues a command line addressed to this shell and returns
// the pending request id, or ErrApprovalOff when the session does not gate.
func (cs *ChildShell) SubmitForApproval(source, text string, keys []string) (string, error) {
	p := cs.parentSession()
	if p == nil {
		return "", ErrApprovalOff
	}
	return p.submitApproval(cs.ID, source, text, keys)
}

// StageForApproval holds text for this shell without queueing it. The text joins
// the queue when an ending key is committed.
func (cs *ChildShell) StageForApproval(source, text string) error {
	p := cs.parentSession()
	if p == nil {
		return ErrApprovalOff
	}
	return p.SubmitStaged(cs.ID, source, text)
}

// CommitStagedForApproval queues whatever this shell staged, plus the ending key.
func (cs *ChildShell) CommitStagedForApproval(source, key string, repeat int) (string, error) {
	p := cs.parentSession()
	if p == nil {
		return "", ErrApprovalOff
	}
	return p.CommitStaged(cs.ID, source, key, repeat)
}

// ExecuteApprovedInput writes bytes that already passed approval, using the
// same write path as normal input. It exists so the approval executor has one
// call to make and does not need to reconstruct the byte stream itself.
//
// It does not re-check approval mode: the caller has the resolved request in
// hand, and a mode change between approval and execution must not silently drop
// a decision a human already made.
func (cs *ChildShell) ExecuteApprovedInput(text string, pressEnter bool, src InputSource) error {
	return cs.SendTerminalBytesFrom([]byte(text), pressEnter, src)
}

// ExecuteApprovedKey presses a named key that already passed approval.
func (cs *ChildShell) ExecuteApprovedKey(key string, repeat int, src InputSource) error {
	return cs.PressKeyFrom(key, repeat, src)
}

// ApprovalQueue returns this shell's session queue, or nil when the session
// does not gate input. Handlers hold a *ChildShell, so the lookup is offered
// here rather than making every caller walk to the session itself.
func (cs *ChildShell) ApprovalQueue() *approval.Queue {
	p := cs.parentSession()
	if p == nil {
		return nil
	}
	return p.ApprovalQueue()
}

// recordApprovalMark writes an approval transition into the shell's byte log.
//
// The marks reuse log.jsonl's status field, which is documented as an open set:
// no schema change is needed, and a reader that does not know the new values
// still replays the log correctly (a mark only says where a span starts).
//
// Only the two states a reader must distinguish are recorded: that a decision
// was requested, and that input was released. A rejection or expiry leaves the
// log at "requested", which is exactly right — no bytes ever followed.
func (s *Session) recordApprovalMark(r approval.Request) {
	if s.msgMgr == nil || r.ShellID == "" {
		return
	}
	var status api.LogStatus
	switch r.State {
	case approval.Pending:
		status = api.LogApprovalRequest
	case approval.Approved:
		status = api.LogApprovalGranted
	default:
		// Rejected, expired, cancelled: the request never produced bytes, so the
		// "requested" mark already tells the whole story.
		return
	}
	_ = s.msgMgr.AppendMarkOnly(s.ID, r.ShellID, status)
}
