// Package sftpengine keeps one SFTP subsystem per session so a file operation does
// not pay for a new one on every call.
package sftpengine

import (
	"sync"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftp"
)

// Pool keeps one SFTP engine per session, keyed by the canonical session id.
//
// WHY THIS EXISTS (issue #244). Every file handler on both entrances — the MCP/REST
// tool surface and the Web UI's file panel — used to call `sftp.NewClient` and close it
// when the handler returned. That pays three round trips before the operation sends a
// single byte of its own: the SSH channel open, the subsystem request, and the SFTP
// INIT/VERSION handshake. Measured against an in-process server with a 350ms round trip
// injected, the engine costs 526ms and an 894-byte upload costs 879ms — so on the small
// writes the issue reports, opening the pipe was most of the wall clock, and it was paid
// again on every call.
//
// WHAT THIS DOES NOT FIX, because the measurement says so rather than the issue's
// hypothesis: the per-operation round trips. A whole-file write is OPEN (a temporary
// sibling), WRITE, CLOSE, RENAME — five requests measured, the price of an atomic
// write. And the chunking the issue blames never applied below 32 KiB: an 894-byte body
// is already ONE WRITE. Pipelining is not available either — UseConcurrentWrites splits
// by 32 KiB packet, so a sub-packet file has one request to overlap (measured: 1 MiB
// sequential 5.45s, concurrent 4.94s, over a 100ms round trip). Bulk transfer over a
// high-latency link belongs on the upload URL or scp, not the control channel.
//
// SCOPE. A `use` target's transport is NOT pooled: its lease is request-scoped, so a
// cached engine would hand a later call a transport that has already been released — a
// correctness bug traded for a latency win. A `use` file call still opens and closes
// its own engine.
type Pool struct {
	mu sync.Mutex
	m  map[string]*entry
}

// entry is one session's engine, or the attempt to build one. `ready` closes when that
// attempt finishes, so concurrent calls for the same session share a single handshake
// instead of racing two — the single-flight shape TransportPool uses.
//
// The view is cached beside the SSH client it rides: handing out the SAME view is what
// makes "did these calls share one engine" a pointer comparison rather than an
// inference from a clock.
type entry struct {
	ready  chan struct{}
	sshCli *ssh.Client
	view   *sftp.Client
	err    error
}

// NewPool returns a pool that evicts a session's engine when the session goes DEAD —
// the same hook forwards and peers use, and what keeps the map bounded: a daemon that
// has served a thousand sessions holds none of their subsystems.
func NewPool(mgr *session.Manager) *Pool {
	p := &Pool{m: make(map[string]*entry)}
	if mgr != nil {
		mgr.AddOnDeadHook(p.Forget)
	}
	return p
}

// Get returns the session's SFTP engine as a shared view, opening it once.
//
// The returned Client MUST NOT be relied on as owned: its Close is a no-op (see
// sftp.Shared), because the engine's lifetime is the session's, not the call's. That is
// deliberate — a dozen call sites already say `defer sftpCli.Close()`, and the dangerous
// version of this change (one caller closing an engine another is using) is made
// impossible to write rather than merely discouraged.
func (p *Pool) Get(sessionID string, sshCli *ssh.Client) (*sftp.Client, error) {
	if p == nil {
		// A surface built without a pool (a Server literal in a test, a Handler wired
		// before main) still works: the pool is a latency optimisation, and an engine per
		// call is correct, only slower. Nil-safe instead of a nil check at every caller,
		// so "remember the guard" is not a rule anyone has to keep.
		return sftp.NewClient(sshCli)
	}
	p.mu.Lock()
	if e := p.m[sessionID]; e != nil {
		if e.sshCli != sshCli {
			// The session reconnected: the engine rides a transport that is gone and
			// nothing can revive it, so this call rebuilds rather than reuses.
			delete(p.m, sessionID)
		} else {
			p.mu.Unlock()
			<-e.ready
			if e.err != nil {
				// A failed handshake is not worth remembering: the next call gets a
				// fresh attempt instead of a cached error for ever.
				p.mu.Lock()
				if p.m[sessionID] == e {
					delete(p.m, sessionID)
				}
				p.mu.Unlock()
			}
			return e.view, e.err
		}
	}
	e := &entry{ready: make(chan struct{}), sshCli: sshCli}
	p.m[sessionID] = e
	p.mu.Unlock()

	// THE HANDSHAKE RUNS WITHOUT THE LOCK HELD. It is three round trips, and holding a
	// mutex across network I/O is the failure this repository has paid for repeatedly
	// (AGENTS.md). Concurrent callers wait on `ready` — bounded by the handshake they
	// would each have paid for anyway.
	engine, err := pkgsftp.NewClient(sshCli)
	e.view, e.err = sftp.Shared(engine), err
	close(e.ready)
	if e.err != nil {
		p.mu.Lock()
		if p.m[sessionID] == e {
			delete(p.m, sessionID)
		}
		p.mu.Unlock()
	}
	return e.view, e.err
}

// Forget drops a session's engine. The SSH transport is closed by the same shutdown and
// the subsystem dies with it; closing the engine here would race a call still using it.
func (p *Pool) Forget(sessionID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.m, sessionID)
	p.mu.Unlock()
}
