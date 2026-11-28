package extensions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/transcript"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Failure is one recorded reason an extension was not brought up. History is
// kept rather than only the last entry because the shape of repeated failures is
// the diagnosis: always exit 127 says "wrong command", always a dial timeout
// says "wrong host".
type Failure struct {
	At     int64  `json:"at"`    // Unix ms
	Stage  string `json:"stage"` // target | dial | start | forward | session
	Reason string `json:"reason"`
	// Via names the host whose connection this failure belongs to, for a tunnel that
	// does not live on the extension's own (issue #330). It is what lets a reader see
	// which machine to go and look at, and what lets the status stay `available`: a
	// failure carrying a Via is about somebody else's connection, not this one.
	Via string `json:"via,omitempty"`
}

// ForwardStatus is a declared forward as the reconciler actually established it.
// ListenAddr/TargetAddr are the bound addresses, which differ from the
// declaration whenever local_port was left at 0 ("pick one").
type ForwardStatus struct {
	DirectionKind string `json:"kind"`
	ForwardID     string `json:"forward_id,omitempty"`
	ListenAddr    string `json:"listen_addr,omitempty"`
	TargetAddr    string `json:"target_addr,omitempty"`
	// Via is the host whose connection carries this tunnel, when it is not the
	// extension's own. It is reported so a reader can see WHERE a listener is: a
	// tunnel on another machine is invisible on this one, and a status that omitted
	// the host would leave `ss -ltn` and the reply disagreeing with nothing to explain
	// it (issue #330).
	Via string `json:"via,omitempty"`
}

// ensureForwards establishes the forwards the template declares, skipping the
// ones still listening. Returning an error leaves the session standing: a tunnel
// is a capability of the extension, not its reason to exist.
func (r *Reconciler) ensureForwards(ctx context.Context, ext *Extension, st *extState) error {
	if r.forwardMgr == nil || len(ext.Forwards) == 0 {
		return nil
	}
	d := st.load()
	if d.sessionID == "" {
		return errors.New("no session to attach forwards to")
	}
	sessID := d.sessionID
	existing := append([]ForwardStatus(nil), d.forwards...)

	sess := r.sessMgr.Get(sessID)
	if sess == nil {
		return errors.New("session for forwards is gone")
	}

	// WHICH SESSIONS MAY ALREADY HOLD ONE OF THESE TUNNELS. A via tunnel lives on the
	// CARRIER's session, not this one, so a live map built from the extension's own
	// session would consider every via tunnel missing and rebuild it onto a port it
	// already holds on the other machine — the failure that leaves an extension unable
	// to come back up.
	live := map[string]bool{}
	seen := map[string]bool{sessID: true}
	for _, fw := range r.forwardMgr.ListBySession(sessID) {
		live[fw.ForwardID] = true
	}
	for _, f := range ext.Forwards {
		if f.Via == "" || seen[f.Via] {
			continue
		}
		seen[f.Via] = true
		if carrier := r.carrierSession(ext, f.Via); carrier != nil {
			for _, fw := range r.forwardMgr.ListBySession(carrier.ID) {
				live[fw.ForwardID] = true
			}
		}
	}

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
		// THE TUNNEL IS BUILT ON THE CONNECTION THAT CARRIES IT (issue #330). Without
		// `via` that is this extension's own session — today's behaviour, unchanged. With
		// it, a connection to that host is opened for the tunnel alone.
		carrier, carrierCli, carrierInfo := sess, sess.SSHClient(), sess.Info()
		// ONE ERROR PATH FOR BOTH STEPS: opening the carrier connection and building the
		// tunnel fail for different reasons but mean the same thing to this loop, and
		// they must therefore record and report identically. They used to be two
		// branches, and the first one — a via host whose profile does not exist —
		// returned before the carrier-aware recording, so the extension reported itself
		// down over a machine it never touched (issue #330).
		var fw *forward.ForwardInfo
		var err error
		if f.Via != "" {
			if carrier, err = r.carrierFor(ctx, ext, f.Via); err != nil {
				fw = nil
			} else {
				carrierCli, carrierInfo = carrier.SSHClient(), carrier.Info()
			}
		}
		if err == nil {
			fw, err = r.createForward(f, carrier, carrierCli, carrierInfo)
		}
		if err != nil {
			// Record what this attempt did establish before reporting the failure:
			// the walk reads these entries to decide what is already listening.
			snapshot := append([]ForwardStatus(nil), built...)
			st.update(func(d *extStateData) { d.forwards = snapshot })
			// A TUNNEL ON ANOTHER HOST IS THAT HOST'S BUSINESS (issue #330). The
			// extension's own channel is up and usable; only the machine on the far side
			// of `via` could not be reached. Reporting the whole extension as down would
			// take away a capability that works — the caller would stop calling an MCP
			// endpoint that answers — so the failure is recorded against the tunnel and
			// the walk retries that alone (bringUp's retry reuses the running channel,
			// which is exactly "retry the tunnel").
			if f.Via != "" {
				st.recordCarrierFailure("forward", fmt.Sprintf("%s forward on %s via %s: %v",
					f.DirectionKind, listenSpec(f), f.Via, err), f.Via)
				continue
			}
			return fmt.Errorf("%s forward on %s: %w", f.DirectionKind, listenSpec(f), err)
		}
		// Tear down with the session rather than outliving it as a dangling
		// listener, the same way the forward tool attaches its own — and with the
		// CARRIER's session, which is where the listener actually lives.
		carrier.AttachCleanup(func() { r.forwardMgr.Close(fw.ForwardID) })
		built[i] = ForwardStatus{
			DirectionKind: f.DirectionKind,
			ForwardID:     fw.ForwardID,
			ListenAddr:    fw.ListenAddr,
			TargetAddr:    fw.TargetAddr,
			Via:           f.Via,
		}
	}

	st.update(func(d *extStateData) { d.forwards = built })
	return nil
}

// tunnelFailures reports how many recorded failures belong to a tunnel on another
// host, which is what tells the walk its next attempt is worth making even though the
// extension itself is up.
func (st *extState) tunnelFailures() int {
	n := 0
	for _, f := range st.load().failures {
		if f.Via != "" {
			n++
		}
	}
	return n
}

func (r *Reconciler) createForward(f ForwardDecl, sess *session.Session, sshCli *ssh.Client, info api.Session) (*forward.ForwardInfo, error) {
	switch f.DirectionKind {
	case ForwardDynamic:
		return r.forwardMgr.CreateDynamic(sess.ID, info.Name, f.BindHost, f.LocalPort, sshCli, info.SSHEndpoint == "internal")
	case ForwardLocal:
		return r.forwardMgr.CreateLocal(sess.ID, info.Name, f.BindHost, f.RemoteHost, f.RemotePort, f.LocalPort, sshCli)
	case ForwardRemote:
		return r.forwardMgr.CreateRemote(sess.ID, info.Name, f.LocalHost, f.LocalPort, f.RemoteHost, f.RemotePort, sshCli)
	}
	return nil, fmt.Errorf("unknown forward direction %q", f.DirectionKind)
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

// carrierFor returns the session a `via` tunnel is built on, opening the connection
// when it is not up yet.
//
// ONE CONNECTION PER HOST, SHARED. The group is keyed by the host and a fixed purpose
// string, so two extensions tunnelling through the same machine share one SSH
// connection instead of opening one each — and a tunnel retry finds the connection it
// already has rather than redialling.
func (r *Reconciler) carrierFor(ctx context.Context, ext *Extension, host string) (*session.Session, error) {
	plan, err := r.tunnelPlan(ext, host)
	if err != nil {
		return nil, err
	}
	g := r.group(tunnelGroupKey(host), ext)
	return r.ensureGroupSession(ctx, g, plan.config)
}

// carrierSession reports the live carrier session for a host, without opening one.
func (r *Reconciler) carrierSession(ext *Extension, host string) *session.Session {
	g := r.group(tunnelGroupKey(host), ext)
	return r.liveSession(g.session())
}

// tunnelGroupKey keeps a carrier connection out of the namespace the extension's own
// channel uses: it has no channel, no restriction and no command, so sharing a group
// with a template would mean two different session configs under one key.
func tunnelGroupKey(host string) string {
	return "via\x00" + host
}

// tunnelPlan is the plan for a connection that exists only to carry tunnels.
//
// IT IS THE EXTENSION'S OWN RESOLVE WITH EVERYTHING CHANNEL-SHAPED REMOVED, rather
// than a second copy of the dial logic: the URL bridging, the command and the mode are
// what would otherwise leak in, and clearing them is one place to get right instead of
// a parallel resolver to keep in step.
func (r *Reconciler) tunnelPlan(ext *Extension, host string) (hostPlan, error) {
	bare := *ext
	bare.Target = host
	bare.Targets = nil
	bare.URL = ""
	bare.Command = ""
	bare.Args = nil
	bare.Mode = ""
	bare.Forwards = nil
	bare.Exposes = nil
	return r.resolve(&bare)
}
