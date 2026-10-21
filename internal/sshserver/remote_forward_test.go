package sshserver

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/forward"
)

// TestRemoteForwardOnInternalEndpoint locks the wiring that makes `ssh -R` work
// against the built-in in-memory sshd.
//
// It exists because two separate omissions made the feature fail in a way that
// reads as somebody else's policy: charmbracelet/ssh ships
// ForwardedTCPHandler but puts nothing in DefaultRequestHandlers, and its
// ReversePortForwardingCallback denies everything when nil. With neither in
// place the client gets "tcpip-forward request denied by peer", which the docs
// used to explain as the target's GatewayPorts setting — the feature was never
// wired, and nothing said so.
//
// The shape matters: the assertion is that traffic entering the port the TARGET
// opened comes back out of the service, which only holds if the library's
// forwarded-tcpip channel is really being used.
func TestRemoteForwardOnInternalEndpoint(t *testing.T) {
	s := New()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// The service this forward exists to publish.
	svc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	go func() {
		for {
			c, err := svc.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "svc-answer")
			c.Close()
		}
	}()
	svcPort := svc.Addr().(*net.TCPAddr).Port

	cli := dialServer(t, s, mintCfg(t, s))
	defer cli.Close()

	// The port the target will bind is chosen here because 0 is rejected for -R
	// (it is the number the TARGET binds, so the caller has to pick it).
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	fm := forward.NewForwardManager()
	fw, err := fm.CreateRemote("sid", "internal", "127.0.0.1", rPort, "127.0.0.1", svcPort, cli)
	if err != nil {
		t.Fatalf("remote forward on the internal endpoint: %v", err)
	}
	defer fm.Close(fw.ForwardID)

	// The reported address must be the one the listener holds, not the request:
	// for port 0 the request names nothing dialable.
	if fw.ListenAddr != net.JoinHostPort("127.0.0.1", strconv.Itoa(rPort)) {
		t.Errorf("ListenAddr = %q, want %s", fw.ListenAddr, net.JoinHostPort("127.0.0.1", strconv.Itoa(rPort)))
	}

	nc, err := net.DialTimeout("tcp", fw.ListenAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the -R port %q: %v", fw.ListenAddr, err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	n, err := nc.Read(buf)
	if err != nil {
		t.Fatalf("read through the -R tunnel: %v", err)
	}
	if string(buf[:n]) != "svc-answer" {
		t.Fatalf("got %q through the tunnel, want %q", buf[:n], "svc-answer")
	}
}

// TestRemoteForwardWildcardAllowed pins the boundary where it actually is.
//
// An earlier version of this file asserted the opposite -- that the server refuses
// a non-loopback bind because the address arrives in the client's request. That
// reasoning does not survive contact with the transport: Server.Dial is the only
// way in and production calls it from exactly one place with a one-time credential
// this process minted, so the "client" asking to listen IS termcp. A guard there
// would defend a boundary that does not exist, and would do it silently -- the
// caller reads the refusal as the target sshd's policy.
//
// So the address is free here and the decision lives where it can be seen: the
// forward.open capability queues the request and the review prompt names the
// address (see summarizeOperation). This test locks in that the transport does not
// second-guess a bind the reviewed layer already allowed.
func TestRemoteForwardWildcardAllowed(t *testing.T) {
	s := New()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	cli := dialServer(t, s, mintCfg(t, s))
	defer cli.Close()

	// 0.0.0.0 is the address a published tunnel uses; it must reach a real
	// listener rather than a denial. The port is released immediately: what is
	// under test is that the request is GRANTED, not what is served on it.
	ln, err := cli.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("the server refused a wildcard reverse-forward bind: %v", err)
	}
	defer ln.Close()
	if got := ln.Addr().String(); got == "" || strings.HasSuffix(got, ":0") {
		t.Errorf("listener reports %q, want the bound address", got)
	}
}

// TestRemoteForwardLoopbackAllowed is the companion case: the ordinary bind must
// keep working, and it would pass even if the handler were missing entirely --
// which is why the wildcard case above exists.
func TestRemoteForwardLoopbackAllowed(t *testing.T) {
	s := New()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	cli := dialServer(t, s, mintCfg(t, s))
	defer cli.Close()

	ln, err := cli.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback reverse forward refused: %v", err)
	}
	defer ln.Close()
	if got := ln.Addr().String(); got == "" || got == "127.0.0.1:0" {
		t.Errorf("listener reports %q, want the allocated address", got)
	}
}
