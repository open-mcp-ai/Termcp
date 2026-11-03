package mcpbridge

import (
	"context"
	"net"

	"github.com/charmbracelet/ssh"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
)

// ViaDialer turns a profile name into a dialer whose connections leave from that
// host, plus the release that ends the lease when the bridge does.
//
// The lease is held for the bridge's whole lifetime rather than per request: a
// bridge issues many requests over one MCP session, and reacquiring per request
// would pay a handshake each time to reach the same host.
//
// It is a function rather than the session manager so this package stays a relay
// — what a profile means and how its connection is kept warm is not this
// package's business. main supplies it, which is also where the dependency
// becomes visible in one line.
type ViaDialer func(ctx context.Context, profile string) (dial func(network, addr string) (net.Conn, error), release func(), err error)

// RegisterBuiltin teaches an internal SSH server to serve BuiltinCommand
// in-process: a session that asks for it is handed to this relay, and the
// relay's own stdin/stdout/stderr are the SSH channel's. No process is spawned
// and no PATH is consulted, which is the whole reason an extension can reach an
// HTTP MCP server without a second binary to install.
//
// via gives `--via <profile>` its meaning. A nil via refuses the flag rather than
// ignoring it: silently dialling from this machine when the caller asked to dial
// from there is how a template ends up talking to the wrong server.
func RegisterBuiltin(s *sshserver.Server, via ViaDialer) {
	if s == nil {
		return
	}
	s.RegisterCommand(BuiltinCommand, func(ctx context.Context, sess ssh.Session, args []string) int {
		return CommandHandler(ctx, sess, sess, sess.Stderr(), args, via)
	})
}
