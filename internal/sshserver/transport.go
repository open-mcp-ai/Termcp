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
	writeMu   sync.Mutex // serializes close(writeCh) and sends to writeCh
	readCh    <-chan []byte
	writeCh   chan []byte // bidirectional; nil after Close (peer reader gets io.EOF via close)
	closeCh   chan struct{}
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
	case data, ok := <-c.readCh:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, data)
		if n < len(data) {
			c.readBuf = data[n:]
		}
		return n, nil
	case <-c.closeCh:
		return 0, net.ErrClosed
	}
}

func (c *duplexConn) Write(b []byte) (int, error) {
	data := make([]byte, len(b))
	copy(data, b)
	// Hold writeMu across the send so Close cannot close writeCh mid-send
	// (that would be a data race and a "send on closed channel" panic).
	// Close closes closeCh before taking writeMu, so a blocked send here is
	// always released instead of deadlocking the two.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wch := c.writeCh
	if wch == nil {
		return 0, net.ErrClosed
	}
	select {
	case wch <- data:
		return len(b), nil
	case <-c.closeCh:
		return 0, net.ErrClosed
	}
}

func (c *duplexConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closeCh) // unblock local reads/writes
		c.writeMu.Lock()
		close(c.writeCh) // unblock peer's reader (writeCh is always non-nil inside closeOnce)
		c.writeCh = nil
		c.writeMu.Unlock()
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

	a := &duplexConn{readCh: b2a, writeCh: a2b, closeCh: make(chan struct{})}
	b := &duplexConn{readCh: a2b, writeCh: b2a, closeCh: make(chan struct{})}
	return a, b
}

type inMemAddr struct{}

func (inMemAddr) Network() string { return "inmem" }
func (inMemAddr) String() string  { return "inmem" }
