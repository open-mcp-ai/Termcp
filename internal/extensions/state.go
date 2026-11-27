package extensions

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/session"
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
	exposes   []ForwardStatus
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

	// restarts counts the times this extension came back up AFTER going down, and
	// downSince marks that it is currently down (issue #325). Counting recoveries
	// needs both: a bring-up while it was already up is a reuse, not a restart, and
	// counting every bring-up would turn "asked for twice" into "restarted once".
	restarts  int
	downSince bool

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

	// sideRuns are the template's channels beyond the default one (issue #331).
	sideRuns []channelRun

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
		fmt.Fprintf(&b, "%s:%s:%s:%d:%s:%d|", f.DirectionKind, f.BindHost, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort)
	}
	// An expose is part of what runs: changing its paths changes what the far side can
	// reach, so a template edit must rebuild the listener rather than leave the old
	// whitelist in place.
	b.WriteByte('|')
	for _, x := range ext.Exposes {
		fmt.Fprintf(&b, "%s:%s:%d:", x.Transport, x.RemoteHost, x.RemotePort)
		for _, p := range x.Paths {
			fmt.Fprintf(&b, "%q,", p)
		}
		b.WriteByte('|')
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
		recordFailure(d, "session", reason, "")
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
// carrierOnlyFailure reports whether every recorded failure belongs to a tunnel carried
// by another host's connection, with none about this extension itself.
//
// IT IS THE LINE BETWEEN "this extension is down" and "the machine it tunnels through
// is". Only the first should change the status word (issue #330).
func carrierOnlyFailure(d *extStateData) bool {
	if len(d.failures) == 0 {
		return false
	}
	for _, f := range d.failures {
		if f.Via == "" {
			return false
		}
	}
	return true
}

func settle(d *extStateData) bool {
	switch {
	case carrierOnlyFailure(d):
		// A TUNNEL ON ANOTHER HOST IS DOWN AND THE EXTENSION IS NOT (issue #330). The
		// status bringUp settled on — available, because the endpoint or the channel
		// answered — is the truth here, and rewriting it to idle or unavailable would
		// take away a capability that works. The failure still names its via host in
		// `failures[]`.
		return false
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
		d.sideRuns = nil
	})
	return sessionID, shellID, forwards
}

// channelRun is one of the template's channels beyond the default one, as it is
// actually opened (issue #331).
//
// WHY THE DEFAULT CHANNEL IS NOT IN HERE. The default channel is the extension's
// own instance — the thing `open ext` hands out copies of, the thing the idle rule
// reaps — and that machinery already lives in sessionID/shellID and in `extra`. The
// other channels are one per session rather than one per caller: opening a second
// copy of a worker for each caller would run N processes where the template declared
// one. So they are tracked here and closed with the attachment.
type channelRun struct {
	name      string
	label     string
	target    string
	sessionID string
	shellID   string
}

// setSideRuns records the extra channels and reports the shells it is replacing,
// which the caller closes.
func (st *extState) setSideRuns(runs []channelRun) []string {
	var stale []string
	st.update(func(d *extStateData) {
		for _, old := range d.sideRuns {
			keep := false
			for _, n := range runs {
				if n.shellID == old.shellID {
					keep = true
					break
				}
			}
			if !keep && old.shellID != "" {
				stale = append(stale, old.shellID)
			}
		}
		// A SNAPSHOT THAT WAS HANDED OUT MUST NEVER CHANGE, so the slice is copied in.
		d.sideRuns = append([]channelRun(nil), runs...)
	})
	return stale
}

// takeSideRuns removes and returns the extra channels, for the paths that close
// everything this extension holds.
func (st *extState) takeSideRuns() []channelRun {
	var out []channelRun
	st.update(func(d *extStateData) {
		out = d.sideRuns
		d.sideRuns = nil
	})
	return out
}

// sideRunsSnapshot copies the extra channels for a status read.
func (st *extState) sideRunsSnapshot() []channelRun {
	return append([]channelRun(nil), st.load().sideRuns...)
}

// sideShell returns the running shell of one extra channel, or "".
func (st *extState) sideShell(name string) string {
	for _, run := range st.load().sideRuns {
		if run.name == name {
			return run.shellID
		}
	}
	return ""
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

// mark sets the status, and counts a recovery when one happens (issue #325).
//
// THE COUNTER MOVES ON THE TRANSITION OUT OF `unavailable`, which is the only moment
// that means "it was down and now it is up". Counting at bring-up time instead would
// also count a caller's second `open` of a healthy extension, and a number that goes up
// when nothing restarted is worse than no number.
func (st *extState) mark(status string) {
	st.update(func(d *extStateData) {
		// `downSince` ALONE DECIDES, not the status field: a tunnel failure records the
		// failure without flipping the published status (the endpoint is still answering),
		// and a recovery after that is still a recovery. Requiring status != available here
		// made the counter miss exactly that case — measured, not assumed.
		if status == StatusAvailable && d.downSince {
			d.restarts++
			d.downSince = false
		}
		d.status = status
	})
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
	st.update(func(d *extStateData) {
		// EVERY FAILURE MEANS THE EXTENSION IS DOWN, which is what a later successful
		// bring-up counts as a restart (issue #325). Set here rather than in fail(): a
		// tunnel failure and a channel failure both go through this path, and a restart
		// after either is a restart.
		d.downSince = true
		recordFailure(d, stage, reason, "")
	})
}

// recordCarrierFailure records a failure that belongs to a tunnel carried by ANOTHER
// host's connection (issue #330).
//
// THE `via` IS THE POINT, not decoration: a failure naming a carrier host is a
// statement about that machine, and the extension's own endpoint or channel may be
// answering perfectly well. The status code reads this field to decide whether the
// extension is down or only its tunnel is (settleLocked).
func (st *extState) recordCarrierFailure(stage, reason, via string) {
	st.update(func(d *extStateData) { recordFailure(d, stage, reason, via) })
}

func recordFailure(d *extStateData, stage, reason, via string) {
	if len(d.failures) > 0 {
		last := d.failures[len(d.failures)-1]
		if last.Stage == stage && last.Reason == reason && last.Via == via {
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
		Via:    via,
	})
	if len(next) > maxFailures {
		next = append([]Failure(nil), next[len(next)-maxFailures:]...)
	}
	d.failures = next
	d.consecutive++
}

// probeLiveness MEASURES whether the extension's process is still there (issue #325).
//
// IT TAKES THE HANDLES FROM THE SNAPSHOT AND LOOKS THEM UP OUTSIDE IT, because the lookup
// reaches into the session manager and a probe must never hold anything this state owns
// while calling into another subsystem (AGENTS.md: a lock inside a cross-component call is
// how two independent locks become one deadlock).
//
// A DEAD CHANNEL IS REPORTED, NOT CORRECTED. The reconciler's reaper owns the transition
// to unavailable; a read path that flipped the status would race the walk and could
// publish "down" for a channel that came back between the two.
func (st *extState) probeLiveness(lookup func(string) *session.ChildShell, sessionStatus func(string) string) *Liveness {
	d := st.load()
	shellID, sessionID := d.shellID, d.sessionID
	closedShellID, closedReason := d.closedShellID, d.closedReason
	if shellID == "" && len(d.extra) > 0 {
		shellID = d.extra[len(d.extra)-1].shellID
		sessionID = d.extra[len(d.extra)-1].sessionID
	}

	out := &Liveness{CheckedAt: time.Now().UnixMilli()}
	if shellID == "" {
		if sessionID != "" {
			out.SessionStatus = sessionStatus(sessionID)
		}
		// "THE CHANNEL DIED" AND "THERE NEVER WAS ONE" ARE DIFFERENT ANSWERS, and a
		// probe that said the second about the first would be the same class of silence
		// this field exists to remove. A read-only template really has no process; an
		// extension whose channel closed has one that is gone, and the reason it went is
		// worth more than the distinction.
		if closedShellID != "" {
			out.PidUnknown = "the channel this extension ran on has closed (" + closedReason + "); there is no process left to name"
		} else {
			out.PidUnknown = "this template declares no shell: it is served by its connection, so there is no channel process to name"
		}
		return out
	}
	out.SessionStatus = sessionStatus(sessionID)
	if cs := lookup(shellID); cs != nil {
		out.Alive = cs.Alive()
		if code := cs.ExitCode(); code != nil {
			out.ExitCode = code
		}
	}
	// THE PID IS NOT AVAILABLE OVER SSH, and saying so is the honest answer: the child
	// runs on the target host and the protocol has no field for its pid, so a number
	// here could only be this daemon's own — a value a caller can check and find wrong.
	out.PidUnknown = "the channel runs on the target host over SSH; the protocol does not carry its pid. `alive` is measured from the channel's own transport"
	return out
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
	// BOTH ARE READ FROM THE SAME SNAPSHOT AS THE REST OF THE ROW, so the reply cannot
	// describe a restart count from one moment and a last-use time from another.
	//
	// LAST USE IS A FACT ABOUT THE CALLER, NOT ABOUT HEALTH: it is reported for every
	// extension, not only the lazy ones whose idle timer happened to need it.
	out.Restarts = d.restarts
	if !d.lastUse.IsZero() {
		out.LastActiveAt = d.lastUse.UnixMilli()
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

// ownsShell reports whether this extension holds the channel handle: its own default
// channel, one of the template's other channels, or an instance handed to a caller.
func (st *extState) ownsShell(shellID string) bool {
	_, ok := st.channelOfShell(shellID)
	return ok
}

// channelOfShell names the channel a handle belongs to, "" for the extension's own
// default channel.
//
// THE NAME IS WHAT MAKES THE KIND ANSWERABLE (issue #316): "which channel is this" is
// the question a caller asks to find out whether the process on it is a stdio MCP
// server or a terminal, and a handle alone cannot answer it — the template can hold
// several channels of different kinds (issue #331), so the shell has to be placed in
// one of them before its kind means anything.
func (st *extState) channelOfShell(shellID string) (string, bool) {
	d := st.load()
	if d.shellID == shellID {
		return "", true
	}
	for _, run := range d.sideRuns {
		if run.shellID == shellID {
			return run.name, true
		}
	}
	for _, ex := range d.extra {
		if ex.shellID == shellID {
			// AN INSTANCE'S CHANNEL IS THE DEFAULT ONE, and its label must not be read as
			// a channel name: "fresh" resolves the template's default channel
			// (bringUp(fresh=true)), while the label is what a human sees in `ls`.
			return "", true
		}
	}
	return "", false
}
