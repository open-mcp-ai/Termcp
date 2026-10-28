package extensions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
	mu       sync.Mutex
	states   map[string]*extState
	groups   map[string]*extGroup
	onChange func()

	// syncs carries the names of templates whose saved file changed, and revive
	// carries "a profile was rewritten". They are queues, not signals to a walk: the
	// callback that fills them runs inside a store's write, so it must only drop the
	// name in and get out — doing the work there would deadlock on the store's own
	// lock, which is exactly what a synchronous subscriber did.
	syncs  chan string
	revive chan struct{}
	// ctx is the run's context, kept so an event that arrives at any time — a save
	// from a tool call, a deleted file — has a run to join.
	ctx    context.Context
	cancel context.CancelFunc
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
		states:     map[string]*extState{},
		groups:     map[string]*extGroup{},
		syncs:      make(chan string, 64),
		revive:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if store != nil {
		store.SetSSHConfigStore(sshConfigs)
		// The write names the template, so the runtime does that one and nothing
		// else. This is the whole of "the store changed" — there is no listing to
		// diff and no tick to wait for.
		store.SetOnChange(r.syncNamed)
	}
	if sshConfigs != nil {
		// A profile rewritten by hand is the fix a failing extension was waiting for.
		// It has no name to give (it is not an extension), so the extensions that gave
		// up on a missing target get one more attempt.
		sshConfigs.SetOnChange(r.ProfilesChanged)
	}
	if sessMgr != nil {
		sessMgr.AddOnDeadHook(r.onSessionDead)
	}
	return r
}

func (r *Reconciler) ProfilesChanged() {
	select {
	case r.revive <- struct{}{}:
	default:
	}
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
	r.mu.Lock()
	r.onChange = fn
	r.mu.Unlock()
}

func (r *Reconciler) notifyChange() {
	r.mu.Lock()
	fn := r.onChange
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Start loads every extension and begins maintaining them. Returning an error
// only for a store that cannot be walked: one broken extension is reported in
// its own status, not as a failure to start the reconciler.
func (r *Reconciler) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.mu.Unlock()
	if _, err := r.store.List(); err != nil {
		r.mu.Lock()
		r.cancel = nil
		r.mu.Unlock()
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
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-r.done
	r.probes.Wait()
}

// probe is one attempt to bring an extension to available. The connection is the
// group's — reused when it is up, created when it is not — and everything else
// belongs to this extension: its channel, its tunnels, its failure history.
func (r *Reconciler) probe(ctx context.Context, name string) {
	ext, err := r.store.Load(name)
	if err != nil || !ext.IsEnabled() {
		// Removed or parked between the scheduling and the attempt.
		return
	}
	st := r.state(name)
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
	target, identity, err := r.sshConfigs.ResolveReference(ext.Target, ext.TargetIdentity)
	if err != nil {
		if errors.Is(err, sshconfig.ErrNotFound) {
			return hostPlan{}, fmt.Errorf("target profile %q does not exist", ext.Target)
		}
		return hostPlan{}, fmt.Errorf("cannot resolve target profile %q: %w", ext.Target, err)
	}
	ext.Target, ext.TargetIdentity = target, identity
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
	if ext.URL != "" {
		// The endpoint form runs the in-process relay instead of a program: the
		// template names a server, and what executes on the channel is termcp's own
		// bridge (see internal/mcpbridge).
		//
		// That relay is termcp's own process, so the channel has to be the in-memory
		// one: on a remote target this command would be looked up on the far host,
		// where it does not exist. Refusing here rather than at dial is what turns
		// "command not found" into a sentence about the template.
		if ent.Kind != sshconfig.KindInternal {
			return hostPlan{}, fmt.Errorf("url requires target = \"internal\": the bridge runs inside termcp, so the endpoint is reached from this host (not from %q)", ext.Target)
		}
		cmd, args = bridgeCommand(ext)
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
	name := ns.ForExtensions(namespaceTarget(ext))
	return hostPlan{
		config: session.Config{
			Name:              name,
			SSHConfig:         ext.Target,
			SSHConfigIdentity: ext.TargetIdentity,
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
func bridgeCommand(ext *Extension) (string, []string) {
	args := []string{ext.URL}
	for _, name := range sortedKeys(ext.Headers) {
		args = append(args, "--header", name+"="+ext.Headers[name])
	}
	return mcpbridge.BuiltinCommand, args
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
	st := r.state(name)
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
		out = append(out, r.state(n).snapshot())
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
	ext, err := r.store.Load(name)
	if err != nil {
		return err
	}
	if !ext.IsEnabled() {
		return errors.New("extension is disabled; enable it before retrying")
	}
	if ext.Lazy() {
		// ON DEMAND, "RETRY" MEANS "GIVE ME AN INSTANCE NOW". There is no channel for
		// the walk to repair — a lazy extension holds nothing until a caller asks —
		// so the retry is the ask, and it answers with the same failure the first
		// attempt did when the template is what is wrong (issue #183).
		_, err := r.Acquire(name, 0)
		return err
	}
	st := r.state(name)
	// A manual retry is an ask: for a resident extension it is what starts it again
	// after a failure the ladder gave up on. Nothing is woken here — the attempt is
	// started directly, which is what the walk used to do a tick later.
	st.want()
	r.detach(st)
	st.mark(StatusUnavailable)
	st.resetRetryBudget()
	r.retryNow(name)
	return nil
}

// retryNow starts one attempt for an extension the reconciler owns. It is what the
// retry timer, the panel's key and the MCP call all funnel through, so there is one
// place that decides whether an attempt is allowed to start.
func (r *Reconciler) retryNow(name string) {
	ext, err := r.store.Load(name)
	if err != nil || !ext.IsEnabled() {
		return
	}
	if ext.Lazy() {
		// On demand, an attempt is a caller's ask, and nobody asked here.
		return
	}
	st := r.state(name)
	if st.statusOf() == StatusProbing {
		return
	}
	if _, autoRetry := st.flags(); !autoRetry {
		/* The budget is spent. One condition is still worth re-checking without a
		   nudge: the target profile itself. Recreated by hand, it is a fix rather
		   than a transient, and reading it costs one store lookup instead of a dial.
		   A host whose profile resolves fine but is not answering keeps waiting for
		   the panel's key or the agent — that is the case the budget exists to stop. */
		if st.lastStage() != "target" || !r.rebuildable(ext) {
			return
		}
		st.resetRetryBudget()
		r.notifyChange()
	}
	if wait, ok := st.backoffPending(); ok && wait > 0 {
		// Still inside the window the failure printed. Re-arm rather than ignore, so
		// the deadline is kept even if this call arrived early.
		r.armRetry(st, wait)
		return
	}
	ctx := st.runContext()
	if ctx == nil {
		return
	}
	r.startProbe(ctx, name)
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

// runCtx is the reconciler's run context, or nil before Start.
func (r *Reconciler) runCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctx
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

// state returns the state for name, creating it on first sight. An extension
// starts unavailable by design: termcp has to prove an extension works before it
// reports it as ready to use.
func (r *Reconciler) state(name string) *extState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[name]; ok {
		return st
	}
	st := &extState{status: StatusUnavailable, autoRetry: true, extName: name}
	r.states[name] = st
	return st
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
