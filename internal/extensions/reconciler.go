package extensions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// Reconciler keeps every enabled extension's session warm. It is the half of the
// extension layer that has a lifecycle: the Store answers "what did the author
// ask for" and this answers "is it actually working right now".
//
// A session is not per extension but per host and capability set: extensions with
// the same target and the same whitelist are served by one connection — the
// singleton, created when the first of them comes up, reused by the rest and
// rebuilt if it dies — and each of them drives its own channel inside it, or its
// own SFTP view, or its own tunnels. That is what makes one extension per tool
// affordable: an MCP server, a read-only skill window and a tunnel to the same
// machine cost one connection rather than three.
//
// The loop is deliberately passive about how an extension is used once it is up.
// Bringing an extension to available means having the group's connection and
// proving the extension's own channel (or, for a template that declares no shell,
// the connection alone) can stay up; nothing else. An agent then drives it with
// the ordinary shell/SFTP tools, gated by the session's Restriction. The
// reconciler never reads a skill or speaks JSON-RPC on the agent's behalf.
type Reconciler struct {
	store      *Store
	sessMgr    *session.Manager
	msgMgr     *message.Manager
	sshConfigs *sshconfig.Store
	forwardMgr *forward.ForwardManager

	// NoInternal refuses the built-in loopback profile as an extension target,
	// mirroring the flag the MCP server exposes.
	NoInternal bool

	// mu guards the two maps; a group's own session and an extension's own
	// attachment are behind their own locks, because a probe goroutine writes them
	// while the walk reads them. It also guards cancel, so Start and Stop can be
	// called from different goroutines.
	// states and groups are sync.Maps: every lookup, insert and delete touches one
	// entry, and whole-map walks (status lists, garbage collection) Range over a
	// snapshot. ZERO LOCKS ON THE RECONCILER.
	states sync.Map // key string -> *extState
	groups sync.Map // key string -> *extGroup

	// onChange is atomic so the callback fires without any lock held.
	onChange atomic.Pointer[func()]

	syncs  chan string
	revive chan struct{}

	runCtx atomic.Pointer[context.Context]
	cancel atomic.Pointer[context.CancelFunc]
	done   chan struct{}

	// probes tracks the connection attempts the run loop has launched and not yet
	// heard back from. Stop waits on it so a reconciler that has been told to stop
	// cannot leave a session behind that it is no longer tracking — a probe that
	// dials after Stop returns would create exactly that.
	probes sync.WaitGroup
}

// Status names the reconciler's report on one extension. An extension that has
// never been tried, one whose dial is in flight, and one whose process exited
// five minutes ago are all reported distinctly, because the caller's next move
// differs for each.
const (
	StatusDisabled    = "disabled"    // enabled=false: the reconciler will not touch it
	StatusIdle        = "idle"        // nothing is running because nothing has asked; the next use starts it
	StatusUnavailable = "unavailable" // not up; either not yet tried or down after a failure
	StatusProbing     = "probing"     // a connection attempt is in flight
	StatusAvailable   = "available"   // session established, process alive, forwards up
)

// Status is the runtime half of an extension. The template half stays in the
// Store; the two are merged for whoever asks (the MCP tool, the Web UI).
type Status struct {
	Name string `json:"name"`
	// Status is one of the five above.
	Status string `json:"status"`
	// Lazy is true for an extension the reconciler starts on demand rather than at
	// startup (every enabled template that declares no forward).
	Lazy bool `json:"lazy,omitempty"`
	// SessionID is the connection this extension is served by. Extensions that
	// share a host and a capability set report the same one.
	SessionID string `json:"session_id,omitempty"`
	// ShellID is the channel this extension runs on, and the handle an agent
	// drives it through. It is empty for a template that declares no shell: a
	// read-only file window has no channel of its own.
	ShellID  string          `json:"shell_id,omitempty"`
	Forwards []ForwardStatus `json:"forwards,omitempty"`
	Failures []Failure       `json:"failures,omitempty"`
	// NextRetry is when the reconciler tries again (Unix ms); 0 = no retry
	// scheduled.
	NextRetry int64 `json:"next_retry,omitempty"`
	// IdleTimeoutMs is how long a lazy extension stays up after its last use, and
	// IdleForMs is how long ago that was; both are reported so "why is this not
	// up" and "how long have I got" are facts rather than guesses.
	IdleTimeoutMs int64 `json:"idle_timeout_ms,omitempty"`
	IdleForMs     int64 `json:"idle_for_ms,omitempty"`
	// Instances is how many callers this extension is currently serving: one
	// channel each, all on the group's connection (issue #183).
	Instances int `json:"instances,omitempty"`
	// ClosedAt and ClosedSessionID describe the instance the reconciler took away:
	// a caller holding the old handle is told what happened to it instead of
	// reading "not found".
	ClosedAt        int64  `json:"closed_at,omitempty"`
	ClosedSessionID string `json:"closed_session_id,omitempty"`
	ClosedShellID   string `json:"closed_shell_id,omitempty"`
	ClosedReason    string `json:"closed_reason,omitempty"`

	// Targets reports the per-host breakdown when the extension declares multiple
	// targets. Omitted on single-target templates so the wire format stays small.
	Targets []TargetStatus `json:"targets,omitempty"`
}

// maxFailures caps the history. Enough to see a pattern, not so much that a
// flapping extension makes the status unbounded.
const maxFailures = 8

// backoffSteps spaces the automatic retries. The steps are short on purpose:
// most extension failures are a wrong command or a sleeping laptop, and either
// is fixed by a human, so a retry exists to recover the moment the underlying
// condition clears.
var backoffSteps = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
}

// maxAutoRetries is how many times the reconciler brings an extension back up on
// its own. It is one per backoff step on purpose: the ladder and the budget are
// the same length, so a spent extension is one that has climbed the whole
// ladder. Three covers a transient (a laptop waking, a briefly missing profile,
// a slow listener); past it the extension waits for a nudge — the panel's retry
// key or the agent's extension(action=retry) — rather than hammering a host that
// is not coming back.
const maxAutoRetries = 3

// probeGrace is how long a started process gets to prove it can stay alive. A
// stdio MCP server with a bad command dies within milliseconds; a healthy one
// blocks on its first read forever, so the wait costs nothing for it.
const probeGrace = 700 * time.Millisecond

// failureTailBytes is the slice of output captured as the failure reason when a
// process exits during its grace window. It is short because it is a reason, not
// a transcript: the agent that wants the whole output reads the session itself.
const failureTailBytes = 512

// NewReconciler wires the reconciler to the stores and managers it drives. It
// subscribes to the extension store's change callback itself, so a saved
// extension is reconciled without the caller remembering to poke anything.
func NewReconciler(store *Store, sessMgr *session.Manager, msgMgr *message.Manager, sshConfigs *sshconfig.Store, forwardMgr *forward.ForwardManager) *Reconciler {
	r := &Reconciler{
		store:      store,
		sessMgr:    sessMgr,
		msgMgr:     msgMgr,
		sshConfigs: sshConfigs,
		forwardMgr: forwardMgr,

		syncs:      make(chan string, 64),
		revive:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if store != nil {
		// The write names the template, so the runtime does that one and nothing
		// else. This is the whole of "the store changed" — there is no listing to
		// diff and no tick to wait for.
		store.SetOnChange(r.syncNamed)
	}
	if sshConfigs != nil {
		// A profile rewritten by hand is the fix a failing extension was waiting for.
		// It has no name to give (it is not an extension), so the extensions that gave
		// up on a missing target get one more attempt.
		sshConfigs.SetOnChange(func() {
			select {
			case r.revive <- struct{}{}:
			default:
			}
		})
	}
	if sessMgr != nil {
		sessMgr.AddOnDeadHook(r.onSessionDead)
	}
	return r
}

// Store returns the extension store the reconciler reconciles. Callers that
// write extensions (the MCP extension tool) go through it rather than keeping a
// second handle, so a save still reaches this reconciler through the store's
// own change subscription.
func (r *Reconciler) Store() *Store { return r.store }

// SSHConfigs returns the profile store the templates resolve against. A profile
// created after an extension that needed it is the recovery the walk is waiting
// for, and writing it through this handle notifies the reconciler at once.
func (r *Reconciler) SSHConfigs() *sshconfig.Store { return r.sshConfigs }

// SetOnChange subscribes to status transitions (the Web UI's panel and its push
// frames). The callback fires after the state has been written, so a reader sees
// what it is being told about.
func (r *Reconciler) SetOnChange(fn func()) {
	r.onChange.Store(&fn)
}

func (r *Reconciler) notifyChange() {
	if p := r.onChange.Load(); p != nil && *p != nil {
		(*p)()
	}
}

// Start loads every extension and begins maintaining them. Returning an error
// only for a store that cannot be walked: one broken extension is reported in
// its own status, not as a failure to start the reconciler.
func (r *Reconciler) Start(ctx context.Context) error {
	cctx, cancel := context.WithCancel(ctx)
	if !r.cancel.CompareAndSwap(nil, &cancel) {
		cancel()
		return nil
	}
	if _, err := r.store.List(); err != nil {
		r.cancel.Store(nil)
		cancel()
		return err
	}
	r.sweepRestored()
	go r.run(cctx)
	return nil
}

// sweepRestored removes what a previous process left behind. An extension's
// session serves the run that created it: this process builds its own from the
// templates, so a restored placeholder — a connection nothing can be driven
// through — is dropped rather than kept as an entry that can never be reopened.
func (r *Reconciler) sweepRestored() {
	if r.sessMgr == nil {
		return
	}
	for _, s := range r.sessMgr.ListAll() {
		if ns.IsExtensions(s.Namespace) {
			slog.Debug("dropping restored extension session", "session_id", s.ID, "namespace", s.Namespace)
			_ = r.sessMgr.Delete(s.ID)
		}
	}
}

// Stop cancels the loop, waits for it to exit, and waits again for any probe it
// had in flight. Sessions that are already up stay where they are: they are
// ordinary sessions, and their own teardown follows process exit or the caller's
// shutdown.
func (r *Reconciler) Stop() {
	p := r.cancel.Swap(nil)
	if p == nil || *p == nil {
		return
	}
	(*p)()
	<-r.done
	r.probes.Wait()
}

// probe is one attempt to bring an extension to available. The connection is the
// group's — reused when it is up, created when it is not — and everything else
// belongs to this extension: its channel, its tunnels, its failure history.
func (r *Reconciler) probe(ctx context.Context, name, target string) {
	ext, err := r.loadTarget(name, target)
	if err != nil || !ext.IsEnabled() {
		// Removed or parked between the scheduling and the attempt.
		return
	}
	st := r.stateFor(name, target)
	if r.groupFor(st) == nil {
		return
	}
	// The reconciler's own attempt: it reuses the channel the extension already has,
	// because a retry after a tunnel failure must not restart the process.
	_, _, _, _, _ = r.bringUp(ctx, ext, st, false)
}

// fail records a reason, marks the extension unavailable, and arms the backoff
// ladder. It is the shape of every failure a redial or a reopened channel might
// clear — a dial that timed out, a process that exited, a port that was taken —
// so the four things that must happen together happen in one place.
func (r *Reconciler) fail(st *extState, stage, reason string) {
	st.recordFailure(stage, reason)
	st.mark(StatusUnavailable)
	// ON DEMAND, AN ATTEMPT IS A CALLER'S ASK (issue #183). Arming a retry here
	// would print a NextRetry the timer then declines to honour — the timer's
	// callback goes through retryNow, which returns immediately for a lazy
	// extension. A field that promises a retry nobody scheduled is worse than no
	// field: the caller waits for something that is not coming.
	if st.isLazy() {
		r.notifyChange()
		return
	}
	if wait, ok := st.scheduleBackoff(); ok {
		// THE LADDER IS A TIMER, NOT A POLL (issue #183). The walk used to re-read
		// "has the backoff elapsed" every five seconds, which made a retry due at
		// 1.5s fire somewhere between 1.5 and 6.5 — and it made the reconciler the
		// owner of a deadline it could not keep. One timer per extension keeps the
		// deadline it printed.
		r.armRetry(st, wait)
	}
	r.notifyChange()
}

// failForGood is fail for a failure no retry can fix: the ladder is spent rather
// than climbed, so the walk stops re-probing a condition the probe cannot change
// and the extension waits for an edit, the panel's retry key, or the agent's.
func (r *Reconciler) failForGood(st *extState, stage, reason string) {
	st.recordFailure(stage, reason)
	st.mark(StatusUnavailable)
	st.exhaustRetryBudget()
	r.notifyChange()
}

// rebuildable reports whether the template resolves to a connection at all. It is
// the recovery check the walk makes once the automatic budget is spent: nothing
// dials, nothing is created, and the answer costs a store lookup. A template that
// failed to resolve and now resolves is a fix (a profile recreated by hand), so
// the extension gets its retries back; one that still does not resolve keeps
// waiting for a nudge.
func (r *Reconciler) rebuildable(ext *Extension) bool {
	_, err := r.resolve(ext)
	return err == nil
}

// resolve reads the template's target profile and turns the pair into a plan. It
// is the one place an extension's fields are interpreted, so a session created by
// the reconciler and one created by hand from the same profile are the same
// session.
func (r *Reconciler) resolve(ext *Extension) (hostPlan, error) {
	if r.sshConfigs == nil {
		return hostPlan{}, errors.New("no ssh config store configured")
	}
	ent, err := r.sshConfigs.Load(ext.Target)
	if err != nil {
		if errors.Is(err, sshconfig.ErrNotFound) {
			return hostPlan{}, fmt.Errorf("target profile %q does not exist", ext.Target)
		}
		return hostPlan{}, fmt.Errorf("cannot read target profile %q: %w", ext.Target, err)
	}
	if ent.Kind == sshconfig.KindInternal && r.NoInternal {
		return hostPlan{}, fmt.Errorf("target profile %q is the built-in loopback profile, which is disabled", ext.Target)
	}
	var remote *session.RemoteSSH
	if ent.Kind != sshconfig.KindInternal {
		remote, err = sshconfig.RemoteFromEntry(ent, r.sshConfigs.ConfigDir(ext.Target))
		if err != nil {
			return hostPlan{}, fmt.Errorf("target profile %q: %w", ext.Target, err)
		}
	}
	restriction, err := session.NewRestriction(ext.Allow, ext.Paths)
	if err != nil {
		return hostPlan{}, fmt.Errorf("restriction: %w", err)
	}

	cmd, args := sshconfig.EffectiveCommand(ent, ext.Command, ext.Args)
	// THE TARGET OF A url TEMPLATE IS THE MACHINE THE CONNECTION LEAVES FROM (#233).
	// It is not where the process runs: what runs on the channel is termcp's own
	// bridge, an in-process command only the built-in loopback server knows. So the
	// session stays internal and the far host rides as `--via`, where the relay dials
	// its requests inside that host's SSH connection.
	//
	// The profile is still loaded above, and still has to exist: a template naming a
	// host nothing has configured should fail as "no such profile", not as a
	// connection to somewhere else. What changes is only which machine CARRIES the
	// channel.
	bridgeVia := ""
	if ext.URL != "" && !sshconfig.IsInternalName(ext.Target) {
		bridgeVia = ext.Target
	}
	// A url TEMPLATE'S SESSION IS ALWAYS THE LOOPBACK ONE, because the thing that
	// runs on its channel is termcp's built-in relay and nothing else serves it.
	// The far host travels as `--via` instead, so the session is not "where the
	// process is" but "where the relay stands" — which is this machine.
	sessionHost := ext.Target
	if ext.URL != "" {
		sessionHost = "internal"
		switch {
		case r.NoInternal:
			return hostPlan{}, fmt.Errorf("url requires the built-in loopback profile, which is disabled on this instance")
		case bridgeVia != "":
			// The remote profile was loaded and validated above; drop its transport,
			// because the session itself no longer dials it. `--via` re-dials it from
			// the relay side, through the same pool.
			ent = sshconfig.InternalEntry()
			remote = nil
		default:
			// target = "internal" with a url: the loopback entry already loaded above.
		}
		cmd, args = bridgeCommand(ext, bridgeVia)
	}
	mode := sshconfig.EffectiveMode(ent, ext.Mode)
	rows, cols := ext.Rows, ext.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	// The container's name is the namespace it carries, so a leftover is
	// recognizable in a manifest listing or a log line rather than being a session
	// with a generated name and no owner.
	name := ns.ForExtensions(sessionHost)
	return hostPlan{
		config: session.Config{
			Name:      name,
			SSHConfig: sessionHost,
			// The container has no terminal: rows and cols are the channel's, and the
			// session record merely carries the conventional defaults.
			Rows:         24,
			Cols:         80,
			Remote:       remote,
			DefaultShell: sshconfig.EffectiveDefaultShell(ent),
			// An extension is pre-authorised by whoever wrote its template; the
			// profile's review default would gate an unattended MCP server behind an
			// approval queue nothing is watching.
			Approval: false,
			// The connection outlives any one extension's process: a pipe command
			// that finishes says nothing about the transport, and the group's next
			// member may be about to open its channel on it.
			OnExit: session.OnExitKeep,
			// No channel of its own: an extension brings its own, and one opened here
			// would be a process on the target that nothing drives and nothing
			// watches.
			NoShell:     true,
			Restriction: restriction,
			Namespace:   name,
		},
		channel: ext.Name,
		command: cmd,
		args:    args,
		shell:   ext.Mode != "",
		pty:     mode == "pty",
		rows:    rows,
		cols:    cols,
	}, nil
}

// bridgeCommand renders a URL template as the argv that carries it: the built-in
// relay's name, then the endpoint and its headers. Rendering happens here rather
// than being stored because a stored argv is a second copy of the URL that can
// drift from it — an edit would have to remember to rewrite both.
//
// via names the machine the connection leaves from; empty means this one, which
// is what `target = "internal"` says (#233).
func bridgeCommand(ext *Extension, via string) (string, []string) {
	args := []string{ext.URL}
	if via != "" {
		args = append(args, "--via", via)
	}
	for _, name := range sortedKeys(ext.Headers) {
		args = append(args, "--header", name+"="+ext.Headers[name])
	}
	return mcpbridge.BuiltinCommand, args
}

// TargetStatus is one host of an extension, as the reconciler runs it. An
// extension with a single target reports the same fields at the top level, so a
// caller that never asked about targets keeps working unchanged.
type TargetStatus struct {
	Target string `json:"target"`
	Status string `json:"status"`
	// SessionID/ShellID are this host's handles. Extensions on the same host and
	// whitelist share a session, so the same session id can appear on several hosts'
	// rows of different extensions — address work by shell_id.
	SessionID string          `json:"session_id,omitempty"`
	ShellID   string          `json:"shell_id,omitempty"`
	Forwards  []ForwardStatus `json:"forwards,omitempty"`
	Failures  []Failure       `json:"failures,omitempty"`
	NextRetry int64           `json:"next_retry,omitempty"`
	// IdleTimeoutMs/IdleForMs describe this host's on-demand window.
	IdleTimeoutMs   int64  `json:"idle_timeout_ms,omitempty"`
	IdleForMs       int64  `json:"idle_for_ms,omitempty"`
	Instances       int    `json:"instances,omitempty"`
	ClosedAt        int64  `json:"closed_at,omitempty"`
	ClosedSessionID string `json:"closed_session_id,omitempty"`
	ClosedShellID   string `json:"closed_shell_id,omitempty"`
	ClosedReason    string `json:"closed_reason,omitempty"`
}

// Status reports the runtime state of one extension. ok is false for a name the
// store does not hold.
func (r *Reconciler) Status(name string) (Status, bool) {
	// ONE STAT, NOT A DIRECTORY LISTING. This used to call store.List() — a ReadDir
	// plus a Stat per entry — to answer "is this name configured", so a caller
	// asking about every extension did N×(N+1) filesystem calls. `ls ext` with 80
	// templates was 6480 of them (issue #152); the reply was correct either way, and
	// on a machine that scans each one it was slower than the whole rest of the call.
	if !r.store.Exists(name) {
		return Status{}, false
	}
	ext, err := r.store.Load(name)
	if err != nil {
		// A template that no longer parses has no targets to walk; the state the
		// reconciler recorded on it is the whole answer.
		st := r.state(name)
		return st.snapshot(), true
	}
	return r.statusOf(name, ext), true
}

// statusOf folds the per-host states of one extension into the single row its
// callers read.
//
// ONE TARGET REPORTS ITSELF. A template that names one host fills the top-level
// fields from that host, so every caller written before targets existed — the
// panel, `ls ext`, the MCP list — sees exactly what it saw then. A template that
// names several reports the fold at the top and the per-host truth in `targets`:
// a row that said "available" because one of three machines answered would be a
// lie about the other two.
func (r *Reconciler) statusOf(name string, ext *Extension) Status {
	targets := ext.EffectiveTargets()
	parts := make([]Status, 0, len(targets))
	for _, t := range targets {
		st := r.stateFor(name, t.Host)
		r.reapEnded(name, st)
		parts = append(parts, st.snapshot())
	}
	if len(parts) == 1 {
		out := parts[0]
		out.Lazy = ext.Lazy()
		return out
	}
	out := foldStatus(parts)
	out.Lazy = ext.Lazy()
	for i, t := range targets {
		row := TargetStatus{
			Target:          t.Host,
			Status:          parts[i].Status,
			SessionID:       parts[i].SessionID,
			ShellID:         parts[i].ShellID,
			Forwards:        parts[i].Forwards,
			Failures:        parts[i].Failures,
			NextRetry:       parts[i].NextRetry,
			IdleTimeoutMs:   parts[i].IdleTimeoutMs,
			IdleForMs:       parts[i].IdleForMs,
			Instances:       parts[i].Instances,
			ClosedAt:        parts[i].ClosedAt,
			ClosedSessionID: parts[i].ClosedSessionID,
			ClosedShellID:   parts[i].ClosedShellID,
			ClosedReason:    parts[i].ClosedReason,
		}
		out.Targets = append(out.Targets, row)
	}
	return out
}

// foldStatus is the one-word verdict for an extension spread over several hosts.
//
// PROBING OUTRANKS EVERYTHING: something is being tried right now, and that is the
// most useful thing to say. Otherwise "available" requires ALL of them up, "idle"
// and "disabled" require all of them so, and any mixture is "unavailable" — the
// honest answer to "is this extension usable" when half its machines are down.
func foldStatus(parts []Status) Status {
	out := Status{Status: StatusUnavailable}
	if len(parts) == 0 {
		return out
	}
	all := func(word string) bool {
		for _, p := range parts {
			if p.Status != word {
				return false
			}
		}
		return true
	}
	for _, p := range parts {
		if p.Status == StatusProbing {
			out.Status = StatusProbing
			return out
		}
	}
	switch {
	case all(StatusAvailable):
		out.Status = StatusAvailable
	case all(StatusIdle):
		out.Status = StatusIdle
	case all(StatusDisabled):
		out.Status = StatusDisabled
	}
	// The retry countdown is the soonest one: it is what the reader is waiting for.
	for _, p := range parts {
		if p.NextRetry == 0 {
			continue
		}
		if out.NextRetry == 0 || p.NextRetry < out.NextRetry {
			out.NextRetry = p.NextRetry
		}
	}
	return out
}

// TargetStatus reports the runtime state of one host of an extension. ok is false
// when the name is unknown or the host is not one this extension declares.
func (r *Reconciler) TargetStatus(name, target string) (Status, bool) {
	if !r.store.Exists(name) {
		return Status{}, false
	}
	ext, err := r.store.Load(name)
	if err != nil {
		st := r.state(name)
		return st.snapshot(), true
	}
	t, ok := ext.TargetFor(target)
	if !ok {
		return Status{}, false
	}
	st := r.stateFor(name, t.Host)
	r.reapEnded(name, st)
	return st.snapshot(), true
}

// List reports every extension's runtime state, sorted the way the store sorts
// names.
func (r *Reconciler) List() []Status {
	names, err := r.store.List()
	if err != nil {
		return nil
	}
	out := make([]Status, 0, len(names))
	for _, n := range names {
		st, ok := r.Status(n)
		if !ok {
			continue
		}
		out = append(out, st)
	}
	return out
}

// Retry forces an immediate attempt on an extension, clearing its backoff. Both
// the human and the agent reach for the same thing after fixing the underlying
// problem, and either should be served at once rather than at the next step. The
// connection the extension shares is kept: what a retry re-establishes is the
// extension's own channel and tunnels, and a session that is gone is rebuilt by
// the attempt itself.
func (r *Reconciler) Retry(name string) error {
	return r.RetryTarget(name, "")
}

// RetryTarget forces an immediate attempt on one host of an extension, clearing
// its backoff. Empty target defaults to the single declared target.
func (r *Reconciler) RetryTarget(name, target string) error {
	ext, err := r.loadTarget(name, target)
	if err != nil {
		return err
	}
	if !ext.IsEnabled() {
		return errors.New("extension is disabled; enable it before retrying")
	}
	st := r.stateFor(name, ext.Target)
	if ext.Lazy() {
		_, err := r.AcquireTarget(name, ext.Target, 0)
		return err
	}
	st.want()
	r.detach(st)
	st.mark(StatusUnavailable)
	st.resetRetryBudget()
	r.retryTargetNow(name, ext.Target)
	return nil
}

// retryNow starts one attempt for an extension's default/first target.
func (r *Reconciler) retryNow(name string) {
	r.retryTargetNow(name, "")
}

// retryTargetNow starts one attempt on one host of an extension.
func (r *Reconciler) retryTargetNow(name, target string) {
	parent, err := r.store.Load(name)
	if err != nil || !parent.IsEnabled() {
		return
	}
	host, ok := resolveTarget(parent, target)
	if !ok {
		return
	}
	view := forTarget(parent, host)
	if view.Lazy() {
		return
	}
	st := r.stateFor(name, host)
	if st.statusOf() == StatusProbing {
		return
	}
	if _, autoRetry := st.flags(); !autoRetry {
		if st.lastStage() != "target" || !r.rebuildable(&view) {
			return
		}
		st.resetRetryBudget()
		r.notifyChange()
	}
	if wait, ok := st.backoffPending(); ok && wait > 0 {
		r.armRetry(st, wait)
		return
	}
	ctx := st.runContext()
	if ctx == nil {
		return
	}
	r.startProbe(ctx, name, host)
}

// backoffPending reports how much of the current backoff window is left.
func (st *extState) backoffPending() (time.Duration, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.readyAt.IsZero() {
		return 0, false
	}
	return time.Until(st.readyAt), true
}

// runContext returns the reconciler's run context, or nil before Start.
func (r *Reconciler) runContext() context.Context {
	if p := r.runCtx.Load(); p != nil {
		return *p
	}
	return nil
}

// runContext is the run this state belongs to, for calls that start an attempt.
func (st *extState) runContext() context.Context {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.ctx
}

// name is the extension this state is filed under, needed by the timers that have
// to look the template up again when they fire.
func (st *extState) name() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.extName
}

// host is the target this state runs on, so a timer can reload the template and
// project it onto the same host it was armed for.
func (st *extState) host() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.target
}

// forTarget projects a template onto one of its hosts: the fields the whole
// extension shares, with Target and Forwards taken from that host's own entry.
//
// THIS IS THE WHOLE MULTI-TARGET MECHANISM. Everything downstream — resolve,
// fingerprint, groupKey, Lazy, ensureForwards — already worked on one host, so
// narrowing the template here leaves all of it unchanged: each target gets its own
// connection key, its own forward set (and therefore its own local ports), and its
// own resident/on-demand decision.
func forTarget(ext *Extension, target string) Extension {
	out := *ext
	out.Targets = nil
	if t, ok := ext.TargetFor(target); ok {
		out.Target = t.Host
		out.Forwards = t.Forwards
	}
	return out
}

// resolveTarget finds the host name for an ask. An empty target accepts the
// single declared host and refuses an ambiguous multi-target extension.
func resolveTarget(ext *Extension, target string) (string, bool) {
	t, ok := ext.TargetFor(target)
	if !ok {
		return "", false
	}
	return t.Host, true
}

// loadTarget reads the template and narrows it to one host.
func (r *Reconciler) loadTarget(name, target string) (*Extension, error) {
	ext, err := r.store.Load(name)
	if err != nil {
		return nil, err
	}
	host, ok := resolveTarget(ext, target)
	if !ok {
		return nil, fmt.Errorf("target %q not found in extension %q", target, name)
	}
	view := forTarget(ext, host)
	return &view, nil
}

// stateKey is the identity of one runtime state: an extension plus the host it
// runs on. A template that names one host keeps the bare name, so every existing
// state map, timer and test addresses it exactly as before.
func stateKey(name, target string) string {
	if target == "" {
		return name
	}
	return name + "@" + target
}

// state returns the state for name, creating it on first sight. An extension
// starts unavailable by design: termcp has to prove an extension works before it
// reports it as ready to use.
func (r *Reconciler) state(name string) *extState {
	return r.stateFor(name, "")
}

// stateFor returns the state for one extension on one host. An empty target
// resolves to the first host the template declares, so every pre-existing caller
// that knows only a name lands on the same state the reconciler uses.
func (r *Reconciler) stateFor(name, target string) *extState {
	if target == "" {
		if ext, err := r.store.Load(name); err == nil {
			if host, ok := resolveTarget(ext, ""); ok {
				target = host
			}
		}
	}
	key := stateKey(name, target)
	if v, ok := r.states.Load(key); ok {
		return v.(*extState)
	}
	st := &extState{status: StatusUnavailable, autoRetry: true, extName: name, target: target}
	actual, _ := r.states.LoadOrStore(key, st)
	return actual.(*extState)
}

// statesNamed returns every state filed under one extension name, in the order
// its targets were declared. One entry for a single-target template.
func (r *Reconciler) statesNamed(name string, targets []TargetDecl) []*extState {
	out := make([]*extState, 0, len(targets))
	for _, t := range targets {
		out = append(out, r.stateFor(name, t.Host))
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
