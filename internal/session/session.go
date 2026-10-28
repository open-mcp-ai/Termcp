package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"golang.org/x/crypto/ssh"
)

// Lock ordering: shellStateMu -> mu -> stdinMu (and shellStateMu -> cs.mu).
// Never acquire in reverse order. s.mu and cs.mu are leaves; shellStateMu is
// only taken by session-level transitions (close/DEAD/terminate, new shells).
// The manager-assigned callbacks and the per-shell closed flag are atomics and
// need no lock (see field docs below).

// OnExitPolicy decides what a session does when its last shell channel ends by
// itself (the user typing `exit`, or a run-to-exit pipe command finishing).
//
// It exists because the container contract and the one-shot use case pull in
// opposite directions: the SSH connection outliving its shells is what keeps
// forwards/SFTP/new channels usable, but an agent that started a session only to
// run one command has nothing left to reuse and leaves a "running" tile behind.
// The policy is per session, so both behaviours coexist.
type OnExitPolicy string

const (
	// OnExitKeep (default) leaves the container running after its last shell ends.
	OnExitKeep OnExitPolicy = "keep"
	// OnExitClose terminates the container once its last shell ends: the session
	// goes DEAD and moves to the archive, and its output stays readable.
	OnExitClose OnExitPolicy = "close"
)

// Config holds parameters for creating a new Session.
type Config struct {
	Command string
	Args    []string
	// Mode applies to the first shell only. Mode is per shell channel; a session
	// is a connection container and owns no mode of its own.
	Mode api.SessionMode
	Name string
	// SSHConfig is the profile name that established this session. It is kept
	// separately from Name because the display name is user-editable.
	SSHConfig         string
	SSHConfigIdentity string
	Rows              int
	Cols              int
	Remote            *RemoteSSH
	// DefaultShell is the profile's default_shell, kept so shells opened later on
	// this connection resolve their shell the same way the first one did: the
	// caller's command, then this, then the server's own login shell.
	DefaultShell string
	// Approval turns review mode on as the session is created, so a profile can
	// make "every write to this host is reviewed" the default rather than a
	// switch someone has to remember after launching.
	Approval bool
	// OnExit decides what happens to the container when its last shell channel
	// ends by itself. "" or "keep" (the default) leaves it running: an interactive
	// shell the user typed `exit` in, or a pipe command that finished, says
	// nothing about the SSH connection, which still carries forwards, SFTP and
	// new channels. "close" terminates the container when that last shell exits,
	// which is what a one-shot command wants -- it was started to run one program,
	// and leaving the session "running" in the list forever is the cost of
	// treating it like a reusable connection.
	OnExit OnExitPolicy
	// Ctx, when non-nil, cancels the dial: a browser that closed its pending
	// window (or an agent whose request was aborted) stops the connect it started
	// instead of having a session it can no longer reach appear in the list. It
	// applies to establishing the transport only — a created session outlives the
	// request that asked for it, so the caller's request context must NOT be kept.
	Ctx context.Context
	// NoShell creates the container without a primary shell channel. The session
	// then exists to carry SFTP and forwards, which an extension whose purpose is
	// a read-only file window or a declared tunnel wants without paying for a
	// shell nobody will drive. Mode is ignored when NoShell is set.
	NoShell bool
	// Restriction trims which tools may act on this session. It is the mechanism
	// behind extensions: a zero value leaves an ordinary session unrestricted,
	// and a session created from an extension template carries the whitelist the
	// template's author defined.
	Restriction Restriction
	// Namespace marks the session as owned by a subsystem rather than requested
	// by a user. It travels into the session record and the manifest, so the Web
	// UI's cards, the node cards' session counts and the agent's session_list can
	// leave it out while its terminal stays openable by whoever holds its id (see
	// internal/ns). Empty means an ordinary session.
	Namespace string
}

// Session wraps an interactive process session managed over SSH.
// Session is a connection container; terminal I/O is addressed by shell_id via ChildShell.
type Session struct {
	api.Session
	mu            sync.RWMutex
	shellStateMu  sync.Mutex // serializes child registration with close/DEAD transition
	closing       bool       // guarded by shellStateMu; blocks new child channels
	stdinMu       sync.Mutex
	terminateOnce sync.Once
	deadOnce      sync.Once
	exitOnce      sync.Once
	execSession   *sshclient.ExecSession
	buf           *buffer.Buffer
	readerID      int
	msgMgr        *message.Manager
	// scope owns the session's lifecycle resources (forwards, notification rules,
	// minted credentials, readers). Subsystems attach themselves at creation;
	// Release() in finalize cascades the teardown in LIFO order.
	scope          *ResourceScope
	onDead         atomic.Pointer[func()] // invoked once when the session turns DEAD; assigned by the manager right after New(), while exit watchers may already be reading
	onChildChange  atomic.Pointer[func()] // invoked when child shells are added/removed; assigned under the same constraint
	onOutput       atomic.Pointer[func(shellID string)]
	onInput        atomic.Pointer[func(shellID string, src InputSource, submit bool)] // invoked when a shell receives input
	onShellExit    atomic.Pointer[func(shellID string, exitCode *int)]
	onShellClose   atomic.Pointer[func(shellID string)]
	enterCRLF      bool         // line-ending for pipe-mode enter (\r\n for cmd/powershell, \n for unix)
	defaultShell   string       // profile default_shell, applied to shells opened later
	onExit         OnExitPolicy // what to do when the last shell ends by itself
	primaryShellID string       // first shell id (≠ session id); addressed by session-level helpers; empty when NoShell
	nextShellIndex int          // guarded by shellStateMu; monotonically assigns channel indexes
	restriction    Restriction  // the zero value is unrestricted; see Config.Restriction

	shells sync.Map // *ChildShell by ID
	// approval gates input for the whole session when non-nil (see approval.go).
	// It is session-scoped because production access is per-target: one
	// deployment can hold a scratch host and a prod host at the same time.
	approval *approvalState
	// approvalSink is the change handler installed before approval mode is
	// enabled, so a handler set at startup is not lost.
	approvalSink func(sessionID string, req approval.Request)

	// shellHistory retains the last-known per-shell metadata (id, name, status)
	// so a DEAD session can still render per-shell tabs after shells leave the
	// live map on exit. Also persisted to the shell manifests for restart restore.
	shellHistory sync.Map // string → api.Session
	// doneWG tracks live output pipe goroutines so markDead can flush their final
	// log writes before the session is observed as exited.
	doneWG sync.WaitGroup
	// watchWG tracks per-shell exit-watcher goroutines. The watcher performs the
	// final log writes and persist() after the transport closes, so finalize must
	// wait for them: otherwise Delete could return (and a caller could purge/remove
	// the data dir) while a watcher is still appending log.bin. The watcher is
	// deliberately not in doneWG: markDead (called *by* the watcher on a pipe exit)
	// waits on doneWG, so adding the watcher there would self-deadlock.
	watchWG sync.WaitGroup
}

// newResourceID mints one resource id. Session and shell ids are the same kind
// of value — a short opaque token that names an object in a URL and on disk — so
// they come from one place rather than two copies of the same expression. The
// 12-hex-character form keeps ids short enough to read out of a URL while
// staying collision-free in practice; storage.validateID accepts it (lowercase
// hex is within [a-zA-Z0-9_-]).
func newResourceID() string {
	return uuid.New().String()[:12]
}

// New creates and starts a new Session.
// internal must be the built-in sshserver.Server (after Start) when cfg.Remote is nil; it may be nil for remote-only callers.
func New(internal *sshserver.Server, cfg Config, msgMgr *message.Manager) (*Session, error) {
	sessionID := newResourceID()
	name := cfg.Name
	if name == "" {
		name = fmt.Sprintf("session-%s", sessionID)
	}

	usePty := cfg.Mode == api.ModePTY

	execSession, sshEndpointPublic, err := dialTransport(internal, cfg, usePty)
	if err != nil {
		return nil, err
	}
	// The dial can also finish in the same instant the caller gives up. Dropping
	// the transport here rather than registering it keeps the promise the cancel
	// makes: after a cancelled request, no session exists to be listed. Without
	// this the race is small but real -- a fast host completes the dial just as
	// the window closes, and the session appears with nobody attached to it.
	if cfg.Ctx != nil && cfg.Ctx.Err() != nil {
		_ = execSession.Close()
		return nil, cfg.Ctx.Err()
	}

	// Determine the target shell family so press_enter sends the right line
	// ending. For internal sessions the target is the local host, so we probe
	// it via the shell detector. For remote sessions the target OS is unknown
	// without an extra round-trip; unix (\n) is the safe default for SSH targets.
	enterCRLF := false
	if sshEndpointPublic == "internal" {
		if _, family, _ := shell.NewDetector().Detect(); family != "unix" {
			enterCRLF = true
		}
	}

	s := &Session{
		Session: api.Session{
			ID:                sessionID,
			Name:              name,
			SSHConfig:         cfg.SSHConfig,
			SSHConfigIdentity: cfg.SSHConfigIdentity,
			Command:           cfg.Command,
			Args:              cfg.Args,
			Status:            api.SessionRunning,
			CreatedAt:         clock.Now(),
			UpdatedAt:         clock.Now(),
			Rows:              cfg.Rows,
			Cols:              cfg.Cols,
			SSHEndpoint:       sshEndpointPublic,
			Namespace:         cfg.Namespace,
		},
		enterCRLF:    enterCRLF,
		defaultShell: cfg.DefaultShell,
		onExit:       normalizeOnExit(cfg.OnExit),
		execSession:  execSession,
		restriction:  cfg.Restriction,
		msgMgr:       msgMgr,
		scope:        newResourceScope(),
	}

	// Message-manager session state is a session-scoped resource: release it
	// through the same cascade that closes forwards and notification rules.
	if msgMgr != nil {
		s.AttachCleanup(func() { msgMgr.ForgetSession(sessionID) })
	}

	// A shell-less container is a connection that exists to carry SFTP and
	// forwards. It skips the primary channel entirely, so the first shell opened
	// on it later still becomes channel 1 — the public numbering a locator
	// (termcp://#<sid>:1) refers to. Only the transport is watched, which is the
	// one observer that survives having no shells at all.
	if cfg.NoShell {
		s.watchTransport(execSession)
		slog.Debug("session started without a shell", "session_id", sessionID, "ssh_endpoint", sshEndpointPublic)
		return s, nil
	}

	// The primary channel: a peer in the shells map whose id is never equal to
	// the session id. Mint its id here, after the shell-less path has returned,
	// so an unused id is not allocated for a container that will never have it.
	shellID := newResourceID()
	buf := buffer.New(1024 * 1024)
	rid, _ := buf.NewReader()
	s.buf = buf
	s.readerID = rid
	s.primaryShellID = shellID
	s.nextShellIndex = 1

	root := &ChildShell{
		ID:          shellID,
		Index:       1,
		Name:        name,
		execSession: execSession,
		buf:         buf,
		done:        make(chan struct{}),
		CreatedAt:   clock.Now(),
		enterCRLF:   enterCRLF,
		mode:        cfg.Mode,
	}
	root.setStatus(api.SessionRunning)
	root.rows.Store(int32(cfg.Rows))
	root.cols.Store(int32(cfg.Cols))

	// "Process started" is a session lifecycle event, not shell output. It used to
	// be stored as a system message; the new layout has no message records, so it
	// is logged rather than persisted as a transcript entry. Session lifecycle is
	// still visible in the session manifest's status and timestamps.
	slog.Debug("session ready", "session_id", sessionID, "shell_id", shellID)
	// Attach the root to the session BEFORE starting its readers so the output
	// pipe and exit watcher see a live parent + message manager from the first
	// byte. Starting readers first used to drop early output (never persisted).
	root.parent.Store(s)
	s.shells.Store(root.ID, root)
	s.shellHistory.Store(root.ID, root.Info())
	root.startReaders()
	// Watch the transport itself, not just its channels. A shell's exit watcher
	// only reports a lost connection while that shell is alive; once every shell
	// has ended (a user typing `exit`, a run-to-exit command finishing) nothing
	// else observes the connection, and a session whose SSH peer went away kept
	// reporting "running" forever -- listed as a live session, refusing to move to
	// the archive, while every operation on it failed with "new session: EOF".
	//
	// This is the only signal that survives having no shells, and it does not
	// duplicate the per-shell path: markDead is idempotent, so whichever watcher
	// notices first wins and the message is recorded once.
	s.watchTransport(execSession)

	slog.Debug("session started", "session_id", sessionID, "shell_id", shellID, "command", cfg.Command, "ssh_endpoint", sshEndpointPublic)

	return s, nil
}

// watchTransport marks the session DEAD when its SSH connection shuts down.
//
// It exists because connection loss is otherwise only noticed by a running
// shell's watcher. A shell ending — cleanly or not — never ends the container
// (the transport still carries forwards, SFTP and new channels), so the
// session legitimately has states with no live shell to watch anything; in
// those states an SSH peer that disappears used to go unnoticed and the session
// stayed "running" for the life of the process.
//
// WaitTransport blocks until the connection is down, so this goroutine costs
// nothing while the transport lives. finalize() closes the transport before
// waiting on watchWG, which is what releases it.
func (s *Session) watchTransport(es *sshclient.ExecSession) {
	if es == nil {
		return // restored placeholder with no live transport
	}
	s.watchWG.Add(1)
	go func() {
		defer s.watchWG.Done()
		// THE REASON IS LOGGED WHERE AN OPERATOR LOOKS.
		//
		// This line used to be Debug, which is BELOW the daemon's default level (Info),
		// so on a normal run it was never written — and the one durable record of why a
		// session disappeared was therefore off. The session itself is then gone from
		// every surface, and the transcript message says the same sentence whether the
		// network dropped, the far side closed, or something else ended the transport
		// (issue #176). Four sessions were lost in one afternoon and ruling out the
		// causes took four separate experiments; the error that would have answered it
		// was in this discarded line.
		//
		// Warn rather than Info because a transport that ends with an error is not the
		// session's ordinary exit: the caller may not have asked for it.
		// THE TRANSCRIPT SAYS WHICH OF THE TWO IT WAS, because the caller reads the
		// transcript and not the log. One sentence for both events ("SSH connection lost
		// — network disconnected") claimed a cause nobody had established: the same text
		// covered a dropped network, a closed far side and a restarted relay (issue
		// #176). The error decides, and it is also in the log line above.
		message := transportClosedMessage
		if err := es.WaitTransport(); err != nil {
			slog.Warn("session transport lost", "session_id", s.ID, "err", err)
			// The error IS the cause, and it is the only thing that can distinguish a
			// dropped network from a far side that went away. The sentence no longer
			// asserts one on its own (issue #176).
			message = transportLostMessage + ": " + err.Error()
		} else {
			slog.Info("session transport closed", "session_id", s.ID)
		}
		// A deliberate close (Terminate, Disconnect, Delete) sets closing before it
		// touches the transport, so markDeadWithMessage drops this notification
		// instead of reporting an intentional teardown as a network loss.
		s.markDeadWithMessage(message)
	}()
}

// dialTransport establishes the transport a session runs on and reports which
// kind it is ("internal" or "remote", the two values published to MCP and the
// JSON API - never a host or a credential).
//
// It is the branch that decides between the process's own loopback sshd and a
// real remote target, which are the two ways a session can exist; separating it
// keeps the session constructor about assembling a Session rather than about
// dialing. The internal branch revokes its one-time credential on every failure
// path: the credential is minted before the dial, and leaving it pending would
// grow the server's map by one dead entry per failed attempt for the life of the
// process.
func dialTransport(internal *sshserver.Server, cfg Config, usePty bool) (*sshclient.ExecSession, string, error) {
	client, closers, endpoint, err := dialClient(internal, cfg)
	if err != nil {
		return nil, "", err
	}
	if cfg.NoShell {
		// A shell-less container is the connection and nothing more: SFTP and
		// forwards need the client, and a channel would start a process on the
		// target that nothing drives and nothing watches — a container's liveness
		// is the connection's.
		return sshclient.BareClient(client, true, closers), endpoint, nil
	}
	es, err := sshclient.StartWithChain(cfg.Ctx, client, closers, cfg.Command, cfg.Args, usePty, cfg.Rows, cfg.Cols)
	if err != nil {
		return nil, "", err
	}
	return es, endpoint, nil
}

// dialClient acquires the SSH connection a session runs on and reports which
// kind it is ("internal" or "remote", the two values published to MCP and the
// JSON API - never a host or a credential). It opens no channel: what the
// connection then carries is dialTransport's business.
//
// It is the branch that decides between the process's own loopback sshd and a
// real remote target, which are the two ways a session can exist; separating it
// keeps the session constructor about assembling a Session rather than about
// dialing. The internal branch revokes its one-time credential on every failure
// path: the credential is minted before the dial, and leaving it pending would
// grow the server's map by one dead entry per failed attempt for the life of the
// process.
func dialClient(internal *sshserver.Server, cfg Config) (*ssh.Client, []io.Closer, string, error) {
	ctx := cfg.Ctx
	if !isRemote(cfg) {
		if internal == nil {
			return nil, nil, "", errors.New("internal ssh server is not configured")
		}
		minted, err := internal.MintClientConfig()
		if err != nil {
			return nil, nil, "", err
		}
		conn, err := internal.Dial()
		if err != nil {
			// The credential was never used: drop it instead of leaving a dead
			// entry in the server's pending map for the process lifetime.
			internal.RevokeClientConfig(minted.User)
			return nil, nil, "", err
		}
		client, err := sshclient.Handshake(ctx, conn, "inmem", minted)
		if err != nil {
			// A failed handshake consumes nothing, so the one-time credential is
			// still pending; revoke it to keep the map bounded by live sessions.
			internal.RevokeClientConfig(minted.User)
			return nil, nil, "", err
		}
		return client, nil, "internal", nil
	}

	r := cfg.Remote
	port := r.Port
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return nil, nil, "", fmt.Errorf("ssh_port must be between 1 and 65535, got %d", port)
	}
	if r.Jump != nil {
		client, closers, err := buildChainClient(ctx, r)
		if err != nil {
			return nil, nil, "", err
		}
		return client, closers, "remote", nil
	}
	clientCfg, err := remoteClientConfig(r)
	if err != nil {
		return nil, nil, "", err
	}
	addr := remoteDialAddr(r)
	conn, err := sshclient.DialConn(ctx, addr, r.Proxy, clientCfg.Timeout)
	if err != nil {
		return nil, nil, "", fmt.Errorf("ssh dial: %w", err)
	}
	client, err := sshclient.Handshake(ctx, conn, addr, clientCfg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("ssh handshake: %w", err)
	}
	return client, nil, "remote", nil
}

// PrimaryShellID returns the first shell created with this session. It is empty
// for a shell-less container (Config.NoShell), which carries SFTP and forwards
// but no channel to drive; callers that address the primary shell must treat
// empty as "no terminal here".
func (s *Session) PrimaryShellID() string {
	return s.primaryShellID
}

// PrimaryShell returns the first shell if still registered.
func (s *Session) PrimaryShell() *ChildShell {
	return s.GetChildShell(s.primaryShellID)
}

// Restriction reports what the tool surface may do to this session. The zero
// value is unrestricted; a session created from an extension template carries
// the whitelist that template defined. Tool handlers consult it before acting.
func (s *Session) Restriction() Restriction {
	return s.restriction
}

// Terminate ends the remote process/transport and marks the session DEAD
// (exited) in place. The session object, its shells' metadata and retained
// buffers, and its byte log are all kept for read-only viewing; nothing
// is removed from the registry or purged. Only Manager.Delete/finalize release
// resources.
func (s *Session) Terminate(force bool, gracePeriod time.Duration) {
	s.terminateOnce.Do(func() {
		s.beginClosing()
		es := s.execSession
		if es == nil {
			// Restored (restart) placeholder with no live transport.
			s.markDead()
			return
		}
		if !force {
			es.Signal(ssh.SIGTERM)
			select {
			case <-es.Done():
				s.markDead()
				return
			case <-time.After(gracePeriod):
			}
		}

		// Close the session's own transport, not sibling session channels.
		// For remote sessions the shared SSH client is left for Disconnect(); for
		// internal loopback there's no multiplexing so close everything here.
		if s.SSHEndpoint == "internal" {
			es.Close()
		} else {
			es.CloseSessionOnly()
		}
		s.markDead()
	})
}

// transportLostMessage is the system message recorded when a session's SSH
// connection dies under it (network loss, remote sshd gone). The per-shell
// watcher and the session-level transport watcher report the same event, so the
// text lives here rather than at either call site.
//
// It names no cause: "— network disconnected" used to be appended here and was a
// diagnosis this code is not in a position to make. The watcher that HAS the error
// appends it; the per-shell watcher, which does not, leaves it at this sentence.
const transportLostMessage = "❌ SSH connection lost"

// transportClosedMessage is the same event with NO error behind it: the transport
// ended cleanly, which is what a far side that shut down on purpose looks like. It is
// a separate sentence because the transcript is the only record the caller has, and
// "network disconnected" is a diagnosis this code is not in a position to make from a
// nil error.
const transportClosedMessage = "❌ the session's transport closed"

// exitReasonRequested is why a session the CALLER closed says it is gone. It is
// recorded rather than left empty because "no reason" and "nobody asked" are
// different states, and `get` prints whichever it was (issue #176).
const exitReasonRequested = "❌ closed on request"

// normalizeOnExit maps anything that is not the explicit "close" to the default
// behaviour. An unknown value must not silently end a user's connection: the
// safe reading of a typo is "keep it alive".
func normalizeOnExit(p OnExitPolicy) OnExitPolicy {
	if p == OnExitClose {
		return OnExitClose
	}
	return OnExitKeep
}

// closeIfLastShellEnded implements Config.OnExit for the one-shot case: when a
// shell ends by itself and it was the last live channel, a session configured
// with OnExitClose is terminated, moving it to the archive with its output still
// readable.
//
// It is called by the exit watcher, after the shell has left Running. "Last" is
// decided by the live map, and the transition is guarded by the same
// shellStateMu that registration uses, so a shell being opened concurrently
// either lands before the check (and the session stays) or finds the session
// already closing (and is refused) -- a new channel can never be added to a
// session that this call is about to close.
func (s *Session) closeIfLastShellEnded() {
	if s.onExit != OnExitClose {
		return
	}
	s.shellStateMu.Lock()
	live := false
	s.shells.Range(func(_, v any) bool {
		cs := v.(*ChildShell)
		if cs.statusValue() == api.SessionRunning {
			live = true
			return false
		}
		return true
	})
	closing := s.closing
	s.shellStateMu.Unlock()
	// A live channel (or a session already going down) means this was not the
	// end of the session's work; another shell will report its own exit.
	if live || closing {
		return
	}
	slog.Debug("one-shot session closing after its last shell exited", "session_id", s.ID)
	s.Terminate(true, 0)
}

// markDead transitions a Running session to exited (DEAD) in place, retaining
// the object in the registry, its retained buffers, and its byte log.
// Idempotent (runs once). It never removes the registry entry, forgets logs,
// or closes retained buffers; only Manager.Delete → finalize do that.
func (s *Session) markDead() {
	s.markDeadWithMessage("")
}

func (s *Session) markDeadWithMessage(systemMessage string) {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()
	// A deliberate close may race with an exit watcher that already observed
	// the transport error. Do not turn that intentional close into a network-loss
	// notification.
	if systemMessage != "" && s.closing {
		return
	}
	// A session the CALLER closed has a reason too, and "no reason" is not it: whoever
	// asks `get` after a close wants to be told the close was theirs, not left to guess
	// between that and something breaking. beginClosing() sets s.closing before the
	// deliberate paths call here, which is exactly what distinguishes them.
	if systemMessage == "" && s.closing {
		systemMessage = exitReasonRequested
	}
	s.markDeadLocked(systemMessage)
}

// markDeadLocked requires shellStateMu.
func (s *Session) markDeadLocked(systemMessage string) {
	s.closing = true
	s.deadOnce.Do(func() {
		// Flush output pipe goroutines so the last bytes are appended to the
		// byte log before DEAD becomes observable.
		flushDone := make(chan struct{})
		go func() { s.doneWG.Wait(); close(flushDone) }()
		select {
		case <-flushDone:
		case <-time.After(500 * time.Millisecond):
		}

		if systemMessage != "" {
			// Session lifecycle, not shell output: it has no bytes and therefore no
			// place in the byte log. Recorded in the log stream instead.
			slog.Debug("session exit", "session_id", s.ID, "message", systemMessage)
		}

		s.mu.Lock()
		if s.Status == api.SessionRunning {
			s.Status = api.SessionExited
			s.UpdatedAt = clock.Now()
		}
		// WHY, not just THAT. This is the only record of the reason that outlives the
		// process: the caller asks precisely when the session is no longer live, and by
		// then the transcript (which carries the same sentence) may be gone with the
		// directory (issue #176). A deliberate close passes "" and keeps the field empty
		// rather than claiming a cause.
		if systemMessage != "" && s.ExitReason == "" {
			s.ExitReason = systemMessage
		}
		s.mu.Unlock()

		slog.Debug("session DEAD", "session_id", s.ID)
		if fn := s.onDead.Load(); fn != nil {
			(*fn)()
		}
	})
}

// finalize releases the session's resources: remaining shells, buffers, and
// transport. Its only caller is Manager.Delete. Runs once.
func (s *Session) finalize() {
	s.exitOnce.Do(func() {
		s.beginClosing()
		s.terminateChildren()
		if s.buf != nil {
			s.buf.Close()
		}
		if s.execSession != nil {
			_ = s.execSession.Close()
		}
		// Wait for exit watchers to finish their post-transport writes (final
		// system message + persist) before releasing the session, so a caller
		// that purges the data directory afterwards cannot race a writer.
		s.watchWG.Wait()
		// Cascade: cancel every goroutine bound to the session scope and run the
		// registered cleanups (forwards, notification rules, minted credentials,
		// readers) in LIFO order. Nothing else needs to know what is attached.
		if s.scope != nil {
			s.scope.Release()
		}
	})
}

// Context returns the session's lifecycle context. It is canceled when the
// session is finalized (deleted/purged), so long-lived loops or network dialers
// bound to this session exit automatically without a coordinator polling them.
func (s *Session) Context() context.Context {
	if s.scope == nil {
		return context.Background()
	}
	return s.scope.Context()
}

// AttachCleanup binds an arbitrary teardown function to the session's
// ResourceScope. When the session is finalized (deleted), fn is invoked in
// LIFO order alongside other attached resources. If the session has already
// finalized, fn runs immediately.
func (s *Session) AttachCleanup(fn func()) {
	if s.scope == nil {
		if fn != nil {
			fn()
		}
		return
	}
	s.scope.Add(fn)
}

// AttachCloser registers an io.Closer to be closed when the session is finalized.
func (s *Session) AttachCloser(c io.Closer) {
	if s.scope == nil {
		if c != nil {
			_ = c.Close()
		}
		return
	}
	s.scope.AddCloser(c)
}

// TerminateShellOnly closes the primary shell channel by shell id. For internal sessions this is a
// no-op — the process outlives the tab (like detaching from screen/tmux). For remote sessions only
// that SSH session channel is closed; other shells on the same TCP connection are unaffected.
func (s *Session) TerminateShellOnly() {
	if s.SSHEndpoint == "internal" {
		return // tab close doesn't kill the process
	}
	if cs := s.PrimaryShell(); cs != nil {
		cs.TerminateShell()
	}
}

// Disconnect closes the session transport and marks the session DEAD in place,
// preserving the object, buffers, and history for read-only viewing.
func (s *Session) Disconnect() {
	s.beginClosing()
	if s.execSession != nil {
		s.execSession.Close()
	}
	s.markDead()
}

func (s *Session) beginClosing() {
	s.shellStateMu.Lock()
	if s.closing {
		s.shellStateMu.Unlock()
		return
	}
	s.closing = true
	s.shells.Range(func(_, v any) bool {
		v.(*ChildShell).deliberateClose.Store(true)
		return true
	})
	s.shellStateMu.Unlock()

	// A closing session can no longer execute anything, so its pending requests
	// must not stay decidable. Without this, Terminate (which does not run the
	// scope cascade) left them pending: a later approve succeeded and recorded a
	// grant, while the bytes were refused because the process was already gone.
	// The decision then read as effective in the audit trail when nothing ran.
	//
	// Called after releasing shellStateMu: the lock order in this file is
	// shellStateMu then s.mu (see CreateChildShell), and DisableApproval takes
	// s.mu.
	s.DisableApproval()
}

// terminateChildren closes all child shells. Called during parent session termination.
// Holds write lock long enough to snapshot+clear the map, preventing races with Create/Close.
func (s *Session) terminateChildren() {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()

	var children []*ChildShell
	s.shells.Range(func(k, v any) bool {
		s.shells.Delete(k)
		children = append(children, v.(*ChildShell))
		return true
	})
	for _, cs := range children {
		cs.TerminateShell()
	}
}

// Info returns a deep copy of the session metadata.
func (s *Session) Info() api.Session {
	s.mu.RLock()
	approvalState := s.approval
	cp := s.Session
	if cp.ExitCode != nil {
		v := *cp.ExitCode
		cp.ExitCode = &v
	}
	s.mu.RUnlock()

	// Approval fields are derived from live state rather than stored on the
	// embedded api.Session, so a manifest written before approval mode existed
	// cannot claim the session is gated (or ungated).
	if approvalState != nil {
		cp.ApprovalMode = true
		cp.ApprovalNeed = approvalState.queue.Need()
		// Empty for "every kind": the JSON key is omitted then, so an older
		// reader sees exactly the manifest it always did.
		for _, k := range approvalState.scopeList() {
			cp.ApprovalScopes = append(cp.ApprovalScopes, string(k))
		}
	} else {
		cp.ApprovalMode = false
		cp.ApprovalNeed = 0
		cp.ApprovalScopes = nil
	}
	return cp
}

// HasMoreOutput returns whether the given reader has unread data.
// SSHClient returns the underlying SSH client. All sessions (including internal/loopback)
// use SSH transport, so this never returns nil for a running session.
func (s *Session) SSHClient() *ssh.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.execSession == nil {
		return nil
	}
	return s.execSession.SSHClient()
}
