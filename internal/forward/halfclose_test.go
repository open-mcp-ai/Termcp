package forward

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// socks5Connect performs the smallest handshake this proxy accepts: no auth, one
// CONNECT to a literal IPv4 address. The address is not what the test is about --
// the dialer stands in for SSH and answers with its own target -- so any value works.
func socks5Connect(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		t.Fatalf("greeting reply = %v, want [5 0]", reply)
	}
	req := []byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 80}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("connect request: %v", err)
	}
	resp := make([]byte, 10)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if resp[1] != 0 {
		t.Fatalf("connect reply code = %d, want 0", resp[1])
	}
}

// TestSOCKS5ForwardPropagatesHalfClose pins that a target which ends its reply by
// closing the connection is seen as finished by the caller.
//
// This is not a corner case. The adb server answers a `shell:` request the same way:
// bytes, then FIN. HTTP/1.0 does it too. Without the half-close the caller's read
// never returns a final EOF, so a finished command is indistinguishable from a hung
// peer -- which is exactly how `mr-robot` came to block forever in ReadUntilEof.
//
// The failure mode is a hang, not a wrong byte, so the test needs its own deadline:
// a regression shows up as a timeout rather than as a mismatched value.
func TestSOCKS5ForwardPropagatesHalfClose(t *testing.T) {
	// The target writes a farewell and hangs up, the way a finished command does.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	defer target.Close()
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("BYE"))
		_ = c.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	go serveSOCKS5(ctx, proxy, func(string) (net.Conn, error) {
		return net.Dial("tcp", target.Addr().String())
	})

	conn, err := net.Dial("tcp", proxy.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	socks5Connect(t, conn)

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("reading the target's reply never reached EOF: %v (read %q)", err, got)
	}
	if string(got) != "BYE" {
		t.Fatalf("read %q, want %q", got, "BYE")
	}
}
