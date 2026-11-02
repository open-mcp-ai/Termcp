package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialSOCKS5 opens a connection to the proxy and completes only the greeting, leaving
// the request unread so each test can send the one it wants to exercise.
func dialSOCKS5(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	return conn
}

// startProxy runs a SOCKS5 proxy whose dialer is supplied by the test, so a failure
// can be produced on demand instead of by hunting for a closed port.
func startProxy(t *testing.T, dialer func(string) (net.Conn, error)) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	go serveSOCKS5(ctx, ln, dialer)
	return ln.Addr().String()
}

// readReply reads the ten-byte CONNECT reply and returns its REP field.
func readReply(t *testing.T, conn net.Conn) byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return reply[1]
}

// TestSOCKS5UnsupportedCommandIsAnswered pins that a request this proxy cannot serve
// gets RFC 1928's "command not supported" instead of a silent hangup. Dropping the
// connection leaves the caller to guess whether the proxy is broken or the request was.
func TestSOCKS5UnsupportedCommandIsAnswered(t *testing.T) {
	addr := startProxy(t, func(string) (net.Conn, error) {
		t.Fatal("dialer must not be reached for an unsupported command")
		return nil, nil
	})

	conn := dialSOCKS5(t, addr)
	defer conn.Close()

	// BIND (command 2): this proxy speaks CONNECT only.
	if _, err := conn.Write([]byte{5, 2, 0, 1, 127, 0, 0, 1, 0, 80}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if rep := readReply(t, conn); rep != 0x07 {
		t.Fatalf("reply code = %#x, want 0x07 (command not supported)", rep)
	}
}

// TestSOCKS5UnsupportedAddressTypeIsAnswered is the same contract for ATYP: RFC 1928
// gives it its own code, and a caller reading 0x01 would go looking for a server fault
// that does not exist.
func TestSOCKS5UnsupportedAddressTypeIsAnswered(t *testing.T) {
	addr := startProxy(t, func(string) (net.Conn, error) {
		t.Fatal("dialer must not be reached for an unsupported address type")
		return nil, nil
	})

	conn := dialSOCKS5(t, addr)
	defer conn.Close()

	// ATYP 9 does not exist.
	if _, err := conn.Write([]byte{5, 1, 0, 9}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if rep := readReply(t, conn); rep != 0x08 {
		t.Fatalf("reply code = %#x, want 0x08 (address type not supported)", rep)
	}
}

// TestSOCKS5DialFailureCarriesTheReason pins that a refused target is reported as
// refused. The general-failure code for everything erases the difference between
// "nothing is listening" and "the proxy itself is down", which is the difference
// between retrying later and fixing the wrong host.
func TestSOCKS5DialFailureCarriesTheReason(t *testing.T) {
	addr := startProxy(t, func(string) (net.Conn, error) {
		return nil, fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED)
	})

	conn := dialSOCKS5(t, addr)
	defer conn.Close()

	if _, err := conn.Write([]byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 80}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if rep := readReply(t, conn); rep != 0x05 {
		t.Fatalf("reply code = %#x, want 0x05 (connection refused)", rep)
	}
}

// TestSOCKS5SSHRejectionKeepsItsMeaning covers the half of the mapping that only
// exists because the dialer is SSH: a channel the far end refuses on policy is not a
// network failure and must not be reported as one.
func TestSOCKS5SSHRejectionKeepsItsMeaning(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want byte
	}{
		{"prohibited", &ssh.OpenChannelError{Reason: ssh.Prohibited}, 0x02},
		{"connect failed", &ssh.OpenChannelError{Reason: ssh.ConnectionFailed}, 0x05},
		{"unknown channel", &ssh.OpenChannelError{Reason: ssh.UnknownChannelType}, 0x07},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := socks5ErrCode(tc.err); got != tc.want {
				t.Fatalf("code = %#x, want %#x", got, tc.want)
			}
		})
	}
}
