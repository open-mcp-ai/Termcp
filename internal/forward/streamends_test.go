package forward

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeAddr exists only because net.Conn demands an address; nothing reads it.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// scriptedConn replays a fixed set of reads and then reports a chosen error.
// It records which kind of teardown it received, which is the whole point: a stream
// that ended cleanly must be half-closed, and one that broke must be closed outright.
type scriptedConn struct {
	chunks    [][]byte
	readErr   error
	closeRead bool
	closed    bool
	wrote     bytes.Buffer
	mu        sync.Mutex
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if len(c.chunks) > 0 {
		chunk := c.chunks[0]
		c.chunks = c.chunks[1:]
		return copy(p, chunk), nil
	}
	if c.readErr != nil {
		return 0, c.readErr
	}
	return 0, io.EOF
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wrote.Write(p)
}

func (c *scriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *scriptedConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *scriptedConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

// halfCloseSpy adds CloseWrite so the test can tell a half-close from a full close.
type halfCloseSpy struct {
	scriptedConn
	closeWrites int
	mu2         sync.Mutex
}

func (c *halfCloseSpy) CloseWrite() error {
	c.mu2.Lock()
	defer c.mu2.Unlock()
	c.closeWrites++
	return nil
}

// TestProxyBothHalfClosesOnCleanEOF pins the normal ending: the target said everything
// it had and closed, so the caller is told the stream ended without dragging down the
// direction that might still be carrying bytes the other way.
func TestProxyBothHalfClosesOnCleanEOF(t *testing.T) {
	src := &halfCloseSpy{scriptedConn: scriptedConn{chunks: [][]byte{[]byte("hello")}}}
	dst := &halfCloseSpy{}

	proxyBoth(src, dst)

	if got := dst.wrote.String(); got != "hello" {
		t.Fatalf("dst received %q, want %q", got, "hello")
	}
	if dst.closeWrites != 1 {
		t.Fatalf("dst got %d half-closes, want 1", dst.closeWrites)
	}
	if dst.closed {
		t.Fatal("dst was fully closed after a clean end, which tears down the other direction")
	}
}

// TestProxyBothAbortsBrokenStream is the other ending, and the more important one: when
// a direction fails part-way the caller must not be told the stream finished. Half-
// closing there turns a truncated transfer into a silent success.
func TestProxyBothAbortsBrokenStream(t *testing.T) {
	src := &halfCloseSpy{scriptedConn: scriptedConn{
		chunks:  [][]byte{[]byte("partial")},
		readErr: errors.New("connection reset by peer"),
	}}
	dst := &halfCloseSpy{}

	proxyBoth(src, dst)

	if dst.closeWrites != 0 {
		t.Fatalf("dst got %d half-closes after a broken stream, want 0", dst.closeWrites)
	}
	if !dst.closed {
		t.Fatal("dst was not closed after a broken stream; the caller cannot tell it was truncated")
	}
	if !src.closed {
		t.Fatal("src was not closed after a broken stream")
	}
}
