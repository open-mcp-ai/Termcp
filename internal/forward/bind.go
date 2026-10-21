package forward

import (
	"net"
	"strings"
)

// defaultBindHost is where a forward listens when the caller names no address.
//
// Loopback rather than all interfaces, matching `ssh -L`/`-D`: a forward carries
// an already-authenticated tunnel, so binding 0.0.0.0 by default would publish
// it to the network without anyone asking. Widening is an explicit act.
const defaultBindHost = "127.0.0.1"

// ResolveBindHost turns a requested bind address into the one net.Listen gets,
// accepting OpenSSH's spellings of "all interfaces" so a value that works in
// `ssh -L` works here too.
//
// An empty host means "the default", not "every interface": Go's ":8080" and
// OpenSSH's "-L :8080" both mean all interfaces, and silently inheriting that
// reading is the one way to widen exposure without writing an address down.
func ResolveBindHost(host string) string {
	switch strings.TrimSpace(host) {
	case "":
		return defaultBindHost
	case "*":
		return "0.0.0.0"
	}
	return strings.TrimSpace(host)
}

// IsLoopbackHost reports whether host names only this machine.
//
// It is the guard on the one forward that has no fixed target: a SOCKS5 proxy
// dials whatever each connection asks for, so it cannot be published and then
// reviewed by its destination the way a local forward is. It is NOT a guard over
// tcpip-forward -- the peer asking the in-memory sshd to bind is this process, so
// that decision belongs to the review queue, not to the transport.
//
// "localhost" is the only name consulted through the resolver; everything else is
// read as a literal, so a DNS answer cannot turn a refusal into a bind.
func IsLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	if h == "" {
		return true // all interfaces by the empty-host rule; the caller decides
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
