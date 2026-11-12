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
}

// ForwardStatus is a declared forward as the reconciler actually established it.
// ListenAddr/TargetAddr are the bound addresses, which differ from the
// declaration whenever local_port was left at 0 ("pick one").
type ForwardStatus struct {
	DirectionKind string `json:"kind"`
	ForwardID     string `json:"forward_id,omitempty"`
	ListenAddr    string `json:"listen_addr,omitempty"`
	TargetAddr    string `json:"target_addr,omitempty"`
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
			return fmt.Errorf("%s forward on %s: %w", f.DirectionKind, listenSpec(f), err)
		}
		// Tear down with the session rather than outliving it as a dangling
		// listener, the same way the forward tool attaches its own.
		sess.AttachCleanup(func() { r.forwardMgr.Close(fw.ForwardID) })
		built[i] = ForwardStatus{
			DirectionKind: f.DirectionKind,
			ForwardID:     fw.ForwardID,
			ListenAddr:    fw.ListenAddr,
			TargetAddr:    fw.TargetAddr,
		}
	}

	st.mu.Lock()
	st.forwards = built
	st.mu.Unlock()
	return nil
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
