package forward

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

// TestResolveBindHost pins the mapping from a requested address to the one
// net.Listen gets. The empty and "*" cases are the ones that matter: Go reads
// ":8080" as every interface, so treating an empty host as "just pass it on"
// would publish a tunnel nobody asked to publish.
func TestResolveBindHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "127.0.0.1"},
		{"   ", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1"},
		{"0.0.0.0", "0.0.0.0"},
		{"*", "0.0.0.0"},
		{" 0.0.0.0 ", "0.0.0.0"},
		{"::1", "::1"},
		{"localhost", "localhost"},
	} {
		if got := ResolveBindHost(tc.in); got != tc.want {
			t.Errorf("ResolveBindHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIsLoopbackHost pins the guard on the dynamic (SOCKS5) forward, which is the
// only forward whose destination is not fixed in advance and so cannot be reviewed
// by where it points.
func TestIsLoopbackHost(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"127.0.0.1", true},
		{"127.0.0.53", true},
		{"::1", true},
		{"[::1]", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"0.0.0.0", false},
		{"192.168.1.10", false},
		{"::", false},
		{"example.com", false},
	} {
		if got := IsLoopbackHost(tc.in); got != tc.want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestLocalForwardBindsRequestedHost is the check that bindHost reaches
// net.Listen. It asserts on the listener and the reported address rather than
// dialling: the accept loop dials through the SSH client, which a bind test has
// no reason to stand up.
func TestLocalForwardBindsRequestedHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fw, ln, err := LocalForwardSSH(ctx, nil, "127.0.0.1", "127.0.0.1", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if got := ln.Addr().String(); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("listener bound %q, want 127.0.0.1", got)
	}
	// Port 0 in the request must be reported as the port the kernel picked, not
	// as the 0 that was asked for.
	if !strings.HasPrefix(fw.ListenAddr, "127.0.0.1:") || strings.HasSuffix(fw.ListenAddr, ":0") {
		t.Errorf("ListenAddr = %q, want 127.0.0.1:<allocated port>", fw.ListenAddr)
	}
	if fw.ListenAddr != ln.Addr().String() {
		t.Errorf("ListenAddr = %q, listener = %q; they must agree", fw.ListenAddr, ln.Addr())
	}
}

// TestLocalForwardReportsRequestedHostNotExpansion keeps an explicit wildcard
// bind reported as what the caller asked for: the kernel expands 0.0.0.0 into
// nothing printable, and a forward created for the network must not list itself
// as a loopback tunnel.
func TestLocalForwardReportsRequestedHostNotExpansion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fw, ln, err := LocalForwardSSH(ctx, nil, "0.0.0.0", "127.0.0.1", 1, 0)
	if err != nil {
		t.Skipf("binding 0.0.0.0 is not permitted here: %v", err)
	}
	defer ln.Close()
	if !strings.HasPrefix(fw.ListenAddr, "0.0.0.0:") {
		t.Errorf("ListenAddr = %q, want the 0.0.0.0 form the caller asked for", fw.ListenAddr)
	}
}

// TestCreateDynamicRefusesNonLoopback pins the one guard that keeps the
// unauthenticated SOCKS5 proxy off the network. No SSH client is needed: the
// address is refused before anything is dialled or bound.
func TestCreateDynamicRefusesNonLoopback(t *testing.T) {
	fm := NewForwardManager()
	for _, host := range []string{"0.0.0.0", "*", "192.168.1.10", "::"} {
		_, err := fm.CreateDynamic("sid", "internal", host, 0, nil, true)
		if err == nil {
			t.Fatalf("CreateDynamic(bind %q) was allowed; the proxy has no authentication", host)
		}
		if !strings.Contains(err.Error(), "open proxy") {
			t.Errorf("CreateDynamic(bind %q) error = %v, want it to name the open-proxy risk", host, err)
		}
	}
	if got := len(fm.List()); got != 0 {
		t.Errorf("%d forwards registered by refused calls, want 0", got)
	}
}

// TestCreateDynamicLoopbackStillWorks is the other half: the refusal above must
// not have closed the ordinary path.
func TestCreateDynamicLoopbackStillWorks(t *testing.T) {
	fm := NewForwardManager()
	fw, err := fm.CreateDynamic("sid", "internal", "", 0, nil, true)
	if err != nil {
		t.Fatalf("CreateDynamic(loopback): %v", err)
	}
	defer fm.Close(fw.ForwardID)
	if !strings.HasPrefix(fw.ListenAddr, "127.0.0.1:") {
		t.Errorf("ListenAddr = %q, want a loopback address", fw.ListenAddr)
	}
}

// TestCreateDynamicRefusalLeavesNoListener is the leak half of the refusal: a
// refused call must not leave a bound listener behind. It is separate because
// the refusal reads an error, while the leak is only visible from the OS.
func TestCreateDynamicRefusalLeavesNoListener(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	fm := NewForwardManager()
	if _, err := fm.CreateDynamic("sid", "internal", "0.0.0.0", port, nil, true); err == nil {
		t.Fatal("non-loopback SOCKS5 bind was allowed")
	}
	// The port must still be free: a refusal that bound first would hold it.
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("port %d was left bound by the refused call: %v", port, err)
	}
	ln.Close()
}
