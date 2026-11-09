package extensions

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// extState is the runtime half of one extension: a FIXED IDENTITY and a mutable
// record published as a whole.
//
// The record is an immutable snapshot behind an atomic pointer, not a set of fields
// behind a mutex. A probe goroutine writes it while the walk reads it, and both want
// one consistent picture: `status`, `sessionID` and the instance list describe one
// moment, and a reader that took the snapshot keeps exactly that moment even if the
// record is replaced underneath. Writers publish by CAS, so they never wait for each
// other either — which is what the mutex was really buying, minus the ordering.
type extState struct {
	// IDENTITY, WRITTEN ONCE when the state is filed; never changed afterwards.
	extName string
	target  string

	data atomic.Pointer[extStateData]
}

type extStateData struct {
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
	//
	// VALUES, NOT POINTERS: the snapshot is shared with readers, so an element that
	// changed must be replaced, never mutated in place.
	extra []extraChannel

	// ctx is the run this state belongs to. The deadline callback checks it, so a
	// timer that was already scheduled when Stop ran does nothing instead of closing
	// a channel on a reconciler nobody is watching — which is why Stop does not have
	// to walk every state and disarm them.
	ctx context.Context

	// extName is what this state is filed under. The timers need it: they fire
	// outside any call, and have to read the template again to decide.
	extName  string
	stateKey string

	// target is the host this state runs on. One extension may declare several, and
	// each gets its own state: a machine that is down must not make its siblings
	// unavailable, and two targets must not share a retry ladder or a forward set.
	// Empty means the single target the template itself names.
	target string

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

// load returns the current record, creating the first one for a state built as a
// literal (tests do). The returned snapshot is never modified.
func (st *extState) load() *extStateData {
	if p := st.data.Load(); p != nil {
		return p
	}
	fresh := &extStateData{}
	if st.data.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return st.data.Load()
}

// update publishes a new record built from the current one. Slices are copied on
// append by the caller: a snapshot that was handed out must never change.
func (st *extState) update(fn func(d *extStateData)) {
	for {
		cur := st.data.Load()
		var next extStateData
		if cur != nil {
			next = *cur
		}
		fn(&next)
		if st.data.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// setLazy records whether the reconciler starts this extension on demand, and
// the window it was given. Called by every walk, so a template that gains or
// loses its forwards changes the answer without any other bookkeeping.
func (st *extState) setRun(ctx context.Context) {
	st.update(func(d *extStateData) { d.ctx = ctx })
}

func (st *extState) setLazy(lazy bool, limit time.Duration) {
	st.update(func(d *extStateData) { d.lazy, d.idleLimit = lazy, limit })
}

// setIdleLimit records the window without touching anything else, for the calls
// that arrive before the first walk has run (an Acquire on a cold daemon).
func (st *extState) setIdleLimit(limit time.Duration) {
	st.update(func(d *extStateData) { d.idleLimit = limit })
}

// want stamps the clock and reports whether this is a fresh ask, which is what
// decides between "already up" and "starting" on the resident path.
func (st *extState) want() (fresh bool) {
	now := time.Now()
	for {
		cur := st.load()
		var next extStateData
		next = *cur
		fresh = next.lastUse.IsZero()
		next.lastUse = now
		if st.data.CompareAndSwap(cur, &next) {
			return fresh
		}
	}
}

// touch renews the clock: something addressed this extension just now. It does
// NOT set wanted — an extension nobody asked for is not kept up by a stray read
// of a retained transcript.
func (st *extState) touch() {
	st.update(func(d *extStateData) { d.lastUse = time.Now() })
}

// idleFor reports how long it has been since the extension was last asked for or
// addressed, and false when it has never been (nothing to reap).
func (st *extState) idleFor() (time.Duration, bool) {
	d := st.load()
	if d.lastUse.IsZero() {
		return 0, false
	}
	return time.Since(d.lastUse), true
}

// recordClosed remembers the instance that was taken away, so the handle the
// caller is still holding has an answer.
func (st *extState) recordClosed(sessionID, shellID, reason string) {
	now := time.Now()
	st.update(func(d *extStateData) {
		d.closedAt = now
		d.closedSessionID, d.closedShellID, d.closedReason = sessionID, shellID, reason
	})
}

// renewSession stamps every instance served by this session, for the tool calls
// that name a session rather than one channel. It reports whether the session is
// this extension's.
func (st *extState) renewSession(sessionID string) bool {
	now := time.Now()
	matched := false
	st.update(func(d *extStateData) {
		if d.sessionID != sessionID && !anyInstanceOn(d, sessionID) {
			return
		}
		matched = true
		d.lastUse = now
		// A SNAPSHOT'S ELEMENTS ARE VALUES: the lastUse of a matching instance is
		// written by replacing it, never by mutating shared memory.
		for i := range d.extra {
			if d.extra[i].sessionID == sessionID {
				d.extra[i].lastUse = now
			}
		}
	})
	return matched
}

// changed reports whether the template differs from the one this extension was
// last brought up from, and remembers the new one. A change rebuilds what the
// extension holds — its channel and its tunnels — without touching the connection
// it shares, which is deliberately coarse: deciding which fields are cosmetic is
// how a live channel drifts from its template.
func (st *extState) changed(ext *Extension) bool {
	fp := fingerprint(ext)
	// ONE CAS, NOT CHECK-THEN-SET: two callers that saw the same old fingerprint
	// would both report a change and rebuild twice.
	for {
		cur := st.load()
		if cur.fingerprint == fp {
			return false
		}
		next := *cur
		next.fingerprint = fp
		if st.data.CompareAndSwap(cur, &next) {
			return true
		}
	}
}

// fingerprint summarizes the template as a comparable string. The target and the
// whitelists are in it as well as in the group key, so a template that moved to
// another connection is also seen as changed — the two are independent readings of
// the same edit.
func fingerprint(ext *Extension) string {
	var b strings.Builder
	// WHAT RUNS IS WHAT IS IN THE FINGERPRINT. Every field a session is built from
	// belongs here; a field that only describes the template does not, because
	// changing a description must not tear down a live instance.
	fmt.Fprintf(&b, "%s|%s|%q|", ext.Target, ext.Mode, ext.Command)
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
	for {
		cur := st.load()
		if cur.groupKey == key {
			return false
		}
		next := *cur
		moved := next.groupKey != ""
		next.groupKey = key
		if st.data.CompareAndSwap(cur, &next) {
			return moved
		}
	}
}

// leaveGroup forgets the connection this extension was served by and reports
// whether it had one.
func (st *extState) leaveGroup() bool {
	for {
		cur := st.load()
		if cur.groupKey == "" {
			return false
		}
		next := *cur
		next.groupKey = ""
		if st.data.CompareAndSwap(cur, &next) {
			return true
		}
	}
}

func (st *extState) groupOf() string { return st.load().groupKey }

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
	now := time.Now()
	st.update(func(d *extStateData) {
		if d.sessionID == "" {
			// The first channel also names the connection, and the name OUTLIVES the
			// channel: the connection is kept for the next caller, so "which session is
			// this extension served by" must still answer after the last channel goes.
			d.sessionID = sessionID
		}
		next := make([]extraChannel, 0, len(d.extra)+1)
		next = append(next, d.extra...)
		next = append(next, extraChannel{sessionID: sessionID, shellID: shellID, label: label, lastUse: now, fp: fp})
		d.extra = next
	})
}

// dropInstancesOnSession forgets every instance served by a connection that is
// gone. It reports whether there were any.
func (st *extState) dropInstancesOnSession(sessionID string) []string {
	var dropped []string
	st.update(func(d *extStateData) {
		kept := make([]extraChannel, 0, len(d.extra))
		for _, ex := range d.extra {
			if ex.sessionID == sessionID {
				dropped = append(dropped, ex.shellID)
				continue
			}
			kept = append(kept, ex)
		}
		d.extra = kept
	})
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
	now := time.Now()
	matched := false
	st.update(func(d *extStateData) {
		for i := range d.extra {
			if d.extra[i].shellID == shellID {
				d.extra[i].lastUse = now
				d.lastUse = now
				matched = true
				return
			}
		}
	})
	return matched
}

// dropExtra forgets one instance and reports whether it had it.
func (st *extState) dropExtra(shellID string) bool {
	found := false
	st.update(func(d *extStateData) {
		for i, ex := range d.extra {
			if ex.shellID == shellID {
				next := make([]extraChannel, 0, len(d.extra)-1)
				next = append(next, d.extra[:i]...)
				next = append(next, d.extra[i+1:]...)
				d.extra = next
				found = true
				return
			}
		}
	})
	return found
}

// dueExtras removes and returns the instances whose silence has run out, and
// records the close for each.
//
// THE REMOVAL, THE REASON AND THE STATUS COME OUT OF ONE LOCK SCOPE. They used to
// be three calls from the caller: the instance was gone, then the reason was
// written, then the status settled — and a reader (Status/List → snapshot) landing
// in between saw an extension with no instances, no closed channel and a status
// that still said "available". A window running out is exactly the case a reader
// asks about, so the answer must never be that half-state. Taking them out of the
// state is what keeps the walk from handing one out while it is being closed.
func (st *extState) dueExtras(limit time.Duration, reason string) []string {
	// A ZERO WINDOW COLLECTS NOTHING (issue #227). Without this guard the comparison
	// below reads `now.Sub(lastUse) >= 0`, which is true for every instance — so
	// allowing idle_timeout = 0 at the config layer alone would have reaped every
	// channel the moment it was created.
	if limit <= 0 {
		return nil
	}
	now := time.Now()
	var due []string
	st.update(func(d *extStateData) {
		kept := make([]extraChannel, 0, len(d.extra))
		for _, ex := range d.extra {
			// A channel-less instance is the connection, and the connection is kept for
			// the next caller — so there is nothing for a window to run out on.
			if ex.shellID != "" && now.Sub(ex.lastUse) >= limit {
				due = append(due, ex.shellID)
				sessionID := d.sessionID
				if sessionID == "" {
					sessionID = ex.sessionID
				}
				d.closedAt = now
				d.closedSessionID, d.closedShellID, d.closedReason = sessionID, ex.shellID, reason
				continue
			}
			kept = append(kept, ex)
		}
		d.extra = kept
		if len(due) > 0 {
			settle(d)
		}
	})
	return due
}

// extraShells lists the instances beyond the primary, for the walk's liveness
// check.
// extraShellsNotBuiltFrom lists the channels whose template is not keepFP: the
// ones an edit invalidated, as opposed to the ones a caller asked for afterwards.
func (st *extState) extraShellsNotBuiltFrom(keepFP string) []string {
	var out []string
	for _, ex := range st.load().extra {
		if ex.fp != keepFP {
			out = append(out, ex.shellID)
		}
	}
	return out
}

func (st *extState) extraShells() []string {
	d := st.load()
	out := make([]string, 0, len(d.extra))
	for _, ex := range d.extra {
		out = append(out, ex.shellID)
	}
	return out
}

func (st *extState) instanceCount() int { return instanceCount(st.load()) }

// instanceCount counts a snapshot's live handles: its own channel plus instances.
func instanceCount(d *extStateData) int {
	n := len(d.extra)
	if d.shellID != "" {
		n++
	}
	return n
}

// retireExtra drops one dead instance, records why it ended and settles the
// status, in ONE lock scope.
//
// The three steps used to be separate calls from the walk, so a reader landing
// between them saw an extension that had just lost its channel and could say
// neither which one nor why — status still "available", closed id empty. The reason
// belongs to the channel, so it is written together with the removal that makes it
// news.
func (st *extState) retireExtra(sessionID, shellID, reason string) bool {
	found := false
	st.update(func(d *extStateData) {
		idx := -1
		for i, ex := range d.extra {
			if ex.shellID == shellID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		found = true
		next := make([]extraChannel, 0, len(d.extra)-1)
		next = append(next, d.extra[:idx]...)
		next = append(next, d.extra[idx+1:]...)
		d.extra = next
		recordFailure(d, "session", reason)
		d.closedAt = time.Now()
		d.closedSessionID, d.closedShellID, d.closedReason = sessionID, shellID, reason
		d.status = StatusUnavailable
		settle(d)
	})
	return found
}

// settleLocked applies the runtime-status rule to the instances that are left and
// reports whether the status changed.
//
// ONE DECISION, SHARED: "no instances left" is a fact two callers create — the
// walk settling after a sweep, and the reap step that just removed the instance.
// Deciding it only on the walk's path left the moment described in retireExtra and
// dueExtras.
func (st *extState) settle() bool {
	changed := false
	st.update(func(d *extStateData) { changed = settle(d) })
	return changed
}

// settle applies the runtime-status rule to the instances that are left and reports
// whether the status changed.
//
// ONE DECISION, SHARED: "no instances left" is a fact two callers create — the
// walk settling after a sweep, and the reap step that just removed the instance.
// Deciding it only on the walk's path left the moment described in retireExtra and
// dueExtras.
func settle(d *extStateData) bool {
	switch {
	case len(d.failures) > 0 && instanceCount(d) == 0:
		return false // leave the failure standing
	case instanceCount(d) > 0:
		if d.status == StatusAvailable {
			return false
		}
		d.status = StatusAvailable
		return true
	default:
		if d.status == StatusIdle {
			return false
		}
		d.status = StatusIdle
		return true
	}
}

// sessionOf and shellOf describe the extension's FIRST instance, which is what
// the group, the session-death hook and the status surface need: they answer
// "which connection carries this extension" rather than "which channel is this
// caller using".
func (st *extState) sessionOf() string {
	d := st.load()
	if d.sessionID != "" {
		return d.sessionID
	}
	// An on-demand extension has no attachment of its own: its connection is the one
	// its first instance runs on.
	if len(d.extra) > 0 {
		return d.extra[0].sessionID
	}
	return ""
}

func (st *extState) shellOf() string { return st.load().shellID }

// attach records the session and the channel this extension is now driven
// through.
func (st *extState) attach(sessionID, shellID string) {
	st.update(func(d *extStateData) { d.sessionID, d.shellID = sessionID, shellID })
}

// takeAttachment removes and returns what this extension holds, so a detach can
// close it without holding the lock while it does.
func (st *extState) takeAttachment() (sessionID, shellID string, forwards []ForwardStatus) {
	st.update(func(d *extStateData) {
		sessionID, shellID, forwards = d.sessionID, d.shellID, d.forwards
		d.sessionID, d.shellID, d.forwards = "", "", nil
	})
	return sessionID, shellID, forwards
}

// detachFrom forgets the attachment when it is the one named, and reports whether
// it was.
func (st *extState) detachFrom(sessionID string) bool {
	matched := false
	st.update(func(d *extStateData) {
		if d.sessionID != sessionID {
			return
		}
		matched = true
		d.sessionID, d.shellID, d.forwards = "", "", nil
	})
	return matched
}

// mark sets the status.
func (st *extState) mark(status string) {
	st.update(func(d *extStateData) { d.status = status })
}

// statusOf and flags are the two reads the walk makes on one extension: the
// status alone, and the status together with the retry budget that decides
// whether the walk may act on it.
func (st *extState) statusOf() string { return st.load().status }

// isLazy reports whether this extension is started on demand rather than kept
// resident, which decides who owns the retry: the caller, not a timer.
func (st *extState) isLazy() bool { return st.load().lazy }

func (st *extState) flags() (status string, autoRetry bool) {
	d := st.load()
	return d.status, d.autoRetry
}

// scheduleBackoff reports how long to wait before the next attempt, and whether
// there is one at all. Once the budget is spent it schedules nothing, so nothing
// keeps probing forever: the timer is the only thing that would.
func (st *extState) scheduleBackoff() (time.Duration, bool) {
	var step time.Duration
	ok := false
	st.update(func(d *extStateData) {
		if d.consecutive > maxAutoRetries {
			d.autoRetry = false
			d.readyAt = time.Time{}
			return
		}
		ok = true
		step = backoffSteps[0]
		if d.consecutive > 1 {
			i := d.consecutive - 1
			if i >= len(backoffSteps) {
				i = len(backoffSteps) - 1
			}
			step = backoffSteps[i]
		}
		d.readyAt = time.Now().Add(step)
	})
	return step, ok
}

// armRetry schedules the next attempt. Bound to the run's context for the same
// reason the idle deadline is: a callback that fires after Stop must do nothing.
func (r *Reconciler) armRetry(st *extState, wait time.Duration) {
	var ctx context.Context
	st.update(func(d *extStateData) {
		if d.retry != nil {
			d.retry.Stop()
			d.retry = nil
		}
		ctx = d.ctx
	})
	if wait <= 0 {
		return
	}
	t := time.AfterFunc(wait, func() {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		name, target := st.name(), st.host()
		if name == "" {
			return
		}
		ext, err := r.loadTarget(name, target)
		if err != nil || !ext.IsEnabled() {
			return
		}
		r.retryTargetNow(name, target)
	})
	st.update(func(d *extStateData) { d.retry = t })
}

// exhaustRetryBudget spends the automatic budget without scheduling a retry.
// It is the answer to a failure no retry can fix, where recovery is something
// else entirely — for a template that cannot resolve, the walk's rebuildable
// check. Climbing the ladder there would only stretch the wait, and leaving the
// budget whole would re-probe every walk for a condition the probe cannot
// change.
func (st *extState) exhaustRetryBudget() {
	st.update(func(d *extStateData) {
		d.autoRetry = false
		d.readyAt = time.Time{}
	})
}

// resetRetryBudget hands the extension its automatic retries back. Called on the
// paths that mean "this is a fresh start": a probe that succeeded, a manual
// retry, a template edit.
func (st *extState) resetRetryBudget() {
	st.update(func(d *extStateData) {
		d.consecutive = 0
		d.autoRetry = true
		d.readyAt = time.Time{}
	})
}

// lastStage is the stage of the most recent failure, empty when there is no
// history. It tells the spent-budget path what it is waiting on.
func (st *extState) lastStage() string {
	d := st.load()
	if n := len(d.failures); n > 0 {
		return d.failures[n-1].Stage
	}
	return ""
}

// recordFailure appends a reason, deduplicating an identical consecutive entry
// so a permanently broken extension does not bury its history under one repeated
// line while the count still climbs.
func (st *extState) recordFailure(stage, reason string) {
	st.update(func(d *extStateData) { recordFailure(d, stage, reason) })
}

// recordFailure is the history update on one snapshot; callers already hold it.
func recordFailure(d *extStateData, stage, reason string) {
	if len(d.failures) > 0 {
		last := d.failures[len(d.failures)-1]
		if last.Stage == stage && last.Reason == reason {
			d.consecutive++
			return
		}
	}
	next := make([]Failure, 0, len(d.failures)+1)
	next = append(next, d.failures...)
	next = append(next, Failure{
		At:     clock.Now(),
		Stage:  stage,
		Reason: truncate(reason, 300),
	})
	if len(next) > maxFailures {
		next = append([]Failure(nil), next[len(next)-maxFailures:]...)
	}
	d.failures = next
	d.consecutive++
}

func (st *extState) snapshot() Status {
	d := st.load()
	out := Status{
		Status:    d.status,
		SessionID: d.sessionID,
		ShellID:   d.shellID,
		Lazy:      d.lazy,
		Instances: len(d.extra),
	}
	// THE CHANNEL REPORTED IS ONE THE CALLER CAN DRIVE. For a template with its own
	// channel that is the reconciler's; for an on-demand extension it is the instance
	// handed out MOST RECENTLY (issue #230).
	//
	// It used to be the FIRST one, which made `ls ext` and `open ext` name different
	// channels in the same breath: every ask gets its own channel (see Acquire), so
	// after two opens `ls` reported :1 while `open` returned :2 — two directives, two
	// addresses, no way to tell which was live. The newest is the one an `open` just
	// returned, so the two now agree; Instances carries how many exist, and
	// `get <session>` still lists every one of them.
	if d.shellID != "" {
		out.Instances++
	} else if len(d.extra) > 0 {
		newest := d.extra[len(d.extra)-1]
		out.ShellID = newest.shellID
	}
	if out.SessionID == "" && len(d.extra) > 0 {
		out.SessionID = d.extra[len(d.extra)-1].sessionID
	}
	if d.lazy {
		out.IdleTimeoutMs = d.idleLimit.Milliseconds()
		if !d.lastUse.IsZero() {
			out.IdleForMs = time.Since(d.lastUse).Milliseconds()
		}
	}
	if !d.closedAt.IsZero() {
		out.ClosedAt = d.closedAt.UnixMilli()
		out.ClosedSessionID, out.ClosedShellID, out.ClosedReason = d.closedSessionID, d.closedShellID, d.closedReason
	}
	if len(d.forwards) > 0 {
		out.Forwards = append([]ForwardStatus(nil), d.forwards...)
	}
	if len(d.failures) > 0 {
		out.Failures = append([]Failure(nil), d.failures...)
	}
	if !d.readyAt.IsZero() {
		out.NextRetry = d.readyAt.UnixMilli()
	}
	return out
}
