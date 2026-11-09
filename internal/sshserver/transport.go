package sshserver

import (
	"io"
	"net"
	"sync"
	"time"
)

// inMemListener is a net.Listener backed by duplex channel pairs for in-process SSH connections.
type inMemListener struct {
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newInMemListener() *inMemListener {
	return &inMemListener{
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
}

func (l *inMemListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *inMemListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *inMemListener) Addr() net.Addr { return inMemAddr{} }

// Dial creates a full-duplex in-memory connection pair (using io.Pipe pairs to avoid
// net.Pipe's synchronous write deadlock during SSH version exchange), enqueues the
// server side, and returns the client side.
func (l *inMemListener) Dial() (net.Conn, error) {
	server, client := duplexPipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		server.Close()
		client.Close()
		return nil, net.ErrClosed
	}
}

// duplexConn is a full-duplex net.Conn backed by buffered byte channels.
// Buffered channels prevent the write-then-read deadlock that occurs during SSH
// version exchange when both sides write before either reads (net.Pipe / io.Pipe
// are synchronous and block writes until the other side reads).
type duplexConn struct {
	readCh  <-chan []byte
	writeCh chan []byte
	// closeCh is closed by Close and is the ONLY thing Close closes. The peer learns
	// about it through peerClose, which is this conn's peer's closeCh — so a reader
	// blocked on readCh is released without anyone closing (or nil-ing) writeCh.
	closeCh   chan struct{}
	peerClose <-chan struct{}
	closeOnce sync.Once
	readBuf   []byte
}

func (c *duplexConn) Read(b []byte) (int, error) {
	if len(c.readBuf) > 0 {
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}
	select {
	case data := <-c.readCh:
		n := copy(b, data)
		if n < len(data) {
			c.readBuf = data[n:]
		}
		return n, nil
	case <-c.peerClose:
		// THE PEER CLOSED: drain what it already sent, then EOF. Reading the buffer
		// first is why this is not a bare EOF — bytes in flight must still arrive.
		select {
		case data := <-c.readCh:
			n := copy(b, data)
			if n < len(data) {
				c.readBuf = data[n:]
			}
			return n, nil
		default:
			return 0, io.EOF
		}
	case <-c.closeCh:
		return 0, net.ErrClosed
	}
}

func (c *duplexConn) Write(b []byte) (int, error) {
	data := make([]byte, len(b))
	copy(data, b)
	// NOBODY EVER CLOSES writeCh, so a send can never panic and needs no lock:
	// Close closes its own closeCh only, and this select is what releases a send
	// that would otherwise block on a peer that is gone.
	select {
	case <-c.closeCh:
		return 0, net.ErrClosed
	case <-c.peerClose:
		return 0, io.ErrClosedPipe
	default:
	}
	select {
	case c.writeCh <- data:
		return len(b), nil
	case <-c.closeCh:
		return 0, net.ErrClosed
	case <-c.peerClose:
		return 0, io.ErrClosedPipe
	}
}

func (c *duplexConn) Close() error {
	c.closeOnce.Do(func() {
		// ONE CHANNEL, ONE CLOSE. The peer's reader watches this through peerClose;
		// writeCh is never closed, which is what makes Write lock-free.
		close(c.closeCh)
	})
	return nil
}
func (c *duplexConn) LocalAddr() net.Addr              { return inMemAddr{} }
func (c *duplexConn) RemoteAddr() net.Addr             { return inMemAddr{} }
func (c *duplexConn) SetDeadline(time.Time) error      { return nil }
func (c *duplexConn) SetReadDeadline(time.Time) error  { return nil }
func (c *duplexConn) SetWriteDeadline(time.Time) error { return nil }

// duplexPipe creates a pair of connected duplexConns using buffered channels.
// The buffered channels (cap 16) allow the SSH version exchange to complete
// without blocking: each side writes ~15-30 bytes which fits in the buffer.
func duplexPipe() (net.Conn, net.Conn) {
	const bufCap = 16
	a2b := make(chan []byte, bufCap)
	b2a := make(chan []byte, bufCap)

	aClose := make(chan struct{})
	bClose := make(chan struct{})
	a := &duplexConn{readCh: b2a, writeCh: a2b, closeCh: aClose, peerClose: bClose}
	b := &duplexConn{readCh: a2b, writeCh: b2a, closeCh: bClose, peerClose: aClose}
	return a, b
}

type inMemAddr struct{}

func (inMemAddr) Network() string { return "inmem" }
func (inMemAddr) String() string  { return "inmem" }
