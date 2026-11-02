package extensions

import (
	"context"
	"log/slog"
)

// syncNamed is the store's subscriber, and it only queues: it runs inside another
// component's write, holding that component's lock, so anything it did here would
// have to be deadlock-free against a lock it does not own. The run loop does the
// work, serialized, one template at a time.
func (r *Reconciler) syncNamed(name string) {
	select {
	case r.syncs <- name:
	default:
		// The queue is full, which means the loop is behind by dozens of writes. Drop
		// the oldest instead of losing this one: the newest state of a name is the one
		// that matters, and a dropped name would leave a template never re-read.
		select {
		case <-r.syncs:
		default:
		}
		select {
		case r.syncs <- name:
		default:
		}
	}
}

// reviveTargets is the profile store's subscriber: a rewritten profile is the fix
// an extension that could not resolve its target was waiting for, so every
// extension whose budget is spent on that stage gets one more attempt. It runs one
// pass over the states, dials nothing, and is caused by a write — the same shape as
// every other reaction here.
func (r *Reconciler) reviveTargets(ctx context.Context) {
	r.mu.Lock()
	pairs := make(map[string]*extState, len(r.states))
	for name, st := range r.states {
		pairs[name] = st
	}
	r.mu.Unlock()
	for _, st := range pairs {
		if _, autoRetry := st.flags(); autoRetry || st.lastStage() != "target" {
			continue
		}
		ext, err := r.loadTarget(st.name(), st.host())
		if err != nil || !r.rebuildable(ext) {
			continue
		}
		st.resetRetryBudget()
		r.notifyChange()
		r.retryTargetNow(st.name(), st.host())
	}
}

// Sync makes the runtime match one template: the write that changed it says which,
// so nothing is listed, nothing is diffed, and no walk is needed to find out what
// might have moved.
//
// It is called for the name a save or a delete named, and once per stored template
// at startup. Everything a periodic walk used to do now happens here, for the one
// extension the event was about.
func (r *Reconciler) Sync(ctx context.Context, name string) {
	ext, err := r.store.Load(name)
	if err != nil {
		// A file that no longer exists is a delete; one that no longer validates is
		// reported through its own status rather than dropped, because silently
		// forgetting a broken extension leaves "unavailable forever, reason unknown".
		if r.store.Exists(name) {
			for _, st := range r.statesOfName(name) {
				st.leaveGroup()
				st.mark(StatusUnavailable)
				st.recordFailure("target", "extension config is no longer valid: "+err.Error())
			}
			r.notifyChange()
			return
		}
		r.Forget(name)
		return
	}
	idleLimit, idleErr := ext.IdleTimeoutDuration()
	if idleErr != nil {
		for _, st := range r.statesOfName(name) {
			st.leaveGroup()
			st.mark(StatusUnavailable)
			st.recordFailure("target", idleErr.Error())
		}
		r.notifyChange()
		return
	}
	targets := ext.EffectiveTargets()
	// A HOST THAT LEFT THE TEMPLATE TAKES ITS STATE WITH IT. Nothing else would ever
	// mention it again: the status folds over the declared targets, so an orphan
	// state would keep a connection open that no row accounts for.
	keep := make(map[string]bool, len(targets))
	for _, t := range targets {
		keep[stateKey(name, t.Host)] = true
	}
	for _, st := range r.statesOfName(name) {
		if keep[stateKey(name, st.host())] {
			continue
		}
		r.dropState(st)
	}

	for _, t := range targets {
		view := forTarget(ext, t.Host)
		st := r.stateFor(name, t.Host)
		st.setRun(ctx)
		st.setLazy(view.Lazy(), idleLimit)

		if !ext.IsEnabled() {
			// A parked extension holds nothing: its channels and its tunnels are closed,
			// and the connection it shared is left to the extensions that are still
			// enabled. Its retry budget is handed back too, so enabling it again is an
			// immediate attempt rather than a resumed wait.
			had := st.leaveGroup()
			st.stopIdle()
			if had || st.statusOf() != StatusDisabled {
				r.detach(st)
				st.mark(StatusDisabled)
				st.resetRetryBudget()
				r.notifyChange()
			}
			continue
		}

		// Both readings happen unconditionally and are combined after: setGroup answers
		// "did the connection move", changed answers "is this a different template", and
		// chaining them with || was the bug — a template that moved groups short-circuited
		// the fingerprint update, so the next pass saw a stale fingerprint, called it a
		// change, and tore down the channel it had just built.
		moved := st.setGroup(groupKey(&view))
		differed := st.changed(&view)
		if moved || differed {
			// SAVING A TEMPLATE STOPS WHAT IT STARTED. A process running the old command
			// is not what the file now describes, and leaving the two disagreeing means
			// every later question ("why is it behaving like that") has two answers. The
			// channels go, the tunnels go, and the file is read again for whatever comes
			// next — for a lazy extension that is the next ask, for a resident one it is
			// the attempt below.
			r.closeInstances(st, fingerprint(&view), "the template was edited")
			r.detach(st)
			st.stopIdle()
			st.mark(StatusUnavailable)
			st.resetRetryBudget()
			r.notifyChange()
		}

		// The connection is not a caller's to hold: a channel belongs to the caller that
		// asked for it, and the connection under it is the install's, kept for whoever
		// asks next. Registering the group is what keeps it alive.
		r.group(groupKey(&view), &view)

		if view.Lazy() {
			// ON DEMAND: NOTHING IS STARTED HERE (issue #183). A channel exists because a
			// caller asked for one; this only keeps what exists honest — drop the ones
			// that ended, close the ones whose silence has run out — and re-arms the
			// window from the template as it is now.
			r.sweepInstances(name, &view, st, idleLimit)
			r.armIdle(name, st)
			continue
		}
		// A resident extension declares a forward: a listener has no caller whose
		// absence could mean "idle", so it is brought up and kept.
		st.setIdleLimit(idleLimit)
		r.retryTargetNow(name, t.Host)
	}
}

// statesOfName returns every state filed under one extension name, whatever
// hosts it currently declares.
func (r *Reconciler) statesOfName(name string) []*extState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*extState, 0, 1)
	for _, st := range r.states {
		if st.name() == name {
			out = append(out, st)
		}
	}
	return out
}

// dropState releases one host's state and forgets it.
func (r *Reconciler) dropState(st *extState) {
	st.stopIdle()
	st.leaveGroup()
	r.closeInstances(st, "", "the target was removed from the template")
	r.detach(st)
	r.mu.Lock()
	delete(r.states, stateKey(st.name(), st.host()))
	r.mu.Unlock()
	r.closeUnnamedGroups()
}

// Forget drops an extension that is no longer stored: its channels, its tunnels,
// its state, and the connection nothing else names.
func (r *Reconciler) Forget(name string) {
	for _, st := range r.statesOfName(name) {
		st.stopIdle()
		st.leaveGroup()
		r.closeInstances(st, "", "the extension was removed")
		r.detach(st)
		r.mu.Lock()
		delete(r.states, stateKey(name, st.host()))
		r.mu.Unlock()
	}
	r.closeUnnamedGroups()
	r.notifyChange()
	slog.Debug("extension removed", "name", name)
}

// closeUnnamedGroups closes every connection no state names any more.
func (r *Reconciler) closeUnnamedGroups() {
	r.mu.Lock()
	groups := make([]*extGroup, 0, len(r.groups))
	for _, g := range r.groups {
		groups = append(groups, g)
	}
	r.mu.Unlock()
	for _, g := range groups {
		if r.groupNamed(g.key) {
			continue
		}
		r.closeGroup(g)
		r.mu.Lock()
		delete(r.groups, g.key)
		r.mu.Unlock()
		slog.Debug("extension session closed", "group", g.key, "target", g.target)
	}
}

// run reconciles on events, not on a clock. Every write that can change the
// runtime — saving a template, deleting one, parking one, a profile a failing
// extension was waiting for — arrives through Trigger, so the work happens when
// something happened and never because a timer ticked.
//
// THE PERIODIC WALK IS GONE ON PURPOSE (issue #183). Its jobs went to the thing
// that owns each deadline: an idle channel closes on its own timer, a failed
// attempt is retried on its own timer, and a channel whose process ended is
// noticed by whoever reads it next. What is left is "the store says something
// different from what I hold", which is exactly what an event means.
func (r *Reconciler) run(ctx context.Context) {
	defer close(r.done)
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	// The startup pass: resident extensions are established here, because a
	// template that declares a forward has no caller whose ask could start it.
	if names, err := r.store.List(); err == nil {
		for _, name := range names {
			r.Sync(ctx, name)
		}
	} else {
		slog.Warn("extension start failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-r.syncs:
			r.Sync(ctx, name)
		case <-r.revive:
			r.reviveTargets(ctx)
		}
	}
}

// startProbe marks the extension probing and runs the attempt in its own
// goroutine, because a dial to an unreachable host blocks for its timeout and
// must not delay the other extensions or the walk.
func (r *Reconciler) startProbe(ctx context.Context, name, target string) {
	r.startProbeTask(ctx, name, target, "", nil, hostPlan{})
}

func (r *Reconciler) startPreparedProbe(ctx context.Context, name, target string, ext *Extension, plan hostPlan) {
	r.startProbeTask(ctx, name, target, "prepared", ext, plan)
}

func (r *Reconciler) startProbeTask(ctx context.Context, name, target, mode string, ext *Extension, plan hostPlan) {
	if ctx == nil {
		return
	}
	st := r.stateFor(name, target)
	st.mu.Lock()
	if st.status == StatusProbing {
		st.mu.Unlock()
		return
	}
	st.status = StatusProbing
	st.mu.Unlock()
	r.notifyChange()
	r.probes.Add(1)
	go func() {
		defer r.probes.Done()
		r.probe(ctx, name, target)
		if mode == "prepared" {
			r.probePlan(ctx, name, target, ext, plan)
			return
		}
		r.probe(ctx, name, target)
	}()
}
