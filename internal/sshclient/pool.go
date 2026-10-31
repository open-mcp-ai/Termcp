package sshclient

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	poolRefMask = uint64(1<<32 - 1)
	poolGenMask = uint64(1<<31 - 1)
	poolClosed  = uint64(1) << 63
)

// DialFunc establishes a new SSH client for one pool key.
type DialFunc func(ctx context.Context) (*ssh.Client, []io.Closer, error)

// TransportPool reuses SSH clients while at least one lease is held, then keeps
// an idle client warm for a bounded linger period. Keys must identify the full
// effective SSH configuration, not just a user-visible profile name.
type TransportPool struct {
	entries sync.Map // key -> *poolEntry
	linger  time.Duration
}

type poolEntry struct {
	key     string
	pool    *TransportPool
	ready   chan struct{}
	client  *ssh.Client
	closers []io.Closer
	dialErr error         // published by closing ready
	state   atomic.Uint64 // closed bit | idle-generation | live reference count
	close   sync.Once
}

// NewTransportPool creates a pool with the given idle linger. Non-positive
// values select the default 15 seconds.
func NewTransportPool(linger time.Duration) *TransportPool {
	if linger <= 0 {
		linger = 15 * time.Second
	}
	return &TransportPool{linger: linger}
}

// Acquire returns a client lease. Every successful call must invoke release
// exactly once; the returned release function is idempotent.
func (p *TransportPool) Acquire(ctx context.Context, key string, dial DialFunc) (*ssh.Client, func(), error) {
	if key == "" {
		return nil, nil, errors.New("empty SSH transport pool key")
	}
	if dial == nil {
		return nil, nil, errors.New("nil SSH transport dialer")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		if value, ok := p.entries.Load(key); ok {
			entry := value.(*poolEntry)
			select {
			case <-entry.ready:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			if entry.dialErr != nil {
				p.entries.CompareAndDelete(key, entry)
				return nil, nil, entry.dialErr
			}
			if !entry.acquire() {
				p.entries.CompareAndDelete(key, entry)
				continue
			}
			return entry.client, entry.lease(), nil
		}

		entry := &poolEntry{key: key, pool: p, ready: make(chan struct{})}
		entry.state.Store(1) // the creator owns the first lease
		if _, loaded := p.entries.LoadOrStore(key, entry); loaded {
			continue
		}

		client, closers, err := dial(ctx)
		entry.client, entry.closers, entry.dialErr = client, closers, err
		close(entry.ready)
		if err != nil {
			p.entries.CompareAndDelete(key, entry)
			entry.closeTransport()
			return nil, nil, err
		}
		if entry.state.Load()&poolClosed != 0 {
			entry.closeTransport()
			return nil, nil, errors.New("SSH transport pool closed during dial")
		}
		if err := ctx.Err(); err != nil {
			entry.lease()()
			return nil, nil, err
		}
		go entry.watchConnection()
		return client, entry.lease(), nil
	}
}

func (e *poolEntry) acquire() bool {
	for {
		state := e.state.Load()
		if state&poolClosed != 0 || state&poolRefMask == poolRefMask {
			return false
		}
		if e.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

func (e *poolEntry) lease() func() {
	var once sync.Once
	return func() { once.Do(e.release) }
}

func (e *poolEntry) release() {
	for {
		state := e.state.Load()
		refs := state & poolRefMask
		if refs == 0 || state&poolClosed != 0 {
			return
		}
		next := state - 1
		if refs == 1 {
			gen := ((state >> 32) + 1) & poolGenMask
			next = gen << 32
		}
		if e.state.CompareAndSwap(state, next) {
			if refs == 1 {
				generation := next &^ poolClosed
				time.AfterFunc(e.pool.linger, func() { e.expire(generation) })
			}
			return
		}
	}
}

// expire closes only the exact idle generation that scheduled this timer. A
// timer from an earlier zero-reference period cannot close a transport that was
// reacquired and released again in the meantime.
func (e *poolEntry) expire(generation uint64) {
	if !e.state.CompareAndSwap(generation, generation|poolClosed) {
		return
	}
	e.pool.entries.CompareAndDelete(e.key, e)
	e.closeTransport()
}

func (e *poolEntry) watchConnection() {
	if e.client == nil || e.client.Conn == nil {
		return
	}
	_ = e.client.Conn.Wait()
	for {
		state := e.state.Load()
		if state&poolClosed != 0 || e.state.CompareAndSwap(state, state|poolClosed) {
			break
		}
	}
	e.pool.entries.CompareAndDelete(e.key, e)
	e.closeTransport()
}

func (e *poolEntry) closeTransport() {
	// SSH Close and proxy teardown are outside all pool coordination. A peer that
	// stopped reading must not hold up another borrow or daemon shutdown.
	e.close.Do(func() {
		go func() {
			if e.client != nil && e.client.Conn != nil {
				_ = e.client.Close()
			}
			DrainClosers(e.closers)
		}()
	})
}

// CloseAll invalidates every entry and starts its transport teardown. A dial
// still in flight observes the closed bit when it finishes and closes its own
// result, so shutdown never waits for a network handshake.
func (p *TransportPool) CloseAll() {
	p.entries.Range(func(key, value any) bool {
		entry := value.(*poolEntry)
		for {
			state := entry.state.Load()
			if state&poolClosed != 0 || entry.state.CompareAndSwap(state, state|poolClosed) {
				break
			}
		}
		p.entries.CompareAndDelete(key, entry)
		select {
		case <-entry.ready:
			entry.closeTransport()
		default:
			// The dialing goroutine owns the result until ready closes.
		}
		return true
	})
}
