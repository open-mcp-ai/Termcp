package sshclient

import (
	"net"
	"testing"
)

// Hide the in-process capability while carrying all SSH bytes to the same
// hermetic server. A conventional TCP-backed client has exactly this view.
type ordinarySSHConn struct{ net.Conn }

func (ordinarySSHConn) LocalAddr() net.Addr { return &net.TCPAddr{} }

func TestOrdinarySSHHasNoPID(t *testing.T) {
	srv := startTestSSHServer(t)
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	cmd, args := longRunningCommand()
	es, err := StartWithConn(t.Context(), ordinarySSHConn{conn}, mintClientConfig(t, srv), cmd, args, false, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	if es.localPID != nil || es.LocalPID() != 0 {
		t.Fatal("ordinary SSH provided a local process identifier")
	}
}
