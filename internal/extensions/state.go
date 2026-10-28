package extensions

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// extState is the mutable runtime half of one extension. Every field behind its
// own mutex because a probe goroutine writes it while the walk reads it.
type extState struct {
	mu        sync.Mutex
	status    string
	groupKey  string // the connection this extension is served by; "" = none
	sessionID string // the group's session, while this extension is attached
	shellID   string // this extension's own channel; "" for a template with no shell
	forwards  []ForwardStatus
	failures  []Failure
	// consecutive counts consecutive failures, for the backoff ladder.
	consecutive int
	readyAt     time.Time // earliest moment a retry may run
	autoRetry   bool      // the automatic budget is unspent; false once it is
	fingerprint string    // last template applied; a change rebuilds what it holds

	// Lazy and the two fields under it are the on-demand half (issue #183): lazy
	// says the reconciler never starts this extension by itself, idleLimit is the
	// window it was given, and lastUse is when anything last asked for or drove an
	// instance of it.
	lazy      bool
	idleLimit time.Duration
	lastUse   time.Time

	// What was taken away, for a caller still holding the handle: "not found" is
	// true and useless where "this was closed at 14:02 because it was idle for 10m"
	// is the answer to the question the caller actually has.
	closedAt        time.Time
	closedSessionID string
	closedShellID   string
	closedReason    string

	// labels counts the instances this extension has EVER opened in this run. It
	// only ever goes up, because a label must not be reused while the channel that
	// held it may still exist: deriving it from the instance count gave the second
	// instance the first one's name the moment the first was dropped from the
	// bookkeeping but not from the session, and the session refused it (#188).
	labels int

	// extra are the channels this extension has handed out. Each is one caller's
	// own process on the group's connection, so a second caller costs a fork, not a
	// dial — and no two callers ever share a cursor (issue #183).
	extra []*extraChannel

	// ctx is the run this state belongs to. The deadline callback checks it, so a
	// timer that was already scheduled when Stop ran does nothing instead of closing
	// a channel on a reconciler nobody is watching — which is why Stop does not have
	// to walk every state and disarm them.
	ctx context.Context

	// extName is what this state is filed under. The timers need it: they fire
	// outside any call, and have to read the template again to decide.
	extName string

	// retry is the pending automatic attempt, or nil. One timer, replaced on every
	// failure — the ladder is a deadline the status prints, so it must be kept.
	retry *time.Timer

	// timer fires when the earliest channel's silence runs out. It is what makes
	// idle_timeout a promise instead of a walk-tick approximation: a caller that
	// asked for ten minutes gets ten, not ten and a bit because the sweep happened
	// to be five seconds away. The walk keeps its own check as a backstop — a timer
	// that fails to fire must not keep a process alive for ever.
	timer *time.Timer
}

// setLazy records whether the reconciler starts this extension on demand, and
// the window it was given. Called by every walk, so a template that gains or
// loses its forwards changes the answer without any other bookkeeping.
func (st *extState) setRun(ctx context.Context) {
	st.mu.Lock()
	st.ctx = ctx
	st.mu.Unlock()
}

func (st *extState) setLazy(lazy bool, limit time.Duration) {
	st.mu.Lock()
	st.lazy, st.idleLimit = lazy, limit
	st.mu.Unlock()
}

// setIdleLimit records the window without touching anything else, for the calls
// that arrive before the first walk has run (an Acquire on a cold daemon).
func (st *extState) setIdleLimit(limit time.Duration) {
	st.mu.Lock()
	st.idleLimit = limit
	st.mu.Unlock()
}

// want stamps the clock and reports whether this is a fresh ask, which is what
// decides between "already up" and "starting" on the resident path.
func (st *extState) want() (fresh bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	fresh = st.lastUse.IsZero()
	st.lastUse = time.Now()
	return fresh
}

// touch renews the clock: something addressed this extension just now. It does
// NOT set wanted — an extension nobody asked for is not kept up by a stray read
// of a retained transcript.
func (st *extState) touch() {
	st.mu.Lock()
	st.lastUse = time.Now()
	st.mu.Unlock()
}

// idleFor reports how long it has been since the extension was last asked for or
// addressed, and false when it has never been (nothing to reap).
func (st *extState) idleFor() (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastUse.IsZero() {
		return 0, false
	}
	return time.Since(st.lastUse), true
}

// recordClosed remembers the instance that was taken away, so the handle the
// caller is still holding has an answer.
func (st *extState) recordClosed(sessionID, shellID, reason string) {
	st.mu.Lock()
	st.closedAt = time.Now()
	st.closedSessionID, st.closedShellID, st.closedReason = sessionID, shellID, reason
	st.mu.Unlock()
}

// renewSession stamps every instance served by this session, for the tool calls
// that name a session rather than one channel. It reports whether the session is
// this extension's.
func (st *extState) renewSession(sessionID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessionID != sessionID && !st.anyInstanceOn(sessionID) {
		return false
	}
	st.lastUse = time.Now()
	for _, ex := range st.extra {
		if ex.sessionID == sessionID {
			ex.lastUse = st.lastUse
		}
	}
	return true
}

// changed reports whether the template differs from the one this extension was
// last brought up from, and remembers the new one. A change rebuilds what the
// extension holds — its channel and its tunnels — without touching the connection
// it shares, which is deliberately coarse: deciding which fields are cosmetic is
// how a live channel drifts from its template.
func (st *extState) changed(ext *Extension) bool {
	fp := fingerprint(ext)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fingerprint == fp {
		return false
	}
	st.fingerprint = fp
	return true
}

// fingerprint summarizes the template as a comparable string. The target and the
// whitelists are in it as well as in the group key, so a template that moved to
// another connection is also seen as changed — the two are independent readings of
// the same edit.
func fingerprint(ext *Extension) string {
	var b strings.Builder
	target := ext.TargetIdentity
	if target == "" {
		target = ext.Target
	}
	fmt.Fprintf(&b, "%s|%s|%s|%q|", ext.Kind, target, ext.Mode, ext.Command)
	for _, a := range ext.Args {
		fmt.Fprintf(&b, "%q,", a)
	}
	// The endpoint and its headers describe what reaches the channel as much as the
	// command does — the URL form's argv is derived from them, so an edit here is
	// an edit to what runs.
	fmt.Fprintf(&b, "|%q|", ext.URL)
	for _, name := range sortedKeys(ext.Headers) {
		fmt.Fprintf(&b, "%q=%q,", name, ext.Headers[name])
	}
	b.WriteByte('|')
	for _, a := range ext.Allow {
		b.WriteString(a)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, p := range ext.Paths {
		b.WriteString(p)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, f := range ext.Forwards {
		fmt.Fprintf(&b, "%s:%s:%s:%d:%s:%d|", f.Kind, f.BindHost, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort)
	}
	fmt.Fprintf(&b, "|%d|%d|%t", ext.Rows, ext.Cols, ext.IsEnabled())
	return b.String()
}

// setGroup records the connection this extension is served by and reports whether
// it moved. The first resolution is not a move: nothing was held yet.
func (st *extState) setGroup(key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.groupKey == key {
		return false
	}
	moved := st.groupKey != ""
	st.groupKey = key
	return moved
}

// leaveGroup forgets the connection this extension was served by and reports
// whether it had one.
func (st *extState) leaveGroup() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	had := st.groupKey != ""
	st.groupKey = ""
	return had
}

func (st *extState) groupOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.groupKey
}

// extraChannel is one instance of an on-demand extension: a caller's channel on
// the group's connection, plus the clock that says whether anybody is still
// driving it.
type extraChannel struct {
	// sessionID is the connection this instance runs on. It is per instance rather
	// than per extension because an instance outlives a detach: an edit closes the
	// channels built from the old template and clears the extension's own
	// attachment, and the instances a caller still holds keep naming the connection
	// they are on.
	sessionID string
	shellID   string
	label     string
	lastUse   time.Time
	// fp is the template this channel was built from. An edit closes the channels
	// built from the OLD one — and only those, so a channel a caller asked for after
	// the edit survives the walk that noticed it. Without this the walk closes the
	// very channel the caller just received, and a caller that asks again after an
	// edit gets nothing but the edit.
	fp string
}

// addExtra records a channel handed to a caller. The session comes with it: an
// on-demand extension's instances all live on the group's connection, so the first
// one is also what names the connection the status reports.
func (st *extState) addExtra(sessionID, shellID, label, fp string) {
	st.mu.Lock()
	if st.sessionID == "" {
		// The first channel also names the connection, and the name OUTLIVES the
		// channel: the connection is kept for the next caller, so "which session is
		// this extension served by" must still answer after the last channel goes.
		st.sessionID = sessionID
	}
	st.extra = append(st.extra, &extraChannel{sessionID: sessionID, shellID: shellID, label: label, lastUse: time.Now(), fp: fp})
	st.mu.Unlock()
}

// dropInstancesOnSession forgets every instance served by a connection that is
// gone. It reports whether there were any.
func (st *extState) dropInstancesOnSession(sessionID string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.extra[:0]
	var dropped []string
	for _, ex := range st.extra {
		if ex.sessionID == sessionID {
			dropped = append(dropped, ex.shellID)
			continue
		}
		kept = append(kept, ex)
	}
	st.extra = kept
	return dropped
}

// renew stamps the instance this shell id names, for the tool calls that keep
// using it. It reports whether the shell is one of this extension's instances.
//
// The extension's own clock moves with it: `idle_for_ms` answers "how long have I
// got" out of that field, and a channel being driven while the reported silence kept
// climbing is the field telling the caller something the daemon does not believe
// (dogfood on #183).
func (st *extState) renew(shellID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, ex := range st.extra {
		if ex.shellID == shellID {
			ex.lastUse = time.Now()
			st.lastUse = ex.lastUse
			return true
		}
	}
	return false
}

// dropExtra forgets one instance and reports whether it had it.
func (st *extState) dropExtra(shellID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, ex := range st.extra {
		if ex.shellID == shellID {
			st.extra = append(st.extra[:i], st.extra[i+1:]...)
			return true
		}
	}
	return false
}

// dueExtras removes and returns the instances whose silence has run out. Taking
// them out of the state is what keeps the walk from handing one out while it is
// being closed.
func (st *extState) dueExtras(limit time.Duration) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	kept := st.extra[:0]
	var due []string
	for _, ex := range st.extra {
		// A channel-less instance is the connection, and the connection is kept for
		// the next caller — so there is nothing for a window to run out on.
		if ex.shellID != "" && now.Sub(ex.lastUse) >= limit {
			due = append(due, ex.shellID)
			continue
		}
		kept = append(kept, ex)
	}
	st.extra = kept
	return due
}

// extraShells lists the instances beyond the primary, for the walk's liveness
// check.
// extraShellsNotBuiltFrom lists the channels whose template is not keepFP: the
// ones an edit invalidated, as opposed to the ones a caller asked for afterwards.
func (st *extState) extraShellsNotBuiltFrom(keepFP string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, ex := range st.extra {
		if ex.fp != keepFP {
			out = append(out, ex.shellID)
		}
	}
	return out
}

func (st *extState) extraShells() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.extra))
	for _, ex := range st.extra {
		out = append(out, ex.shellID)
	}
	return out
}

func (st *extState) instanceCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := len(st.extra)
	if st.shellID != "" {
		n++
	}
	return n
}

// sessionOf and shellOf describe the extension's FIRST instance, which is what
// the group, the session-death hook and the status surface need: they answer
// "which connection carries this extension" rather than "which channel is this
// caller using".
func (st *extState) sessionOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessionID != "" {
		return st.sessionID
	}
	// An on-demand extension has no attachment of its own: its connection is the one
	// its first instance runs on.
	if len(st.extra) > 0 {
		return st.extra[0].sessionID
	}
	return ""
}

func (st *extState) shellOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.shellID
}

// attach records the session and the channel this extension is now driven
// through.
func (st *extState) attach(sessionID, shellID string) {
	st.mu.Lock()
	st.sessionID = sessionID
	st.shellID = shellID
	st.mu.Unlock()
}

// takeAttachment removes and returns what this extension holds, so a detach can
// close it without holding the lock while it does.
func (st *extState) takeAttachment() (sessionID, shellID string, forwards []ForwardStatus) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sessionID, shellID, forwards = st.sessionID, st.shellID, st.forwards
	st.sessionID, st.shellID, st.forwards = "", "", nil
	return sessionID, shellID, forwards
}

// detachFrom forgets the attachment when it is the one named, and reports whether
// it was.
func (st *extState) detachFrom(sessionID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sessionID != sessionID {
		return false
	}
	st.sessionID, st.shellID, st.forwards = "", "", nil
	return true
}

// mark sets the status.
func (st *extState) mark(status string) {
	st.mu.Lock()
	st.status = status
	st.mu.Unlock()
}

// statusOf and flags are the two reads the walk makes on one extension: the
// status alone, and the status together with the retry budget that decides
// whether the walk may act on it.
func (st *extState) statusOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status
}

// isLazy reports whether this extension is started on demand rather than kept
// resident, which decides who owns the retry: the caller, not a timer.
func (st *extState) isLazy() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lazy
}

func (st *extState) flags() (status string, autoRetry bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status, st.autoRetry
}

// scheduleBackoff reports how long to wait before the next attempt, and whether
// there is one at all. Once the budget is spent it schedules nothing, so nothing
// keeps probing forever: the timer is the only thing that would.
func (st *extState) scheduleBackoff() (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.consecutive > maxAutoRetries {
		st.autoRetry = false
		st.readyAt = time.Time{}
		return 0, false
	}
	step := backoffSteps[0]
	if st.consecutive > 1 {
		i := st.consecutive - 1
		if i >= len(backoffSteps) {
			i = len(backoffSteps) - 1
		}
		step = backoffSteps[i]
	}
	st.readyAt = time.Now().Add(step)
	return step, true
}

// armRetry schedules the next attempt. Bound to the run's context for the same
// reason the idle deadline is: a callback that fires after Stop must do nothing.
func (r *Reconciler) armRetry(st *extState, wait time.Duration) {
	st.mu.Lock()
	if st.retry != nil {
		st.retry.Stop()
		st.retry = nil
	}
	ctx := st.ctx
	st.mu.Unlock()
	if wait <= 0 {
		return
	}
	t := time.AfterFunc(wait, func() {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		name := st.name()
		if name == "" {
			return
		}
		ext, err := r.store.Load(name)
		if err != nil || !ext.IsEnabled() {
			return
		}
		r.retryNow(name)
	})
	st.mu.Lock()
	st.retry = t
	st.mu.Unlock()
}

// exhaustRetryBudget spends the automatic budget without scheduling a retry.
// It is the answer to a failure no retry can fix, where recovery is something
// else entirely — for a template that cannot resolve, the walk's rebuildable
// check. Climbing the ladder there would only stretch the wait, and leaving the
// budget whole would re-probe every walk for a condition the probe cannot
// change.
func (st *extState) exhaustRetryBudget() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.autoRetry = false
	st.readyAt = time.Time{}
}

// resetRetryBudget hands the extension its automatic retries back. Called on the
// paths that mean "this is a fresh start": a probe that succeeded, a manual
// retry, a template edit.
func (st *extState) resetRetryBudget() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.consecutive = 0
	st.autoRetry = true
	st.readyAt = time.Time{}
}

// lastStage is the stage of the most recent failure, empty when there is no
// history. It tells the spent-budget path what it is waiting on.
func (st *extState) lastStage() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := len(st.failures); n > 0 {
		return st.failures[n-1].Stage
	}
	return ""
}

// recordFailure appends a reason, deduplicating an identical consecutive entry
// so a permanently broken extension does not bury its history under one repeated
// line while the count still climbs.
func (st *extState) recordFailure(stage, reason string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.failures) > 0 {
		last := st.failures[len(st.failures)-1]
		if last.Stage == stage && last.Reason == reason {
			st.consecutive++
			return
		}
	}
	st.failures = append(st.failures, Failure{
		At:     clock.Now(),
		Stage:  stage,
		Reason: truncate(reason, 300),
	})
	if len(st.failures) > maxFailures {
		st.failures = append([]Failure(nil), st.failures[len(st.failures)-maxFailures:]...)
	}
	st.consecutive++
}

func (st *extState) snapshot() Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := Status{
		Status:    st.status,
		SessionID: st.sessionID,
		ShellID:   st.shellID,
		Lazy:      st.lazy,
		Instances: len(st.extra),
	}
	// THE CHANNEL REPORTED IS ONE THE CALLER CAN DRIVE. For a template with its own
	// channel that is the reconciler's; for an on-demand extension it is the first
	// instance it handed out, because there is no other channel to name.
	if st.shellID != "" {
		out.Instances++
	} else if len(st.extra) > 0 {
		out.ShellID = st.extra[0].shellID
	}
	if out.SessionID == "" && len(st.extra) > 0 {
		out.SessionID = st.extra[0].sessionID
	}
	if st.lazy {
		out.IdleTimeoutMs = st.idleLimit.Milliseconds()
		if !st.lastUse.IsZero() {
			out.IdleForMs = time.Since(st.lastUse).Milliseconds()
		}
	}
	if !st.closedAt.IsZero() {
		out.ClosedAt = st.closedAt.UnixMilli()
		out.ClosedSessionID, out.ClosedShellID, out.ClosedReason = st.closedSessionID, st.closedShellID, st.closedReason
	}
	if len(st.forwards) > 0 {
		out.Forwards = append([]ForwardStatus(nil), st.forwards...)
	}
	if len(st.failures) > 0 {
		out.Failures = append([]Failure(nil), st.failures...)
	}
	if !st.readyAt.IsZero() {
		out.NextRetry = st.readyAt.UnixMilli()
	}
	return out
}
