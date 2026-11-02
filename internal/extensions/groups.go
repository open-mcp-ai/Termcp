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

	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// extGroup is the connection one or more extensions share: the singleton. It is
// keyed by what has to agree for two extensions to be served by the same session
// — the profile they dial and the capability set they were granted — and it holds
// nothing but that session until the last of its members goes away.
type extGroup struct {
	// mu guards the fields below, and NOTHING ELSE. It used to be held across
	// sessMgr.Create — a full SSH dial and handshake — so one member rebuilding the
	// connection stalled every other operation on the same group for as long as the
	// network took (issue #222). A lock around a network call is the same bug as
	// #218 through #221 in a different place.
	//
	// "Two extensions coming up at once must open ONE connection" is still the rule;
	// it is now expressed by dialing, which a caller can leave.
	mu        sync.Mutex
	key       string
	target    string
	namespace string
	// dialing is the connection currently being established, if any. Callers that
	// find it set wait for it instead of starting a second dial, and wait somewhere
	// their own context can release them.
	dialing *groupDial
	// created is when this group was first resolved. The sweep uses it to leave
	// alone a connection that appeared while the walk was running: the walk's
	// picture of who wants what was taken at the top, and a group that did not exist
	// then exists precisely because somebody asked for it in the meantime.
	created time.Time
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

// groupNamed reports whether any state still resolves to this connection.
func (r *Reconciler) groupNamed(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.states {
		if st.groupOf() == key {
			return true
		}
	}
	return false
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
	g := &extGroup{key: key, target: ext.Target, namespace: ns.ForExtensions(ext.Target), created: time.Now()}
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
// groupDial is one in-flight connection attempt. Its done channel is closed when
// the attempt finishes, and the outcome is read from sess/err afterwards.
type groupDial struct {
	done chan struct{}
	sess *session.Session
	err  error
}

func (r *Reconciler) ensureGroupSession(ctx context.Context, g *extGroup, base session.Config) (*session.Session, error) {
	// FAST PATH AND SINGLE-FLIGHT DECISION UNDER THE LOCK, DIAL OUTSIDE IT.
	//
	// The lock now protects only the bookkeeping: is there a live session, and is
	// somebody already dialing. Both are answered in a few instructions, so the lock
	// is never held across anything that can block.
	g.mu.Lock()
	if sess := r.liveSession(g.sessionID); sess != nil {
		g.mu.Unlock()
		return sess, nil
	}
	if d := g.dialing; d != nil {
		// Somebody else is already opening this group's connection. Wait for THEIR
		// result, but wait somewhere this caller's context can end the wait — which
		// is the whole reason this is a channel and not the mutex.
		g.mu.Unlock()
		select {
		case <-d.done:
			return d.sess, d.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d := &groupDial{done: make(chan struct{})}
	g.dialing = d
	deadID := g.sessionID
	g.sessionID = ""
	g.mu.Unlock()

	// THE SLOW HALF, UNLOCKED. Finalizing the dead connection and dialing a new one
	// are both network operations, and neither may hold the group's lock.
	if deadID != "" {
		// The previous session is DEAD and has served its purpose — it held the
		// failure's output for anyone watching — and the group is about to be served
		// by a new one. Finalizing it here keeps a failing group from leaving one dead
		// connection per attempt behind.
		_ = r.sessMgr.Delete(deadID)
	}
	cfg := base
	cfg.Ctx = ctx
	sess, err := r.sessMgr.Create(cfg)

	g.mu.Lock()
	g.dialing = nil
	if err == nil {
		g.sessionID = sess.ID
	}
	g.mu.Unlock()

	d.sess, d.err = sess, err
	close(d.done) // broadcast: every waiter re-reads the outcome

	if err != nil {
		return nil, err
	}
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

	if err := r.waitForStart(sess, shell); err != nil {
		return "", err
	}
	return shell.ID, nil
}

// waitForStart is the grace every channel gets, whoever forked it: the reconciler's
// own path and a caller's on-demand ask share one implementation, because they are
// asking the same question and a second copy is a second answer.
//
// A process that cannot start says so immediately, and waiting a beat turns "the
// extension is unavailable" from a bare fact into the actual error line. The window
// can also close in the same instant the process ends, and a slow starter that dies
// on its own schedule would otherwise be reported as up for one pass and as a
// mystery afterwards — one more look is what keeps "started" honest.
func (r *Reconciler) waitForStart(sess *session.Session, shell *session.ChildShell) error {
	select {
	case <-shell.Done():
		return r.memberExit(sess, shell, "process exited during startup")
	case <-time.After(probeGrace):
	}
	if liveShell(sess, shell.ID) == nil {
		return r.memberExit(sess, shell, "process exited during startup")
	}
	return nil
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
		// THE INSTANCES GO WITH THE CONNECTION, AND THEY SAY SO. They are channels
		// in it, so a handle to one of them is a handle to nothing — and an
		// on-demand extension has no attachment of its own for detachFrom to find,
		// so dropping the instances is the ONLY news this hook has for it. Leaving
		// the drop silent sent it to `idle` with no reason and no failure: the
		// caller's handle answered "not found", and the panel showed a healthy
		// on-demand card for a connection that had died.
		dropped := st.dropInstancesOnSession(sessionID)
		if !st.detachFrom(sessionID) && len(dropped) == 0 {
			// Not one of this session's extensions: the hook fans out to every
			// state, and only the ones attached have anything to say about it.
			continue
		}
		for _, shellID := range dropped {
			st.recordClosed(sessionID, shellID, reason)
		}
		served = true
		r.fail(st, "session", reason)
	}
	if served {
		// The failure armed its own retry timer; there is no pass to wake.
		_ = served
	}
	// The group keeps the dead session's id rather than clearing it: the next
	// attempt replaces the corpse with a live connection, and resolving "does this
	// group have a session" is a liveness question everywhere else.
}
