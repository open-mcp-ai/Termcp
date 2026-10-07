package sshclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecSession wraps an active SSH client session.
type ExecSession struct {
	client       *ssh.Client
	session      *ssh.Session
	Stdin        io.WriteCloser
	Stdout       io.Reader
	Stderr       io.Reader
	done         chan struct{}
	exitCode     int
	err          error
	ownClient    bool // if false, Close() does not close the underlying SSH client
	extraClosers []io.Closer
	// stdinMu serializes stdin writes with stdin teardown. x/crypto's channel
	// does not tolerate a concurrent Write and CloseWrite (they race on the
	// channel's EOF flag), and teardown can happen at any time.
	stdinMu sync.Mutex
}

func closeIfCloser(r io.Reader) {
	if c, ok := r.(io.Closer); ok {
		c.Close()
	}
}

// watchCancel closes conn as soon as ctx is done, so a dial that the caller has
// given up on stops occupying a socket (and, on the other side, a slot). It
// returns a stop function that must be called once the connection is no longer
// cancellable, otherwise the watcher outlives its purpose holding a reference.
//
// Closing the conn rather than merely returning early is what makes the cancel
// reach the transport: the SSH handshake is blocked in a read on this net.Conn,
// and none of the x/crypto calls below take a context, so a closed socket is the
// only signal that gets through. The resulting error is the caller's "context
// canceled" to translate; this function never reports one itself.
func watchCancel(ctx context.Context, conn net.Conn) func() {
	if ctx == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Unblocks whatever read/write is in flight; the pending error is what
			// the dial then reports.
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// canceledErr converts the transport error a canceled dial produced into a
// context error, so callers can recognise a user-initiated cancel rather than
// reading it as a network fault of the target host.
func canceledErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// DrainClosers closes closers in reverse order, ignoring errors. Used on error
// paths and session teardown for bastion chains.
func DrainClosers(closers []io.Closer) {
	for i := len(closers) - 1; i >= 0; i-- {
		if closers[i] != nil {
			closers[i].Close()
		}
	}
}

// setTCPKeepAlive enables kernel-level TCP probes so a genuinely severed link
// eventually surfaces as an SSH transport error. Multiple unanswered probes are
// required before the kernel drops the socket, avoiding the false positives of
// an application-level SSH request/timeout watchdog. Internal in-memory
// connections are silently left unchanged.
func setTCPKeepAlive(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcp.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     30 * time.Second,
		Interval: 15 * time.Second,
		Count:    4,
	})
}

// Handshake completes the SSH handshake over an already-open conn, returning a
// client ready to open channels. ctx, when non-nil, aborts the handshake by
// closing conn — the handshake has no context of its own, so a closed socket is
// the only signal that gets through. On success the connection is owned by the
// returned client and ctx is no longer consulted.
//
// Exported for chain builders, which own their net.Conn (a bastion's
// direct-tcpip channel) and so cannot hand it to StartWithConfig.
func Handshake(ctx context.Context, conn net.Conn, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	stop := watchCancel(ctx, conn)
	defer stop()
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, canceledErr(ctx, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// StartWithConfig dials addr with the given SSH client config and starts a command.
// If proxy is non-nil and enabled, the SSH connection is tunneled through a SOCKS5 proxy.
//
// ctx, when non-nil, cancels the dial: the TCP connect honours it natively, and a
// watcher closes the socket during the SSH handshake, which takes no context of
// its own. Cancellation is dropped once the transport is established — the caller
// owns the connection from then on, and its request context (an HTTP handler's,
// say) is canceled as soon as the response is written.
func StartWithConfig(ctx context.Context, addr string, config *ssh.ClientConfig, proxy *Proxy, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	if config == nil {
		return nil, fmt.Errorf("nil ssh ClientConfig")
	}
	conn, err := DialConn(ctx, addr, proxy, config.Timeout)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	client, err := Handshake(ctx, conn, addr, config)
	if err != nil {
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("new session: %w", err)
	}

	return startSession(client, session, command, args, pty, rows, cols, true)
}

// DialConn opens the underlying TCP connection to addr, optionally via a SOCKS5 proxy.
// Exported so chain builders in other packages can reuse the direct/proxy dial path.
// ctx, when non-nil, aborts the connect (and the proxy handshake) with ctx's error.
func DialConn(ctx context.Context, addr string, proxy *Proxy, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	if proxy != nil && proxy.Enabled() {
		return dialProxy(ctx, proxy, addr, timeout)
	}
	var conn net.Conn
	var err error
	if ctx != nil {
		conn, err = (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = net.DialTimeout("tcp", addr, timeout)
	}
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Annotate with target and timeout so a bare "i/o timeout" / "connectex ..."
		// failure tells the user how long it waited and to where.
		return nil, fmt.Errorf("connect %s (timeout %s): %w", addr, timeout, err)
	}
	setTCPKeepAlive(conn)
	return conn, nil
}

// StartWithConn creates an SSH client over an existing net.Conn (e.g. net.Pipe)
// and starts a command. Used for in-process connections without TCP.
// ctx, when non-nil, cancels the handshake by closing conn.
func StartWithConn(ctx context.Context, conn net.Conn, config *ssh.ClientConfig, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	if config == nil {
		return nil, fmt.Errorf("nil ssh ClientConfig")
	}
	client, err := Handshake(ctx, conn, "inmem", config)
	if err != nil {
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("new session: %w", err)
	}

	return startSession(client, session, command, args, pty, rows, cols, true)
}

// StartWithClient creates a new ExecSession on an existing SSH client without dialing.
// The returned ExecSession has ownClient=false; Close() will not close the shared client.
func StartWithClient(client *ssh.Client, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	if client == nil {
		return nil, fmt.Errorf("nil ssh Client")
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	return startSession(client, session, command, args, pty, rows, cols, false)
}

// StartWithChain creates an ExecSession on a freshly-built SSH client (the chain
// target) plus any intermediate bastion clients. The target client and all
// intermediates are closed when the ExecSession closes. Used for ProxyJump/via
// chains where the underlying *ssh.Client was established over a bastion's
// direct-tcpip channel.
// ctx, when non-nil, cancels the channel open and the command start.
func StartWithChain(ctx context.Context, client *ssh.Client, closers []io.Closer, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	if client == nil {
		DrainClosers(closers)
		return nil, fmt.Errorf("nil ssh Client")
	}
	if ctx != nil && ctx.Err() != nil {
		DrainClosers(closers)
		return nil, ctx.Err()
	}
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		DrainClosers(closers)
		return nil, fmt.Errorf("new session: %w", err)
	}
	es, err := startSession(client, session, command, args, pty, rows, cols, true)
	if err != nil {
		DrainClosers(closers)
		return nil, err
	}
	es.extraClosers = closers
	return es, nil
}

// CloseReaders closes the stdout/stderr read sides so a reader goroutine stuck
// on a transport that never reports EOF can be released. Used after the process
// has exited, where the stream can no longer carry useful data.
func (es *ExecSession) CloseReaders() {
	closeIfCloser(es.Stdout)
	closeIfCloser(es.Stderr)
}

// Done returns a channel that closes when the remote process exits.
func (es *ExecSession) Done() <-chan struct{} {
	return es.done
}

// ExitCode returns the process exit code after Done is closed.
func (es *ExecSession) ExitCode() int {
	return es.exitCode
}

// Aborted returns true if the session ended due to connection loss rather than a clean process exit.
func (es *ExecSession) Aborted() bool {
	return es.err != nil && !isExitError(es.err)
}

func isExitError(err error) bool {
	_, ok := err.(*ssh.ExitError)
	return ok
}

// ResizePty sends a window-change request for the session.
func (es *ExecSession) ResizePty(rows, cols int) error {
	return es.session.WindowChange(rows, cols)
}

// WriteStdin writes to the process stdin. Safe to race with Close/termination:
// the write is serialized with the stdin teardown.
func (es *ExecSession) WriteStdin(data []byte) (int, error) {
	es.stdinMu.Lock()
	defer es.stdinMu.Unlock()
	return es.Stdin.Write(data)
}

// Signal sends a signal to the remote process.
func (es *ExecSession) Signal(sig ssh.Signal) error {
	return es.session.Signal(sig)
}

// closeStdin closes the stdin pipe, serialized with in-flight writes.
func (es *ExecSession) closeStdin() {
	es.stdinMu.Lock()
	defer es.stdinMu.Unlock()
	_ = es.Stdin.Close()
}

// Close forcefully terminates the session and underlying connection.
// SSHClient returns the underlying SSH client, or nil for internal sessions.
func (es *ExecSession) SSHClient() *ssh.Client { return es.client }

func (es *ExecSession) Close() error {
	es.closeStdin()
	var firstErr error
	if err := es.session.Close(); err != nil {
		firstErr = err
	}
	if es.ownClient {
		if err := es.client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	DrainClosers(es.extraClosers)
	es.extraClosers = nil
	return firstErr
}

// CloseSessionOnly closes this session channel without touching the shared SSH client.
func (es *ExecSession) CloseSessionOnly() error {
	es.closeStdin()
	return es.session.Close()
}

func shellQuote(command string, args []string) string {
	parts := []string{quoteIfNeeded(command)}
	for _, a := range args {
		parts = append(parts, quoteIfNeeded(a))
	}
	return strings.Join(parts, " ")
}

func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, " \t\n\"'\\$|&;<>(){}[]*?#~`") {
		return strconv.Quote(s)
	}
	return s
}

// defaultPTYModes returns standard terminal modes for PTY sessions.
func defaultPTYModes() ssh.TerminalModes {
	return ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ICRNL:         1,
		ssh.ONLCR:         1,
		ssh.OPOST:         1,
		ssh.ISIG:          1,
		ssh.ICANON:        1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
}
