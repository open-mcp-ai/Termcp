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
// The ctx is monitored; when cancelled all active connections are closed.
// Returns a local listener and a cancel function for lifecycle management.
func LocalForwardSSH(ctx context.Context, client *ssh.Client, remoteHost string, remotePort int, localPort int) (*ForwardInfo, net.Listener, error) {
	if localPort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		localPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}

	localListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return nil, nil, err
	}

	actualPort := localListener.Addr().(*net.TCPAddr).Port
	target := net.JoinHostPort(remoteHost, fmt.Sprintf("%d", remotePort))

	fwID := ForwardID(DirectionLocal)
	fw := &ForwardInfo{
		ForwardID:  fwID,
		Direction:  DirectionLocal,
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", actualPort),
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
	listener, err := client.Listen("tcp", fmt.Sprintf("%s:%d", agentHost, agentPort))
	if err != nil {
		return nil, nil, fmt.Errorf("ssh local forward: %w", err)
	}

	fwID := ForwardID(DirectionRemote)
	fw := &ForwardInfo{
		ForwardID:  fwID,
		Direction:  DirectionRemote,
		ListenAddr: fmt.Sprintf("%s:%d", agentHost, agentPort),
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
// The ctx is used to stop the SOCKS5 server and close active connections.
// Returns a listener for lifecycle management.
func DynamicForwardSSH(ctx context.Context, client *ssh.Client, localPort int) (*ForwardInfo, net.Listener, error) {
	if localPort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		localPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return nil, nil, err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	slog.Info("SOCKS5 listener started", "addr", ln.Addr().String(), "port", actualPort)

	fwID := ForwardID(DirectionDynamic)
	fw := &ForwardInfo{
		ForwardID:  fwID,
		Direction:  DirectionDynamic,
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", actualPort),
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
func (fm *ForwardManager) dynamicLocal(sessionID, sshConfig string, localPort int) (*ForwardInfo, error) {
	if localPort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		localPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
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
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", actualPort),
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
