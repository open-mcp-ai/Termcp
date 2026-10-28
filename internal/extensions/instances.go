package extensions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
)

// reapEnded drops the channels whose process is gone, and says so.
//
// THIS IS A PULL, NOT A PUSH (issue #183). No walk ticks behind the reconciler
// any more, so a channel whose process exited is noticed by whoever asks about it
// next — which is the caller that was about to drive it. Nothing is lost by the
// delay: a caller reading the status gets the truth, and a caller reading the
// channel gets the exit reason from the read itself. The alternative was a
// goroutine polling every extension every five seconds to keep a line in a
// listing fresh.
func (r *Reconciler) reapEnded(name string, st *extState) {
	st.mu.Lock()
	empty, lazy := len(st.extra) == 0, st.lazy
	st.mu.Unlock()
	if empty || !lazy {
		return
	}
	sess := r.attachedSession(st)
	if sess == nil {
		return
	}
	changed := false
	for _, shellID := range st.extraShells() {
		if shellID == "" || liveShell(sess, shellID) != nil {
			// A template with no shell (a read-only file window) is served by the
			// connection itself, so its instance has no channel to test and no
			// process to outlive. Its liveness is the session's, which is the hook's
			// news — not this loop's.
			continue
		}
		if !st.dropExtra(shellID) {
			continue
		}
		reason := r.instanceEndReason(sess, shellID)
		st.recordFailure("session", reason)
		st.recordClosed(sessIDOf(sess), shellID, reason)
		st.mark(StatusUnavailable)
		slog.Debug("extension instance ended", "name", name, "shell_id", shellID, "reason", reason)
		changed = true
	}
	if changed {
		r.settle(name, st)
		r.notifyChange()
	}
}

// Acquire is how a caller asks for an instance of an on-demand extension: it
// reuses one that has been silent long enough to be nobody's, or brings up a new
// one, and returns the handles (issue #183). It is the whole of `open ext <name>`.
//
// THREE BRANCHES, and they are the caller's rule verbatim:
//
//	one of the instances has been silent for the whole idle window
//
// t                            -> nobody is using it; take it
//
//	there is no instance at all   -> bring one up
//	they were all used recently   -> bring up another one
//
// A template that declares a forward is not on demand and has exactly one
// instance, so its callers all get that one; the reconciler owns its lifecycle
// and there is nothing to choose between.
//
// The wait bounds how long the caller waits for a DIAL, not whether it gets an
// instance: a host that is slow to answer returns `probing` with what to poll
// rather than a failure the caller would have to guess was not one.
func (r *Reconciler) Acquire(name string, wait time.Duration) (Status, error) {
	ext, err := r.store.Load(name)
	if err != nil {
		return Status{}, err
	}
	st, ok := r.Status(name)
	if !ok {
		return Status{}, fmt.Errorf("extension %q not found", name)
	}
	if !ext.IsEnabled() {
		return st, nil
	}
	limit, err := ext.IdleTimeoutDuration()
	if err != nil {
		// A template carrying an unreadable window is a broken template, and
		// refusing it is the only honest answer to a caller asking it to start one.
		return Status{}, err
	}
	if !ext.Lazy() {
		// Resident: the reconciler keeps exactly one instance and everyone shares
		// it. Wanting it is what starts it, and the attempt begins now rather than
		// at whatever moment a sweep would have come round.
		r.state(name).want()
		r.retryNow(name)
		return r.await(name, wait)
	}

	// EVERY ASK GETS ITS OWN CHANNEL — not a free one, not a reused one. A channel
	// somebody else may still be driving is the one thing that cannot be handed
	// over, and deciding "is that one free" costs more code than a fork does. The
	// process is the cheap part; callers never share a cursor.
	state := r.state(name)
	state.want()
	state.setIdleLimit(limit)
	sid, shellID, _, _, err := r.bringUp(context.Background(), ext, state, true)
	if err != nil {
		// The failure is already recorded and its retry armed; the caller's answer is
		// the status, which carries the reason.
		got, _ := r.Status(name)
		return got, nil
	}
	return r.instanceStatus(name, sid, shellID), nil
}

// closeInstances closes every instance of an on-demand extension, recording why.
// It is what an edit and a delete have in common: the processes that were running
// no longer match what the template says, so they go, and the handles that named
// them answer with the reason.
func (r *Reconciler) closeInstances(st *extState, keepFP, reason string) {
	sess := r.sessionOfRecord(st)
	for _, stale := range st.extraShellsNotBuiltFrom(keepFP) {
		if st.dropExtra(stale) {
			if sess != nil {
				_ = sess.CloseChildShell(stale)
			}
			st.recordClosed(sessIDOf(sess), stale, reason)
		}
	}
}

// sessionOfRecord returns the session object this extension names, live or not.
// Closing a channel is about the channel, not about the connection's health: a
// session that has gone DEAD still holds the shells that were in it, and a label
// held by a channel nobody closed is a label the next instance cannot have.
func (r *Reconciler) sessionOfRecord(st *extState) *session.Session {
	if id := st.sessionOf(); id != "" && r.sessMgr != nil {
		return r.sessMgr.Get(id)
	}
	return nil
}

// instanceEndReason reads why a channel ended, for the failure a caller sees. It
// is channelEndReason for a shell id the extension no longer reports as its own.
func (r *Reconciler) instanceEndReason(sess *session.Session, shellID string) string {
	if sess != nil {
		if shell := sess.GetChildShell(shellID); shell != nil {
			if reason := exitReason(r.msgMgr, r.sessMgr, shell); reason != "" {
				return reason
			}
		}
	}
	return "the instance's process is no longer running"
}

// bringUp is the only way an extension is brought to available: resolve the
// template, make sure the connection exists, open the channel it needs, and record
// what happened.
//
// FRESH ASKS FOR A NEW CHANNEL RATHER THAN THE EXTENSION'S OWN (issue #183). An
// on-demand caller gets a process nobody else is driving; the reconciler's own
// extension reuses the channel it already has, so a retry after a tunnel failure
// does not restart the process. Everything else — the resolve, the dial, the
// failure stages, the budget, the status — is the same work, which is why it is one
// function: two copies of it is how the two paths start disagreeing about what
// "dial failed" means.
func (r *Reconciler) bringUp(ctx context.Context, ext *Extension, st *extState, fresh bool) (sessionID, shellID, label, stage string, err error) {
	name := st.name()
	plan, err := r.resolve(ext)
	if err != nil {
		// The stage is the diagnosis: a template that does not resolve is fixed by
		// editing it, not by waiting, so the budget is spent rather than climbed.
		r.failForGood(st, "target", err.Error())
		return "", "", "", "target", err
	}
	g := r.groupFor(st)
	if g == nil {
		// Nothing has registered this extension's group yet: the first ask can arrive
		// before the first sync.
		g = r.group(groupKey(ext), ext)
	}
	sess, err := r.ensureGroupSession(ctx, g, plan.config)
	if err != nil {
		reason := sshclient.DescribeDialError(err)
		r.fail(st, "dial", reason)
		return "", "", "", "dial", errors.New(reason)
	}

	if fresh {
		label = ""
		if plan.shell {
			label = st.nextLabel(ext)
			shell, cerr := sess.CreateChildShell(plan.command, plan.args, plan.pty, plan.rows, plan.cols, label)
			if cerr != nil {
				r.fail(st, "start", cerr.Error())
				return "", "", "", "start", cerr
			}
			shellID = shell.ID
			// A FRESH CHANNEL GETS THE SAME GRACE THE RECONCILER'S OWN DOES.
			//
			// This branch used to mark the extension available the instant the fork
			// returned, so a template whose program does not exist answered
			// `available` with a handle to a process that had already exited — and the
			// caller found out on the next read, or, worse, sent its first JSON-RPC
			// frame into an empty stream. `--command nosuchprogram` is a typo a reader
			// makes on the first day (dogfood on #183).
			if serr := r.waitForStart(sess, shell); serr != nil {
				r.fail(st, "start", serr.Error())
				st.recordClosed(sess.ID, shellID, serr.Error())
				return "", "", "", "start", serr
			}
		}
		// A template with no shell is served by the connection alone: a read-only file
		// window has no channel to hand out, and its instance is the session.
		st.addExtra(sess.ID, shellID, label, fingerprint(ext))
		r.armIdle(name, st)
		st.mark(StatusAvailable)
		st.resetRetryBudget()
		r.notifyChange()
		slog.Debug("extension instance up", "name", name, "session_id", sess.ID, "shell_id", shellID)
		return sess.ID, shellID, label, "", nil
	}

	shellID, err = r.ensureMemberShell(plan, st, sess)
	if err != nil {
		// The connection is up and shared; only this extension's process failed to
		// start, so the retry is a channel open rather than a redial.
		r.fail(st, "start", err.Error())
		return "", "", "", "start", err
	}
	st.attach(sess.ID, shellID)
	// A fresh instance starts with a fresh clock, or the first sync after it came up
	// reads the moment of the ASK as its last use and reaps it early.
	st.touch()

	if err := r.ensureForwards(ext, st); err != nil {
		// The session is up and usable; only the tunnel is missing, so the retry
		// targets the forward alone instead of tearing the connection down.
		r.fail(st, "forward", err.Error())
		return sess.ID, shellID, "", "forward", err
	}
	// Proven up: the budget is whole again, so a later failure starts the ladder over
	// rather than resuming a count from before the extension was working.
	st.resetRetryBudget()
	st.mark(StatusAvailable)
	slog.Debug("extension available", "name", name, "session_id", sess.ID, "shell_id", shellID)
	r.notifyChange()
	return sess.ID, shellID, "", "", nil
}

// nextLabel names an instance's channel. The label has to be unique inside the
// session (#188 refuses a duplicate), and it is what a human reading `ls <session>`
// sees, so the first instance keeps the extension's own name and the rest count up
// from there. The counter never goes back, for the reason it exists.
func (st *extState) nextLabel(ext *Extension) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.labels++
	if st.labels == 1 {
		return ext.Name
	}
	return fmt.Sprintf("%s#%d", ext.Name, st.labels)
}

// instanceStatus renders a status for the instance just handed out: the
// extension's own state with THIS caller's handles on it, which is what the
// reply needs and what `Status` alone cannot say when there is more than one.
func (r *Reconciler) instanceStatus(name, sessionID, shellID string) Status {
	got, _ := r.Status(name)
	got.SessionID, got.ShellID = sessionID, shellID
	return got
}

// await polls until the extension has an answer: up, or down with a reason. Used
// by the resident path, where the walk owns the dial.
func (r *Reconciler) await(name string, wait time.Duration) (Status, error) {
	deadline := time.Now().Add(wait)
	for {
		got, ok := r.Status(name)
		if !ok {
			return Status{}, fmt.Errorf("extension %q not found", name)
		}
		switch got.Status {
		case StatusAvailable, StatusDisabled:
			return got, nil
		case StatusUnavailable:
			// Down WITH a reason is an answer; down without one is still the first
			// attempt. The distinction is what the caller does next: report the
			// failure, or keep waiting.
			if len(got.Failures) > 0 {
				return got, nil
			}
		}
		if time.Now().After(deadline) {
			return got, nil
		}
		time.Sleep(acquirePoll)
	}
}

// acquirePoll is how often Acquire re-reads the status. The walk itself runs on
// the loop interval; this only decides how quickly the answer is noticed, and a
// dial to a healthy host answers in tens of milliseconds.
const acquirePoll = 25 * time.Millisecond

// sweepInstances is the walk's whole job for an on-demand extension: check that
// the channels it handed out are still there, close the ones whose silence has run
// out, and say what the extension is holding (issue #183).
//
// It never opens anything. An instance exists because a caller asked for one, and
// the only thing that can happen to it here is that it ends — because its process
// died, or because nobody has touched it for the whole idle window.
func (r *Reconciler) sweepInstances(name string, ext *Extension, st *extState, limit time.Duration) {
	// THE CHANNELS THAT ENDED ARE DROPPED, NOT REOPENED. That is the difference
	// between an extension's own channel — infrastructure the reconciler repairs —
	// and a caller's instance, which is a lease it lets lapse. The body is
	// reapEnded's: a status read does the same thing, and one of them having a
	// special case the other lacks is how a listing starts lying.
	r.reapEnded(name, st)
	if r.reapDue(name, st, limit) > 0 {
		r.notifyChange()
	}
	r.settle(name, st)
}

// reapDue closes the channels whose silence has run out and reports how many it
// closed. Both the timer and the walk call it: the timer because the window is a
// promise, the walk because a promise kept only by a timer is one missed callback
// away from a process nobody will ever close.
func (r *Reconciler) reapDue(name string, st *extState, limit time.Duration) int {
	sess := r.sessionOfRecord(st)
	reason := "idle for " + limit.Round(time.Second).String() + " with nothing asked of it"
	due := st.takeDueExtras(limit, sessIDOf(sess), reason)
	if len(due) == 0 {
		return 0
	}
	for _, shellID := range due {
		if sess != nil {
			_ = sess.CloseChildShell(shellID)
		}
		slog.Info("extension instance idled out", "name", name, "shell_id", shellID)
	}
	return len(due)
}

// armIdle points the extension's timer at its earliest silence deadline. Called
// wherever a channel is opened, renewed or dropped, because those are the only
// moments the answer changes.
func (r *Reconciler) armIdle(name string, st *extState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	limit := st.idleLimit
	if limit <= 0 || len(st.extra) == 0 {
		return
	}
	var next time.Time
	for _, ex := range st.extra {
		if d := ex.lastUse.Add(limit); next.IsZero() || d.Before(next) {
			next = d
		}
	}
	st.timer = time.AfterFunc(time.Until(next), func() {
		if st.ctx != nil && st.ctx.Err() != nil {
			return
		}
		r.idleFired(name)
	})
}

// stopIdle disarms the timer for a state that is going away.
func (st *extState) stopIdle() {
	st.mu.Lock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.mu.Unlock()
}

// idleFired is the window running out. It closes what is due and re-arms for the
// next channel, rather than closing everything: channels opened at different
// moments run out at different moments.
func (r *Reconciler) idleFired(name string) {
	ext, err := r.store.Load(name)
	if err != nil || !ext.Lazy() {
		return
	}
	limit, err := ext.IdleTimeoutDuration()
	if err != nil {
		return
	}
	st := r.state(name)
	if r.reapDue(name, st, limit) > 0 {
		r.settle(name, st)
		r.notifyChange()
	}
	r.armIdle(name, st)
}

// settle makes the reported status follow what is actually held: something up is
// available, nothing up is idle ("nothing is running because nothing asked"), and
// a recorded failure outranks both.
func (r *Reconciler) settle(name string, st *extState) {
	switch {
	case len(st.snapshot().Failures) > 0 && st.instanceCount() == 0:
		// leave the failure standing
	case st.instanceCount() > 0:
		if st.statusOf() != StatusAvailable {
			st.mark(StatusAvailable)
			r.notifyChange()
		}
	default:
		if st.statusOf() != StatusIdle {
			st.mark(StatusIdle)
			r.notifyChange()
		}
	}
}

// sessIDOf is the session id of a live session, or empty for none.
func sessIDOf(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	return sess.ID
}

// Touch renews the idle clock of whichever instance this call addressed. It is
// called wherever a tool call resolves a session or a channel, because that is the
// only moment the daemon learns that somebody is still driving an on-demand
// extension — the alternative is reaping a channel in the middle of a read.
//
// The shell id is the precise answer and is tried first; a call that named only
// the session renews every instance on it, which is the honest reading of "this
// connection is in use". A session that serves no extension, or one that is not
// on demand, is a no-op.
func (r *Reconciler) Touch(shellID, sessionID string) {
	if shellID == "" && sessionID == "" {
		return
	}
	// The name comes along because renewing has to re-arm that extension's timer,
	// and the timer goes back through the store by name.
	r.mu.Lock()
	pairs := make(map[string]*extState, len(r.states))
	for name, st := range r.states {
		pairs[name] = st
	}
	r.mu.Unlock()

	// A CHANNEL ID NAMES ONE INSTANCE, and only that one is in use.
	//
	// This used to be a single loop trying shellID then sessionID per extension,
	// returning at the first hit — and every extension on the same connection
	// matches the session, so renewing A's channel could land on a sibling B and
	// stop there, leaving A's timer to fire on schedule while its channel was being
	// read. With one extension it looked perfect; with ten on one connection it
	// reaped the channel being used almost every time (dogfood on #183).
	if shellID != "" {
		for name, st := range pairs {
			if st.renew(shellID) {
				r.armIdle(name, st)
				return
			}
		}
	}
	if sessionID != "" {
		// The session IS in use, and every extension hanging off it is being used
		// with it — so all of them are renewed, not just whichever came first.
		for name, st := range pairs {
			if st.renewSession(sessionID) {
				r.armIdle(name, st)
			}
		}
	}
}

// Release hands back an instance a caller is done with. It is the explicit half of
// the idle window: the caller that knows it has finished says so, and the instance
// stops costing a process on the target immediately instead of when its silence
// runs out (issue #183).
//
// The shell id names which instance; an empty one releases every instance of the
// extension, which is what "I am done with this extension" means when the caller
// did not keep the handle. It reports how many were released, so the reply can say
// whether the caller's handle was one of them.
func (r *Reconciler) Release(name, shellID string) (int, error) {
	ext, err := r.store.Load(name)
	if err != nil {
		return 0, err
	}
	if !ext.Lazy() {
		// A resident extension's connection is the reconciler's, not the caller's:
		// its template declares a listener, so there is nothing to hand back.
		return 0, errors.New("this extension is not on demand: it declares a forward, so its connection stays up")
	}
	st := r.state(name)
	var targets []string
	if shellID != "" {
		if !st.hasExtra(shellID) {
			return 0, fmt.Errorf("extension %q is not running an instance with shell id %q", name, shellID)
		}
		targets = []string{shellID}
	} else {
		targets = st.extraShells()
	}
	sess := r.sessionOfRecord(st)
	n := 0
	for _, id := range targets {
		if !st.dropExtra(id) {
			continue
		}
		if sess != nil {
			_ = sess.CloseChildShell(id)
		}
		st.recordClosed(sessIDOf(sess), id, "released by the caller")
		n++
	}
	if st.instanceCount() == 0 && st.statusOf() == StatusAvailable {
		st.mark(StatusIdle)
	}
	r.armIdle(name, st)
	r.notifyChange()
	return n, nil
}

// hasExtra reports whether this shell id names one of the extension's instances.
// anyInstanceOn reports whether one of this extension's instances runs on the
// session, which is what "this call is about this extension" means for a lazy one.
func (st *extState) anyInstanceOn(sessionID string) bool {
	for _, ex := range st.extra {
		if ex.sessionID == sessionID {
			return true
		}
	}
	return false
}

func (st *extState) hasExtra(shellID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, ex := range st.extra {
		if ex.shellID == shellID {
			return true
		}
	}
	return false
}

// ExplainClosed answers "what happened to the handle I was using" for a session
// an on-demand extension gave up. It is what turns the bare not-found reply into
// the fact and the way forward (issue #183).
func (r *Reconciler) ExplainClosed(sessionID, shellID string) (name, reason string, at time.Time, ok bool) {
	if sessionID == "" && shellID == "" {
		return "", "", time.Time{}, false
	}
	r.mu.Lock()
	states := make(map[string]*extState, len(r.states))
	for n, st := range r.states {
		states[n] = st
	}
	r.mu.Unlock()
	for n, st := range states {
		st.mu.Lock()
		match := (sessionID != "" && st.closedSessionID == sessionID) ||
			(shellID != "" && st.closedShellID == shellID)
		reason, at = st.closedReason, st.closedAt
		st.mu.Unlock()
		if match {
			return n, reason, at, true
		}
	}
	return "", "", time.Time{}, false
}
