package extensions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/transcript"
	"github.com/open-mcp-ai/termcp/pkg/api"
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

	trigger chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}

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
	StatusUnavailable = "unavailable" // not up; either not yet tried or down after a failure
	StatusProbing     = "probing"     // a connection attempt is in flight
	StatusAvailable   = "available"   // session established, process alive, forwards up
)

// Failure is one recorded reason an extension was not brought up. History is
// kept rather than only the last entry because the shape of repeated failures is
// the diagnosis: always exit 127 says "wrong command", always a dial timeout
// says "wrong host".
type Failure struct {
	At     int64  `json:"at"`    // Unix ms
	Stage  string `json:"stage"` // target | dial | start | forward | session
	Reason string `json:"reason"`
}

// ForwardStatus is a declared forward as the reconciler actually established it.
// ListenAddr/TargetAddr are the bound addresses, which differ from the
// declaration whenever local_port was left at 0 ("pick one").
type ForwardStatus struct {
	Kind       string `json:"kind"`
	ForwardID  string `json:"forward_id,omitempty"`
	ListenAddr string `json:"listen_addr,omitempty"`
	TargetAddr string `json:"target_addr,omitempty"`
}

// Status is the runtime half of an extension. The template half stays in the
// Store; the two are merged for whoever asks (the MCP tool, the Web UI).
type Status struct {
	Name string `json:"name"`
	// Status is one of the four above.
	Status string `json:"status"`
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

// loopInterval is how often the reconciler re-walks the store even with no
// signal. It is the recovery path for conditions nothing notifies about — a
// profile recreated by hand, a forward that died quietly, a channel whose process
// ended without taking the connection with it — so it is a backstop, not the
// primary driver.
const loopInterval = 5 * time.Second

// probeGrace is how long a started process gets to prove it can stay alive. A
// stdio MCP server with a bad command dies within milliseconds; a healthy one
// blocks on its first read forever, so the wait costs nothing for it.
const probeGrace = 700 * time.Millisecond

// failureTailBytes is the slice of output captured as the failure reason when a
// process exits during its grace window. It is short because it is a reason, not
// a transcript: the agent that wants the whole output reads the session itself.
const failureTailBytes = 512

// extGroup is the connection one or more extensions share: the singleton. It is
// keyed by what has to agree for two extensions to be served by the same session
// — the profile they dial and the capability set they were granted — and it holds
// nothing but that session until the last of its members goes away.
type extGroup struct {
	// mu serializes the group's session against the probes of its members: two
	// extensions coming up at once must open one connection, not two.
	mu        sync.Mutex
	key       string
	target    string
	namespace string
	// sessionID is the session the group is served by. A DEAD one stays named
	// here until the next attempt replaces it, so the corpse can be finalized
	// rather than left in the registry; "does this group have a session" is a
	// liveness question, which is what liveSession answers.
	sessionID string
}

// hostPlan is one template resolved into what it actually needs: the connection
// its group dials, and the channel this extension runs on that connection.
type hostPlan struct {
	// config builds the group's container — a connection with no channel of its
	// own, since every extension brings its own.
	config  session.Config
	channel string // the channel's name, so a shared session says which extension a shell belongs to
	command string
	args    []string
	shell   bool // the template declares a shell (false = SFTP/forwards only)
	pty     bool
	rows    int
	cols    int
}

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
		trigger:    make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if store != nil {
		store.SetOnChange(r.Trigger)
	}
	if sshConfigs != nil {
		// A profile written by hand is the fix a template failure was waiting on,
		// so the walk hears about it when it happens rather than at the next loop
		// tick.
		sshConfigs.SetOnChange(r.Trigger)
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

// Trigger asks the reconciler to re-walk the store as soon as it can. It never
// blocks: the channel has room for one signal, and a second call while one is
// pending is the same walk anyway.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *Reconciler) run(ctx context.Context) {
	defer close(r.done)
	for {
		if err := r.reconcileAll(ctx); err != nil {
			slog.Warn("extension reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
		case <-time.After(loopInterval):
		}
	}
}

// member is one enabled extension the walk is about to act on, with its template
// already loaded.
type member struct {
	name string
	ext  *Extension
}

// reconcileAll walks the store once and leaves the runtime in the shape it
// describes: anything absent from the store loses its state, anything parked is
// detached and left alone, the groups nothing resolves to any more are closed,
// and every remaining member is pushed toward available.
func (r *Reconciler) reconcileAll(ctx context.Context) error {
	names, err := r.store.List()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	live := map[string]bool{} // group keys something still resolves to
	members := make([]member, 0, len(names))

	for _, name := range names {
		seen[name] = true
		ext, err := r.store.Load(name)
		if err != nil {
			// A file that no longer validates is reported through its own status
			// rather than dropped: silently ignoring a broken extension would let
			// it stay "unavailable forever, reason unknown".
			st := r.state(name)
			st.leaveGroup()
			st.mark(StatusUnavailable)
			st.recordFailure("target", "extension config is no longer valid: "+err.Error())
			r.notifyChange()
			continue
		}
		st := r.state(name)
		if !ext.IsEnabled() {
			// A parked extension holds nothing: its channel and its tunnels are
			// closed, and the connection it was sharing is left to the extensions
			// that are still enabled. Its retry budget is handed back too, so
			// enabling it again is an immediate attempt rather than a resumed wait.
			had := st.leaveGroup()
			if had || st.statusOf() != StatusDisabled {
				r.detach(st)
				st.mark(StatusDisabled)
				st.resetRetryBudget()
				r.notifyChange()
			}
			continue
		}
		key := groupKey(ext)
		// Both readings happen unconditionally, and the result is combined after:
		// setGroup answers "did the connection move" and changed answers "is this a
		// different template", and they are independent. Chaining them with || was
		// the bug — a template that moved groups short-circuited the fingerprint
		// update, so the next walk saw the stale fingerprint, called it a change,
		// and tore down the channel and tunnels it had just built.
		moved := st.setGroup(key)
		differed := st.changed(ext)
		if moved || differed {
			// The template changed: it now dials another connection (or asks for
			// another capability set), or what it describes is different. Either way
			// the extension is rebuilt from it — its channel and its tunnels are
			// closed, and the walk below probes it again — while the group's
			// connection is left alone: the session belongs to every extension
			// sharing the host, and rebuilding it would redial for all of them. An
			// edit is also the author's nudge ("change the command and see if it
			// works now"), so the automatic retries come with it.
			r.detach(st)
			st.mark(StatusUnavailable)
			st.resetRetryBudget()
			r.notifyChange()
		}
		live[key] = true
		r.group(key, ext)
		members = append(members, member{name: name, ext: ext})
	}

	// Extensions the store no longer holds lose their state, and with it the status
	// the panel was reading. This runs before the groups are swept: an extension
	// that is gone detaches here, so closing the connection it was last attached to
	// does not read as its session dying.
	r.mu.Lock()
	var gone []string
	for name := range r.states {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	r.mu.Unlock()
	for _, name := range gone {
		st := r.state(name)
		st.leaveGroup()
		r.detach(st)
		r.mu.Lock()
		delete(r.states, name)
		r.mu.Unlock()
		r.notifyChange()
		slog.Debug("extension removed", "name", name)
	}

	// A connection exists for the extensions it serves, so a group nothing
	// resolves to any more is closed: parked, deleted, or moved to another host.
	for _, g := range r.groupList() {
		if live[g.key] {
			continue
		}
		r.closeGroup(g)
		r.mu.Lock()
		delete(r.groups, g.key)
		r.mu.Unlock()
		r.notifyChange()
		slog.Debug("extension session closed", "group", g.key, "target", g.target, "namespace", g.namespace)
	}

	// Finally, the members that are not up. Reads of the store and of state are
	// kept separate locks, so a slow dial in one probe goroutine never blocks this
	// walk.
	for _, m := range members {
		st := r.state(m.name)
		status, autoRetry := st.flags()
		switch status {
		case StatusProbing:
			continue // an attempt is already in flight
		case StatusAvailable:
			sess := r.attachedSession(st)
			if sess == nil {
				// The connection is gone. Forget the handle before recording the
				// failure, or the state keeps advertising a channel that cannot be
				// driven.
				r.detach(st)
				r.fail(st, "session", "session is no longer running")
				continue
			}
			if ch := liveShell(sess, st.shellOf()); m.ext.Mode != "" && ch == nil {
				// The channel the extension runs on ended by itself: its process is
				// gone while the connection it shares is not, so the retry opens a
				// channel instead of redialing the host. Read the exit line before
				// detach closes the channel, then drop the handle so the recorded
				// failure does not carry a dead one.
				reason := r.channelEndReason(sess, st)
				r.detach(st)
				r.fail(st, "session", reason)
				continue
			}
			if err := r.ensureForwards(m.ext, st); err != nil {
				r.fail(st, "forward", err.Error())
				continue
			}
			continue // available and fully forwarded: nothing to do this walk
		default:
			if !autoRetry {
				/* The budget is spent. One condition is still worth re-checking
				   without a nudge: the target profile itself. Recreated by hand, it is
				   a fix rather than a transient, and reading it costs one store lookup
				   instead of a dial. A host whose profile resolves fine but is not
				   answering keeps waiting for the panel key or the agent — that is the
				   case the budget exists to stop. */
				if st.lastStage() != "target" || !r.rebuildable(m.ext) {
					continue
				}
				st.resetRetryBudget()
				r.notifyChange()
			}
			if st.readyAtLocked().After(time.Now()) {
				continue // backoff window not elapsed
			}
		}
		r.startProbe(ctx, m.name)
	}
	return nil
}

// startProbe marks the extension probing and runs the attempt in its own
// goroutine, because a dial to an unreachable host blocks for its timeout and
// must not delay the other extensions or the walk.
func (r *Reconciler) startProbe(ctx context.Context, name string) {
	st := r.state(name)
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
		r.probe(ctx, name)
		r.Trigger()
	}()
}

// probe is one attempt to bring an extension to available. The connection is the
// group's — reused when it is up, created when it is not — and everything else
// belongs to this extension: its channel, its tunnels, its failure history.
func (r *Reconciler) probe(ctx context.Context, name string) {
	ext, err := r.store.Load(name)
	if err != nil || !ext.IsEnabled() {
		// Removed or parked between the walk and the attempt: the next walk drops
		// the state (removed) or leaves it alone (parked).
		return
	}
	st := r.state(name)
	g := r.groupFor(st)
	if g == nil {
		return
	}

	plan, err := r.resolve(ext)
	if err != nil {
		// The template does not resolve to a connection at all — a missing profile,
		// a bad path rule. No retry can clear this: the ladder exists for conditions
		// a redial fixes, and this one is fixed only by editing the template. Its
		// recovery path is the walk's rebuildable check, which costs a store
		// lookup and hands the budget back the moment the template resolves again,
		// so the budget is spent now rather than climbed over twenty seconds —
		// and the missing profile stops re-probing every walk in the meantime.
		r.failForGood(st, "target", err.Error())
		return
	}

	sess, err := r.ensureGroupSession(ctx, g, plan.config)
	if err != nil {
		r.fail(st, "dial", sshclient.DescribeDialError(err))
		return
	}

	shellID, err := r.ensureMemberShell(plan, st, sess)
	if err != nil {
		// The connection is up and shared; only this extension's process failed to
		// start. The session stays — its other members are using it, and this
		// extension's own retry is then a channel open rather than a redial.
		r.fail(st, "start", err.Error())
		return
	}
	st.attach(sess.ID, shellID)

	if err := r.ensureForwards(ext, st); err != nil {
		// The session is up and usable for its own sake; only the tunnel is
		// missing, so the failure is recorded and the retry targets the forward
		// alone rather than tearing the connection down to rebuild it.
		r.fail(st, "forward", err.Error())
		return
	}

	// Proven up: the budget is whole again, so a later failure starts the ladder
	// over rather than resuming a count from before the extension was working.
	st.resetRetryBudget()
	st.mark(StatusAvailable)
	slog.Debug("extension available", "name", name, "session_id", sess.ID, "shell_id", shellID)
	r.notifyChange()
}

// fail records a reason, marks the extension unavailable, and arms the backoff
// ladder. It is the shape of every failure a redial or a reopened channel might
// clear — a dial that timed out, a process that exited, a port that was taken —
// so the four things that must happen together happen in one place.
func (r *Reconciler) fail(st *extState, stage, reason string) {
	st.recordFailure(stage, reason)
	st.mark(StatusUnavailable)
	st.scheduleBackoff()
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
	name := ns.ForExtensions(ext.Target)
	return hostPlan{
		config: session.Config{
			Name:      name,
			SSHConfig: ext.Target,
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

// groupKey is the identity of the connection an extension is served by: the host
// it dials, and the capability set it was granted. Extensions that agree on both
// are served by one session — the second one adds a channel to a connection that
// already exists rather than dialing the host again.
//
// The capability set is part of the identity rather than merely a property of the
// session because the two travel together: a session enforces one whitelist, so
// two extensions with different whitelists sharing a session would each be able
// to do what the other was allowed. Sharing is therefore exactly coincident with
// "the same calls are permitted", and no extension can reach past its own
// template — which is the whole promise of the layer.
func groupKey(ext *Extension) string {
	return strings.TrimSpace(ext.Target) + "\x00" + restrictionKey(ext.Allow, ext.Paths)
}

// restrictionKey renders a whitelist pair as a comparable string. Entries are
// sorted and trimmed first: two templates that list the same tools in a different
// order, or with stray whitespace, describe the same capability set and must land
// on the same connection.
func restrictionKey(allow, paths []string) string {
	var b strings.Builder
	writeSet(&b, allow)
	b.WriteByte('\x01')
	writeSet(&b, paths)
	return b.String()
}

// sortedKeys returns a map's keys in order, so a rendering that walks them is
// deterministic (two runs of one template must produce one argv, and a
// fingerprint over it must not depend on map iteration order).
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeSet(b *strings.Builder, list []string) {
	sorted := make([]string, 0, len(list))
	for _, s := range list {
		sorted = append(sorted, strings.TrimSpace(s))
	}
	sort.Strings(sorted)
	for _, s := range sorted {
		b.WriteString(s)
		b.WriteByte('\x00')
	}
}

// group returns the group a key names, creating it on first sight. Its target and
// namespace come from the extension that created it, which the key already fixes:
// members of a group dial the same profile under the same namespace.
func (r *Reconciler) group(key string, ext *Extension) *extGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.groups[key]; ok {
		return g
	}
	g := &extGroup{key: key, target: ext.Target, namespace: ns.ForExtensions(ext.Target)}
	r.groups[key] = g
	return g
}

// groupFor returns the group an extension's state names, or nil when it has none
// (a parked extension, or one whose template no longer loads).
func (r *Reconciler) groupFor(st *extState) *extGroup {
	key := st.groupOf()
	if key == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.groups[key]
}

func (r *Reconciler) groupList() []*extGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*extGroup, 0, len(r.groups))
	for _, g := range r.groups {
		out = append(out, g)
	}
	return out
}

// ensureGroupSession returns the connection the group is served by: the one it
// already has when that is still running, a new one otherwise. The group's own
// lock is what makes two members starting at once share the answer.
func (r *Reconciler) ensureGroupSession(ctx context.Context, g *extGroup, base session.Config) (*session.Session, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sess := r.liveSession(g.sessionID); sess != nil {
		return sess, nil
	}
	// The previous session is DEAD and has served its purpose — it held the
	// failure's output for anyone watching — and the group is about to be served
	// by a new one. Finalizing it here keeps a failing group from leaving one dead
	// connection per attempt behind.
	if g.sessionID != "" {
		_ = r.sessMgr.Delete(g.sessionID)
		g.sessionID = ""
	}
	cfg := base
	cfg.Ctx = ctx
	sess, err := r.sessMgr.Create(cfg)
	if err != nil {
		return nil, err
	}
	g.sessionID = sess.ID
	slog.Debug("extension session up", "group", g.key, "target", g.target, "session_id", sess.ID)
	return sess, nil
}

// liveSession returns a session that is still running. DEAD sessions stay in the
// registry (their output stays readable), so presence is not liveness.
func (r *Reconciler) liveSession(id string) *session.Session {
	if id == "" || r.sessMgr == nil {
		return nil
	}
	sess := r.sessMgr.Get(id)
	if sess == nil || sess.Info().Status != api.SessionRunning {
		return nil
	}
	return sess
}

// attachedSession returns the running session this extension is attached to, or
// nil when the connection it was served by is gone or it is not attached yet.
func (r *Reconciler) attachedSession(st *extState) *session.Session {
	return r.liveSession(st.sessionOf())
}

// ensureMemberShell returns the extension's own channel in the session, opening
// one when it has none. A template that declares no shell (a read-only file
// window) is served by the connection alone and reports no channel; one whose
// channel is still running keeps it, so a retry after a tunnel failure does not
// restart the process.
func (r *Reconciler) ensureMemberShell(plan hostPlan, st *extState, sess *session.Session) (string, error) {
	if !plan.shell {
		return "", nil
	}
	if shell := liveShell(sess, st.shellOf()); shell != nil {
		return shell.ID, nil
	}
	shell, err := sess.CreateChildShell(plan.command, plan.args, plan.pty, plan.rows, plan.cols, plan.channel)
	if err != nil {
		return "", err
	}

	// A process that cannot start says so immediately. Waiting a beat turns "the
	// extension is unavailable" from a bare fact into the actual error line,
	// which is what makes the failure history worth reading.
	select {
	case <-shell.Done():
		return "", r.memberExit(sess, shell, "process exited during startup")
	case <-time.After(probeGrace):
	}
	// The window can close in the same instant the process ends, and a slow starter
	// that dies on its own schedule would otherwise be reported as up for one walk
	// and as a mystery afterwards. One more look is what keeps "started" honest.
	if liveShell(sess, shell.ID) == nil {
		return "", r.memberExit(sess, shell, "process exited during startup")
	}
	return shell.ID, nil
}

// memberExit closes a channel whose process ended before the extension could use
// it and returns the failure to record: what the process printed, which is the
// part that tells an author what to fix. The channel is closed rather than left
// behind because a failing extension retries, and one dead tab per attempt is how
// a shared session fills up with corpses.
func (r *Reconciler) memberExit(sess *session.Session, shell *session.ChildShell, fallback string) error {
	reason := exitReason(r.msgMgr, r.sessMgr, shell)
	_ = sess.CloseChildShell(shell.ID)
	if reason == "" {
		return errors.New(fallback)
	}
	return errors.New(reason)
}

// channelEndReason describes a channel that ended after the extension was up. The
// exit line is read from the channel itself — a shell that has exited keeps its
// transcript for exactly this kind of read — so a process that dies later is
// reported as informatively as one that dies during startup.
func (r *Reconciler) channelEndReason(sess *session.Session, st *extState) string {
	if sess != nil {
		if shell := sess.GetChildShell(st.shellOf()); shell != nil {
			if reason := exitReason(r.msgMgr, r.sessMgr, shell); reason != "" {
				return reason
			}
		}
	}
	return "the extension's process is no longer running"
}

// liveShell returns the extension's channel when it is still a running channel: a
// shell id that no longer resolves in this session, or one whose process has
// ended, is not a handle the extension can be driven through.
func liveShell(sess *session.Session, shellID string) *session.ChildShell {
	if sess == nil || shellID == "" {
		return nil
	}
	shell := sess.GetChildShell(shellID)
	if shell == nil || shell.Info().Status != api.SessionRunning {
		return nil
	}
	return shell
}

// detach closes what this extension holds on its group's session — its own
// channel and its tunnels — and forgets the handles. The session itself is left
// alone: it belongs to the group, another member may be using it, and one nobody
// is using is closed by the walk when the group loses its last member.
func (r *Reconciler) detach(st *extState) {
	sid, shellID, forwards := st.takeAttachment()
	if shellID != "" {
		if sess := r.sessMgr.Get(sid); sess != nil {
			_ = sess.CloseChildShell(shellID)
		}
	}
	r.closeForwards(forwards)
}

// closeForwards releases tunnels that a detach or a teardown is taking away.
func (r *Reconciler) closeForwards(forwards []ForwardStatus) {
	if r.forwardMgr == nil {
		return
	}
	for _, fw := range forwards {
		if fw.ForwardID != "" {
			r.forwardMgr.Close(fw.ForwardID)
		}
	}
}

// closeGroup closes the connection a group keeps and forgets it: the teardown for
// "nothing is served by this any more". The session is finalized rather than left
// DEAD — it is infrastructure, not a transcript anyone asked for, and a hidden
// dead session only accumulates on disk while the next member dials a fresh one.
func (r *Reconciler) closeGroup(g *extGroup) {
	g.mu.Lock()
	sid := g.sessionID
	g.sessionID = ""
	g.mu.Unlock()
	if sid == "" || r.sessMgr == nil {
		return
	}
	r.sessMgr.Terminate(sid, true, 0)
	_ = r.sessMgr.Delete(sid)
}

// onSessionDead is the DEAD subscription. A session that dies on its own — the
// transport dropped, the far end went away — takes every extension it was serving
// with it, and the reason is recorded so each status explains the drop instead of
// merely showing it.
func (r *Reconciler) onSessionDead(sessionID string) {
	served := false
	r.mu.Lock()
	states := make([]*extState, 0, len(r.states))
	for _, st := range r.states {
		states = append(states, st)
	}
	r.mu.Unlock()

	reason := "session ended"
	if sess := r.sessMgr.Get(sessionID); sess != nil {
		if info := sess.Info(); info.ExitCode != nil {
			reason = fmt.Sprintf("process exited with code %d", *info.ExitCode)
		}
	}
	for _, st := range states {
		if !st.detachFrom(sessionID) {
			// Not one of this session's extensions: the hook fans out to every
			// state, and only the ones attached have anything to say about it.
			continue
		}
		served = true
		r.fail(st, "session", reason)
	}
	if served {
		r.Trigger()
	}
	// The group keeps the dead session's id rather than clearing it: the next
	// attempt replaces the corpse with a live connection, and resolving "does this
	// group have a session" is a liveness question everywhere else.
}

// Status reports the runtime state of one extension. ok is false for a name the
// store does not hold.
func (r *Reconciler) Status(name string) (Status, bool) {
	names, err := r.store.List()
	if err != nil {
		return Status{}, false
	}
	for _, n := range names {
		if n == name {
			return r.state(name).snapshot(), true
		}
	}
	return Status{}, false
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
	st := r.state(name)
	r.detach(st)
	st.mark(StatusUnavailable)
	st.resetRetryBudget()
	r.Trigger()
	return nil
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
	st := &extState{status: StatusUnavailable, autoRetry: true}
	r.states[name] = st
	return st
}

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
	fmt.Fprintf(&b, "%s|%s|%s|%q|", ext.Kind, ext.Target, ext.Mode, ext.Command)
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

func (st *extState) sessionOf() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sessionID
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

func (st *extState) flags() (status string, autoRetry bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status, st.autoRetry
}

// readyAtLocked reports the instant the backoff window elapses. It is the third
// read the walk makes, and it must be taken under the same lock as the others:
// resetRetryBudget clears readyAt to the zero time from the retry path (an MCP
// call or the panel's key) while the walk is reading it, and the zero time is
// exactly the value the walk reads as "no window, probe now" — so an unsynchronised
// read is not only a race the detector sees but a decision made on a torn view.
func (st *extState) readyAtLocked() time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.readyAt
}

// scheduleBackoff sets the earliest retry from the consecutive-failure count.
// Once the budget is spent it schedules nothing and clears the flag, so the walk
// stops bringing the extension up on its own; the timer, not a counter, is what
// would otherwise keep it probing forever.
func (st *extState) scheduleBackoff() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.consecutive > maxAutoRetries {
		st.autoRetry = false
		st.readyAt = time.Time{}
		return
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

// ensureForwards establishes the forwards the template declares, skipping the
// ones still listening. Returning an error leaves the session standing: a tunnel
// is a capability of the extension, not its reason to exist.
func (r *Reconciler) ensureForwards(ext *Extension, st *extState) error {
	if r.forwardMgr == nil || len(ext.Forwards) == 0 {
		return nil
	}
	st.mu.Lock()
	if st.sessionID == "" {
		st.mu.Unlock()
		return errors.New("no session to attach forwards to")
	}
	sessID := st.sessionID
	existing := append([]ForwardStatus(nil), st.forwards...)
	st.mu.Unlock()

	live := map[string]bool{}
	for _, fw := range r.forwardMgr.ListBySession(sessID) {
		live[fw.ForwardID] = true
	}

	sess := r.sessMgr.Get(sessID)
	if sess == nil {
		return errors.New("session for forwards is gone")
	}
	info := sess.Info()
	sshCli := sess.SSHClient()

	built := make([]ForwardStatus, len(ext.Forwards))
	// A forward's position in the declaration is what makes it the same forward
	// across walks, so the slice is laid out by index and a missing entry is an empty
	// one. Keeping what already worked when a later entry fails is what makes the
	// retry target only the one that did not: recording nothing until the whole list
	// succeeded left the earlier listeners live but unrecorded, so the next attempt
	// rebuilt them onto a port it already held and the extension could never come
	// back up.
	for i, f := range ext.Forwards {
		if i < len(existing) && live[existing[i].ForwardID] {
			built[i] = existing[i]
			continue
		}
		fw, err := r.createForward(f, sess, sshCli, info)
		if err != nil {
			// Record what this attempt did establish before reporting the failure:
			// the walk reads these entries to decide what is already listening.
			st.mu.Lock()
			st.forwards = append([]ForwardStatus(nil), built...)
			st.mu.Unlock()
			return fmt.Errorf("%s forward on %s: %w", f.Kind, listenSpec(f), err)
		}
		// Tear down with the session rather than outliving it as a dangling
		// listener, the same way the forward tool attaches its own.
		sess.AttachCleanup(func() { r.forwardMgr.Close(fw.ForwardID) })
		built[i] = ForwardStatus{
			Kind:       f.Kind,
			ForwardID:  fw.ForwardID,
			ListenAddr: fw.ListenAddr,
			TargetAddr: fw.TargetAddr,
		}
	}

	st.mu.Lock()
	st.forwards = built
	st.mu.Unlock()
	return nil
}

func (r *Reconciler) createForward(f ForwardDecl, sess *session.Session, sshCli *ssh.Client, info api.Session) (*forward.ForwardInfo, error) {
	switch f.Kind {
	case ForwardDynamic:
		return r.forwardMgr.CreateDynamic(sess.ID, info.Name, f.BindHost, f.LocalPort, sshCli, info.SSHEndpoint == "internal")
	case ForwardLocal:
		return r.forwardMgr.CreateLocal(sess.ID, info.Name, f.BindHost, f.RemoteHost, f.RemotePort, f.LocalPort, sshCli)
	case ForwardRemote:
		return r.forwardMgr.CreateRemote(sess.ID, info.Name, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort, sshCli)
	}
	return nil, fmt.Errorf("unknown forward kind %q", f.Kind)
}

// exitReason reads the last bytes a dying process produced, so a startup failure
// carries the error line the process printed instead of only its exit code.
func exitReason(msgMgr *message.Manager, sessMgr *session.Manager, shell *session.ChildShell) string {
	if msgMgr == nil || sessMgr == nil || shell == nil {
		return ""
	}
	src, err := transcript.Resolve(sessMgr, msgMgr, shell.ID)
	if err != nil {
		return ""
	}
	w, err := transcript.Read(context.Background(), src, transcript.Params{
		TailBytes: true,
		MaxBytes:  failureTailBytes,
		MaxLines:  12,
		StripAnsi: true,
	})
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(w.Output)
	info := shell.Info()
	if info.ExitCode == nil {
		return text
	}
	if text == "" {
		return fmt.Sprintf("exit code %d", *info.ExitCode)
	}
	return fmt.Sprintf("exit code %d: %s", *info.ExitCode, text)
}

// listenSpec renders a forward's declaration for an error message.
func listenSpec(f ForwardDecl) string {
	if f.LocalPort > 0 {
		return fmt.Sprintf("%s:%d", f.LocalHost, f.LocalPort)
	}
	return "ephemeral port"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
