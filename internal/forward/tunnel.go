package forward

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// tunnel connections back via smux to termcp's remoteHost:remotePort.
// LocalForwardSSH creates a local forward (ssh -L): termcp listens on a local port and
// tunnels each connection through the SSH client's direct-tcpip channel to the remote target.
// bindHost is the local address to listen on; empty means loopback (see
// ResolveBindHost), and a non-loopback address publishes the tunnel to whoever
// can reach it. The ctx is monitored; when cancelled all active connections are closed.
// Returns a local listener and a cancel function for lifecycle management.
func LocalForwardSSH(ctx context.Context, client *ssh.Client, bindHost string, remoteHost string, remotePort int, localPort int) (*ForwardInfo, net.Listener, error) {
	// localPort 0 means "let the kernel pick": bind once and keep it. Probe-then-bind
	// leaves a window for another process to take the port (see dynamicLocal).
	localListener, err := net.Listen("tcp", net.JoinHostPort(ResolveBindHost(bindHost), fmt.Sprintf("%d", localPort)))
	if err != nil {
		return nil, nil, err
	}

	target := net.JoinHostPort(remoteHost, fmt.Sprintf("%d", remotePort))

	fwID := ForwardID(DirectionLocal)
	fw := &ForwardInfo{
		ForwardID: fwID,
		Direction: DirectionLocal,
		// The listener's own address, not the request: the kernel picks a port for
		// localPort 0 and the caller has to be told which one it got. The requested
		// host is kept because an explicit "0.0.0.0" must not be reported back as
		// the wildcard the kernel expanded it to.
		ListenAddr: net.JoinHostPort(ResolveBindHost(bindHost), fmt.Sprintf("%d", localListener.Addr().(*net.TCPAddr).Port)),
		TargetAddr: target,
		Status:     "active",
		CreatedAt:  clock.Now(),
	}

	go func() {
		defer localListener.Close()
		for {
			localConn, err := localListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer localConn.Close()
				remoteConn, err := client.Dial("tcp", target)
				if err != nil {
					slog.Error("ssh forward dial failed", "target", target, "err", err)
					return
				}
				defer remoteConn.Close()
				// Close both sides when the forward is deleted. context.AfterFunc
				// (unlike a `go func(){ <-ctx.Done() }`) leaves no goroutine behind
				// when the connection finishes first — stop() unregisters it.
				stopOnCancel := context.AfterFunc(ctx, func() {
					localConn.Close()
					remoteConn.Close()
				})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { io.Copy(remoteConn, localConn); wg.Done() }()
				go func() { io.Copy(localConn, remoteConn); wg.Done() }()
				wg.Wait()
				stopOnCancel()
			}()
		}
	}()

	return fw, localListener, nil
}

// RemoteForwardSSH creates a remote forward (ssh -R): the remote SSH server listens on
// agentHost:agentPort and tunnels connections back through SSH to termcp's remoteHost:remotePort.
// The ctx is monitored; when cancelled all active connections are closed.
// Returns the local tunnel listener for lifecycle management.
func RemoteForwardSSH(ctx context.Context, client *ssh.Client, agentHost string, agentPort int, remoteHost string, remotePort int) (*ForwardInfo, net.Listener, error) {
	listener, err := client.Listen("tcp", net.JoinHostPort(agentHost, fmt.Sprintf("%d", agentPort)))
	if err != nil {
		return nil, nil, fmt.Errorf("ssh remote forward: %w", err)
	}

	fwID := ForwardID(DirectionRemote)
	fw := &ForwardInfo{
		ForwardID: fwID,
		Direction: DirectionRemote,
		// The listener's address, not the request: the target's sshd answers with
		// the port it actually bound, and for agentPort 0 that is the only place the
		// chosen number appears. Reporting the request left ListenAddr as ":0" -- an
		// address nothing can dial.
		ListenAddr: listener.Addr().String(),
		TargetAddr: fmt.Sprintf("%s:%d", remoteHost, remotePort),
		Status:     "active",
		CreatedAt:  clock.Now(),
	}

	go func() {
		defer listener.Close()
		for {
			agentConn, err := listener.Accept()
			if err != nil {
				return
			}
			remoteConn, err := net.Dial("tcp", net.JoinHostPort(remoteHost, fmt.Sprintf("%d", remotePort)))
			if err != nil {
				agentConn.Close()
				continue
			}
			go func() {
				defer agentConn.Close()
				defer remoteConn.Close()
				// context.AfterFunc leaves nothing behind on normal connection end.
				stopOnCancel := context.AfterFunc(ctx, func() {
					agentConn.Close()
					remoteConn.Close()
				})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { io.Copy(remoteConn, agentConn); wg.Done() }()
				go func() { io.Copy(agentConn, remoteConn); wg.Done() }()
				wg.Wait()
				stopOnCancel()
			}()
		}
	}()

	return fw, listener, nil
}

// DynamicForwardSSH starts a SOCKS5 proxy using an SSH client's dialer.
// bindHost is the local address to listen on; empty means loopback. The SOCKS5
// handshake performs no authentication (matching `ssh -D`), so a non-loopback
// address here is an open proxy on that interface -- callers must refuse it
// rather than pass it through.
// The ctx is used to stop the SOCKS5 server and close active connections.
// Returns a listener for lifecycle management.
func DynamicForwardSSH(ctx context.Context, client *ssh.Client, bindHost string, localPort int) (*ForwardInfo, net.Listener, error) {
	// localPort 0 means "let the kernel pick": bind once and keep it. Probe-then-bind
	// leaves a window for another process to take the port (see dynamicLocal).
	ln, err := net.Listen("tcp", net.JoinHostPort(ResolveBindHost(bindHost), fmt.Sprintf("%d", localPort)))
	if err != nil {
		return nil, nil, err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	slog.Info("SOCKS5 listener started", "addr", ln.Addr().String(), "port", actualPort)

	fwID := ForwardID(DirectionDynamic)
	fw := &ForwardInfo{
		ForwardID:  fwID,
		Direction:  DirectionDynamic,
		ListenAddr: net.JoinHostPort(ResolveBindHost(bindHost), fmt.Sprintf("%d", actualPort)),
		TargetAddr: "SOCKS5",
		Status:     "active",
		CreatedAt:  clock.Now(),
	}
	go serveSOCKS5(ctx, ln, func(target string) (net.Conn, error) {
		conn, err := client.Dial("tcp", target)
		slog.Info("SOCKS5 dial result", "target", target, "err", err)
		return conn, err
	})
	return fw, ln, nil
}

// dynamicLocal starts a SOCKS5 proxy using net.Dial from termcp itself.
func (fm *ForwardManager) dynamicLocal(sessionID, sshConfig, bindHost string, localPort int) (*ForwardInfo, error) {
	// localPort 0 means "let the kernel pick": bind once and keep that listener.
	//
	// The obvious way -- Listen(":0"), read the port, Close, Listen(":N") -- is a
	// race: the port is free when probed and can be taken by anything else before
	// the real bind, which is exactly how this failed on CI
	// ("bind: address already in use"). Holding the first listener removes the
	// window instead of narrowing it, and on Linux the kernel also avoids
	// handing out the same ephemeral port twice while it is held.
	ln, err := net.Listen("tcp", net.JoinHostPort(ResolveBindHost(bindHost), fmt.Sprintf("%d", localPort)))
	if err != nil {
		return nil, err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	slog.Info("SOCKS5 local listener started", "addr", ln.Addr().String())

	fwID := ForwardID(DirectionDynamic)
	ctx, cancel := context.WithCancel(context.Background())
	if sshConfig == "" {
		sshConfig = "internal"
	}
	fw := &ForwardInfo{
		ForwardID:  fwID,
		SessionID:  sessionID,
		Direction:  DirectionDynamic,
		SSHConfig:  sshConfig,
		ListenAddr: net.JoinHostPort(ResolveBindHost(bindHost), fmt.Sprintf("%d", actualPort)),
		TargetAddr: "SOCKS5",
		Status:     "active",
		CreatedAt:  clock.Now(),
	}
	go serveSOCKS5(ctx, ln, func(target string) (net.Conn, error) {
		conn, err := net.Dial("tcp", target)
		slog.Info("SOCKS5 local dial", "target", target, "err", err)
		return conn, err
	})
	fm.put(fw, ln, cancel)
	return fw, nil
}
