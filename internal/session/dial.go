package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"golang.org/x/crypto/ssh"
)

// Lock ordering: shellStateMu -> mu (and shellStateMu -> cs.mu).
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

// The dial timeout's numbers, and they live here because this is where they are used.
//
// 30 IS THE DEFAULT, NOT A FLOOR, and the two were confused once — a comment here read
// "clamps DialTimeoutSeconds to [30, 120]", which described a floor the code did not have
// (`toSec <= 0 { toSec = 30 }` is a default, and `5` was honoured as 5). Reading that
// comment as the domain turned a fast-fail setting into a refusal, which is a worse failure
// than the silent clamp it was meant to fix: a caller that wants a 5 second dial so a script
// fails fast could no longer say so at all.
//
// RecommendedDialTimeoutSeconds is the same kind of number the window arguments publish: a
// size to stay under, not a wall. A longer dial is honoured — a slow link or a bastion chain
// is a real reason to want one, and this server cannot see the link the caller is on.
const (
	MinDialTimeoutSeconds         = 1
	RecommendedDialTimeoutSeconds = 120
	DefaultDialTimeoutSeconds     = 30
)

// ClampDialTimeoutSeconds resolves a dial timeout: an unset (or negative) value becomes the
// default, and everything else is the caller's number.
//
// It is exported because the config validator asks it instead of restating the rule: a
// second copy drifts, and the copy that drifts is the one not enforcing anything. There is
// deliberately NO upper clamp — a value above RecommendedDialTimeoutSeconds is a request
// this server has no basis to overrule, and capping it silently is the failure mode this
// whole area was fixed for (a profile that said 600 was stored as 600, echoed as 600 and
// dialled at 120).
func ClampDialTimeoutSeconds(seconds int) int {
	if seconds <= 0 {
		return DefaultDialTimeoutSeconds
	}
	return seconds
}

func remoteDialTimeout(r *RemoteSSH) time.Duration {
	return time.Duration(ClampDialTimeoutSeconds(r.DialTimeoutSeconds)) * time.Second
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
// ctx cancels the dial of every hop. Only the root hop's TCP connect honours it
// natively; a later hop's connection comes from the bastion's channel open, which
// cannot be interrupted — but the caller's cancel is checked between hops, so the
// chain stops as soon as a hop boundary is reached instead of dialing on.
//
// Returns the final target client plus all intermediate bastion clients (closers)
// that must stay alive for the life of the session. On error, everything opened
// is cleaned up.
//
// Per-hop host-key verification happens locally at termcp; bastions only relay TCP.
// r.Proxy (socks5) only applies at the chain root (the deepest hop, dialed directly);
// non-root hops get their connection from the parent bastion's Dial, so their
// Proxy is ignored.
func buildChainClient(ctx context.Context, r *RemoteSSH) (*ssh.Client, []io.Closer, error) {
	addr := remoteDialAddr(r)
	cfg, err := remoteClientConfig(r)
	if err != nil {
		return nil, nil, err
	}

	if r.Jump == nil {
		conn, err := sshclient.DialConn(ctx, addr, r.Proxy, cfg.Timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh dial %s: %w", addr, err)
		}
		client, err := sshclient.Handshake(ctx, conn, addr, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
		}
		return client, nil, nil
	}

	bastion, subClosers, err := buildChainClient(ctx, r.Jump)
	if err != nil {
		return nil, nil, err
	}
	conn, err := bastion.Dial("tcp", addr)
	if err != nil {
		bastion.Close()
		sshclient.DrainClosers(subClosers)
		return nil, nil, fmt.Errorf("bastion dial %s: %w", addr, err)
	}
	client, err := sshclient.Handshake(ctx, conn, addr, cfg)
	if err != nil {
		bastion.Close()
		sshclient.DrainClosers(subClosers)
		return nil, nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	closers := append(subClosers, io.Closer(bastion))
	return client, closers, nil
}
