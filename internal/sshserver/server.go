package sshserver

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/charmbracelet/ssh"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/pkg/sftp"
)

// Server wraps an internal SSH server.
type Server struct {
	server   *ssh.Server
	listener *inMemListener
	started  atomic.Bool
	mu       sync.Mutex
	// pending maps one-time username -> password. A successful password auth removes the entry.
	pending map[string]string
}

// ptyContextKey is the per-session context key under which the PTY allocated
// for a session is handed from the session request goroutine to the session
// handler. Keying by session keeps concurrent shells multiplexed on one SSH
// connection apart.
type ptyContextKey struct{ sess ssh.Session }

// sessionPTY is the per-session PTY handoff cell.
type sessionPTY struct {
	// mu guards the PTY against concurrent use from the handler goroutine and
	// the request goroutine. charmbracelet/ssh closes the PTY from the session
	// request goroutine when the client closes the channel, which can land in
	// the middle of the handler's fork/exec — and os.File.Fd is documented as
	// unsafe to call concurrently with Close (the child could inherit a
	// recycled descriptor). Taking mu around pty.Start in the handler and
	// around the library's closer removes that interleaving.
	mu  sync.Mutex
	pty ssh.Pty
	ok  bool
}

// stashPTY records the PTY the client requested for sess. It runs on the
// session request goroutine — the same goroutine that applies "window-change"
// updates — so this is the one place where the session's PTY struct can be read
// without racing charmbracelet/ssh's unlocked `sess.pty.Window` write.
func stashPTY(sess ssh.Session) {
	ps, _ := sess.Context().Value(ptyContextKey{sess}).(*sessionPTY)
	if ps == nil {
		return
	}
	if pty, _, ok := sess.Pty(); ok {
		ps.pty, ps.ok = pty, true
	}
}

// takePTY returns the PTY stashed for sess by stashPTY, if any, and clears the
// stash so the connection context does not retain the session (and its pty).
func takePTY(sess ssh.Session) (*sessionPTY, bool) {
	key := ptyContextKey{sess}
	ps, _ := sess.Context().Value(key).(*sessionPTY)
	if ps == nil || !ps.ok {
		return nil, false
	}
	sess.Context().SetValue(key, nil)
	return ps, true
}

// drainWindowChanges applies every window the library delivers on winch to the
// session's PTY.
//
// charmbracelet/ssh runs its own goroutine that consumes winch and applies
// resizes, but it is fire-and-forget: errors are swallowed and nothing revives
// it. If its application of a window silently stops (observed on macOS, where
// resizes stopped reaching the child tty mid-session while every other byte of
// traffic kept flowing), buffered window-changes sit unapplied forever and the
// request loop eventually blocks on the full channel — every later resize of
// the session dies with no error anywhere. This consumer makes resize
// application independent of that goroutine: each window it dequeues is applied
// to the same PTY. It must never discard windows: an earlier discard-only
// consumer (removed in a664a5d) dropped roughly half of all resizes. When both
// consumers are alive they race for each window, but both apply what they get,
// so no window can be lost.
func drainWindowChanges(pty ssh.Pty, winch <-chan ssh.Window) {
	for win := range winch {
		_ = pty.Resize(win.Width, win.Height)
	}
}

// New creates an internal SSH server that communicates in-process via net.Pipe (no TCP port).
func New() *Server {
	s := &Server{
		pending: make(map[string]string),
	}
	srv := &ssh.Server{
		Handler: func(sess ssh.Session) {
			s.handleSession(sess)
		},
		PasswordHandler: func(ctx ssh.Context, password string) bool {
			return s.passwordOK(ctx.User(), password)
		},
		LocalPortForwardingCallback: func(ctx ssh.Context, dHost string, dPort uint32) bool {
			return true
		},
		// Capture the allocated PTY on the request goroutine (see stashPTY) so the
		// session handler never reads the session's PTY struct while a
		// window-change may be updating it.
		SessionRequestCallback: func(sess ssh.Session, _ string) bool {
			stashPTY(sess)
			return true
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(sess ssh.Session) {
				srv, err := sftp.NewServer(sess)
				if err != nil {
					slog.Error("sftp server start", "err", err)
					return
				}
				srv.Serve()
			},
		},
	}
	_ = srv.SetOption(ssh.AllocatePty())
	// Wrap the allocator so the PTY it creates is closed under the same lock the
	// session handler holds while forking the command onto it (see sessionPTY).
	allocPty := srv.PtyHandler
	srv.PtyHandler = func(ctx ssh.Context, sess ssh.Session, pty ssh.Pty) (func() error, error) {
		ps := &sessionPTY{}
		key := ptyContextKey{sess}
		sess.Context().SetValue(key, ps)
		if allocPty == nil {
			return func() error { return nil }, nil
		}
		closer, err := allocPty(ctx, sess, pty)
		if err != nil {
			sess.Context().SetValue(key, nil)
			return nil, err
		}
		if p, winch, ok := sess.Pty(); ok && !p.IsZero() {
			go drainWindowChanges(p, winch)
		}
		return func() error {
			ps.mu.Lock()
			defer ps.mu.Unlock()
			return closer()
		}, nil
	}
	s.server = srv
	return s
}

// Dial creates a new in-memory connection to this server. The returned net.Conn
// is the client side of a net.Pipe(); the server side is handed to the SSH server
// goroutine for handshake and session handling.
func (s *Server) Dial() (net.Conn, error) {
	if !s.started.Load() {
		return nil, net.ErrClosed
	}
	return s.listener.Dial()
}

// Start begins serving SSH connections on the in-memory listener.
func (s *Server) Start() error {
	pemBytes, err := generateHostKeyPEM()
	if err != nil {
		return fmt.Errorf("generate host key: %w", err)
	}
	if err := s.server.SetOption(ssh.HostKeyPEM(pemBytes)); err != nil {
		return fmt.Errorf("set host key: %w", err)
	}

	s.listener = newInMemListener()
	s.started.Store(true)

	go func() {
		if err := s.server.Serve(s.listener); err != nil {
			slog.Info("ssh server stopped", "err", err)
		}
	}()
	return nil
}

// Stop shuts down the SSH server.
//
// This does not wait for in-flight session handlers: Close() tears down the
// connections, which cancels each session's context, and handleSession uses that
// to kill the child it started. Nothing here needs to join the handlers — the
// goroutine count comes back down on its own — and a WaitGroup on this path would
// add an Add/Wait race (a panic, not an error) to satisfy a wait nobody needs.
func (s *Server) Stop() error {
	if !s.started.Load() {
		return nil
	}
	s.mu.Lock()
	s.pending = make(map[string]string)
	s.mu.Unlock()
	_ = s.listener.Close()
	return s.server.Close()
}

func sshSignalToOSSig(sig ssh.Signal) os.Signal {
	switch sig {
	case "TERM":
		return syscall.SIGTERM
	case "INT":
		return syscall.SIGINT
	case "KILL":
		return syscall.SIGKILL
	case "HUP":
		return syscall.SIGHUP
	default:
		return nil
	}
}

func (s *Server) handleSession(sess ssh.Session) {
	// Interactive sessions get the detected default shell, spawned bare with
	// no injected flags: shell-specific options (e.g. bash's +o histexpand)
	// abort shells that do not implement them, e.g. dash/busybox /bin/sh
	// fails with "illegal option +o histexpand".
	cmdArgs := sess.Command()
	if len(cmdArgs) == 0 {
		sh, shArgs := shell.NewDetector().Argv()
		cmdArgs = append([]string{sh}, shArgs...)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)

	// Forward signals from client to local process.
	//
	// The channel is registered once and never unregistered. The library reads the
	// slot it lives in from two places — the request loop, under the session lock,
	// and a replay goroutine it starts when a signal arrived before a channel was
	// registered — and that replay goroutine reads it WITHOUT the lock
	// (charmbracelet/ssh session.go, Signals). So the sess.Signals(nil) this code
	// used to do at teardown had no ordering against the read at all, which is the
	// pair the detector reported. Buffering is routine, not exotic: a client can
	// send its signal between exec being accepted and this line.
	//
	// Since the slot is never rewritten, something must keep reading the channel or
	// the request loop blocks sending into it while holding the session lock. So the
	// forwarder starts here, before the child exists, and drains until the
	// connection is gone — the only moment the library stops sending. It receives
	// the process through an atomic because Start writes that field concurrently.
	sigCh := make(chan ssh.Signal, 8)
	sess.Signals(sigCh)

	var sigProc atomic.Pointer[os.Process]
	go func() {
		for {
			select {
			case sig := <-sigCh:
				if proc := sigProc.Load(); proc != nil {
					if osSig := sshSignalToOSSig(sig); osSig != nil {
						_ = proc.Signal(osSig)
					}
				}
			case <-sess.Context().Done():
				return
			}
		}
	}()

	// The PTY info comes from stashPTY rather than sess.Pty(): the request loop
	// writes sess.pty.Window for every window-change without holding the session
	// lock, so reading that struct here would race it. Window changes are applied
	// by drainWindowChanges plus the library's own drain goroutine.
	ps, hasPty := takePTY(sess)
	if !startProcess(cmd, sess, ps, hasPty) {
		return
	}

	sigProc.Store(cmd.Process)

	// Wait for the child, but never let a dead connection leave it running.
	//
	// cmd.Wait() blocks until the CHILD exits, and nothing else notices when the
	// client disappears: a dropped connection (server shutdown, a crashed agent, a
	// closed socket) left the shell running as an orphan with this handler blocked
	// on it forever — one leaked goroutine and one leaked process per dropped
	// session, and on Windows a held PTY that later teardowns then fail on. So the
	// wait is raced against the session context, which the library cancels when the
	// connection goes away, and the child is killed if the connection lost.
	waitDone := make(chan struct{})
	go func() {
		cmd.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-sess.Context().Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitDone
	}

	exitCode := 127
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	sess.Exit(exitCode)
}

// startProcess launches cmd on the session and reports whether it started. On
// failure it has already written the reason to the client and ended the session
// with exit code 1, so the caller's only job is to return.
//
// The two modes are genuinely different, which is why they live together here
// rather than in the caller: with a PTY the process owns the terminal, and
// without one the SSH stream has to be wired up by hand.
func startProcess(cmd *exec.Cmd, sess ssh.Session, ps *sessionPTY, hasPty bool) bool {
	if hasPty {
		setPtySysProcAttr(cmd)
		cmd.Env = append(os.Environ(), "TERM="+ps.pty.Term)
		// Serialized with the library's PTY teardown: see sessionPTY.
		ps.mu.Lock()
		startErr := ps.pty.Start(cmd)
		ps.mu.Unlock()
		if startErr != nil {
			io.WriteString(sess, startErr.Error()+"\n")
			sess.Exit(1)
			return false
		}
		return true
	}

	// Pipe mode (no TTY). Use StdinPipe so exec.Cmd does not spawn a stdin copy
	// goroutine that cmd.Wait() would block on forever. We copy the SSH stream to
	// the process stdin in our own goroutine, which unblocks only on client EOF -
	// independent of cmd.Wait(). Without this, any non-interactive command (echo,
	// ls, ...) deadlocks: cmd.Wait() waits for the stdin copy to finish, which
	// waits on sess.Read(), which never returns because the channel only closes
	// after sess.Exit() below (which never runs).
	in, err := cmd.StdinPipe()
	if err != nil {
		io.WriteString(sess, err.Error()+"\n")
		sess.Exit(1)
		return false
	}
	cmd.Stdout = sess
	cmd.Stderr = sess.Stderr()
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		io.WriteString(sess, err.Error()+"\n")
		sess.Exit(1)
		return false
	}
	go func() {
		_, _ = io.Copy(in, sess)
		in.Close()
	}()
	return true
}
