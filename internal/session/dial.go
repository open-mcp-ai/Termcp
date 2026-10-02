package session

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"golang.org/x/crypto/ssh"
)

// Lock ordering: shellStateMu -> mu -> stdinMu (and shellStateMu -> cs.mu).
// Never acquire in reverse order. s.mu and cs.mu are leaves; shellStateMu is
// only taken by session-level transitions (close/DEAD/terminate, new shells).
// The manager-assigned callbacks and the per-shell closed flag are atomics and
// need no lock (see field docs below).

// RemoteSSH selects a user-supplied SSH server instead of the built-in internal one.
// Jump, when non-nil, is a bastion (ProxyJump): the SSH connection to this host
// is tunneled through a direct-tcpip channel opened on the bastion's client.
// Jump chains recursively (Jump.Jump) for multi-hop.
type RemoteSSH struct {
	Host               string
	Port               int
	User               string
	Password           string
	PrivateKey         string
	KeyPassphrase      string
	TrustUnknownHost   bool
	KnownHosts         string
	DialTimeoutSeconds int
	Proxy              *sshclient.Proxy
	Jump               *RemoteSSH
}

// isRemote reports whether cfg selects a user-supplied SSH server instead of
// the built-in internal one. Single source of truth used by New and the
// create-failure logger in Manager.Create.
func isRemote(cfg Config) bool {
	return cfg.Remote != nil && strings.TrimSpace(cfg.Remote.Host) != ""
}

// remoteDialAddr returns host:port for a remote, defaulting port 22.
func remoteDialAddr(r *RemoteSSH) string {
	port := r.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(strings.TrimSpace(r.Host), strconv.Itoa(port))
}

// remoteDialTimeout clamps DialTimeoutSeconds to [30, 120] seconds.
func remoteDialTimeout(r *RemoteSSH) time.Duration {
	toSec := r.DialTimeoutSeconds
	if toSec <= 0 {
		toSec = 30
	}
	if toSec > 120 {
		toSec = 120
	}
	return time.Duration(toSec) * time.Second
}

// remoteClientConfig builds the per-hop SSH client config.
func remoteClientConfig(r *RemoteSSH) (*ssh.ClientConfig, error) {
	return sshclient.BuildClientConfig(sshclient.DialAuth{
		User:              strings.TrimSpace(r.User),
		Password:          r.Password,
		PrivateKey:        r.PrivateKey,
		KeyPassphrase:     r.KeyPassphrase,
		TrustUnknownHost:  r.TrustUnknownHost,
		KnownHostsContent: r.KnownHosts,
		DialTimeout:       remoteDialTimeout(r),
	})
}

// buildChainClient establishes the SSH client for r, recursing through r.Jump
// bastions (ProxyJump). The bastion's *ssh.Client.Dial opens a direct-tcpip
// channel to the next hop; the SSH handshake to each hop runs over that channel.
//
// Returns the final target client plus all intermediate bastion clients (closers)
// that must stay alive for the life of the session. On error, everything opened
// is cleaned up.
//
// Per-hop host-key verification happens locally at termcp; bastions only relay TCP.
// r.Proxy (socks5) only applies at the chain root (the deepest hop, dialed directly);
// non-root hops get their connection from the parent bastion's Dial, so their
// Proxy is ignored.
func buildChainClient(r *RemoteSSH) (*ssh.Client, []io.Closer, error) {
	addr := remoteDialAddr(r)
	cfg, err := remoteClientConfig(r)
	if err != nil {
		return nil, nil, err
	}

	if r.Jump == nil {
		conn, err := sshclient.DialConn(addr, r.Proxy, cfg.Timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh dial %s: %w", addr, err)
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
		if err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
		}
		return ssh.NewClient(c, chans, reqs), nil, nil
	}

	bastion, subClosers, err := buildChainClient(r.Jump)
	if err != nil {
		return nil, nil, err
	}
	conn, err := bastion.Dial("tcp", addr)
	if err != nil {
		bastion.Close()
		sshclient.DrainClosers(subClosers)
		return nil, nil, fmt.Errorf("bastion dial %s: %w", addr, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		bastion.Close()
		sshclient.DrainClosers(subClosers)
		return nil, nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	closers := append(subClosers, io.Closer(bastion))
	return ssh.NewClient(c, chans, reqs), closers, nil
}
