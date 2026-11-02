package mcp

import (
	"sync"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftp"
)

// sftpEngines keeps one SFTP subsystem alive per session, so a file operation does not
// pay for a new one on every call.
//
// WHY THIS EXISTS (issue #244). Every file handler used to call `sftp.NewClient` and
// close it when the handler returned. That is three round trips for the engine alone —
// the SSH channel open, the subsystem request, and the SFTP INIT/VERSION handshake —
// spent before the operation sends a single byte of its own. Measured against a link
// with a 350 ms round trip: the engine costs 526 ms and an 894-byte upload costs 879 ms,
// so on the small writes the issue reports, opening the pipe was most of the wall clock
// and was pure overhead on top of the protocol's actual work.
//
// WHAT THIS DOES NOT FIX, stated plainly because it is where the issue's own hypothesis
// pointed: the per-operation round trips remain. A whole-file write is OPEN (a temporary
// sibling), WRITE, CLOSE, RENAME — four requests, five measured, because `pkg/sftp` does
// not batch them. They are the price of an atomic write, and an 894-byte body is ALREADY
// sent as one WRITE: the chunking the issue blames never applied below 32 KiB.
// Pipelining is not available either — `UseConcurrentWrites` splits by 32 KiB packet, so
// a sub-packet file has one request to overlap (measured: 1 MiB sequential 5.45 s,
// concurrent 4.94 s, over a 100 ms round trip).
//
// SCOPE. Only a session's own connection is cached, keyed by the canonical session id —
// which is what lets the DEAD hook evict it exactly. A BORROWED host is not cached: its
// transport lease is request-scoped, so caching it would hand a later call a transport
// that has already been released. A borrowed file call still opens and closes its own.
type sftpEngines struct {
	mu sync.Mutex
	m  map[string]*engineEntry
}

// engineEntry is one session's engine, or the attempt to build one. `ready` closes when
// that attempt finishes, so concurrent calls for the same session share a single
// handshake instead of racing two — the single-flight shape TransportPool uses.
type engineEntry struct {
	ready  chan struct{}
	sshCli *ssh.Client
	// client is the SHARED view, built once with the engine. Caching the view rather
	// than the raw engine is what makes "did this call get the same engine" a pointer
	// comparison a caller can actually make — and it is why two file operations on one
	// session are provably one handshake.
	client *sftp.Client
	err    error
}

func newSFTPEngines(m *session.Manager) *sftpEngines {
	e := &sftpEngines{m: make(map[string]*engineEntry)}
	if m != nil {
		// The DEAD hook is what makes the cache bounded: a session that ends takes its
		// engine with it, and a daemon that has run a thousand sessions holds none of
		// their subsystems. It runs without session locks held, which is why this can
		// take its own.
		m.AddOnDeadHook(e.forget)
	}
	return e
}

// get returns the session's engine, opening it once.
//
// The returned client is SHARED and must not be closed by the caller. Callers get a
// view instead (sftp.EngineView), whose Close does nothing: the engine's lifetime is the
// session's, not the call's.
func (e *sftpEngines) get(sessionID string, sshCli *ssh.Client) (*sftp.Client, error) {
	e.mu.Lock()
	if entry := e.m[sessionID]; entry != nil {
		if entry.sshCli != sshCli {
			// The session reconnected: the engine rides a transport that is gone and
			// nothing can revive it, so this call rebuilds rather than reusing.
			delete(e.m, sessionID)
		} else {
			e.mu.Unlock()
			<-entry.ready
			if entry.err != nil {
				// A failed handshake is not worth remembering: the next call gets a
				// fresh attempt instead of a cached error for ever.
				e.mu.Lock()
				if e.m[sessionID] == entry {
					delete(e.m, sessionID)
				}
				e.mu.Unlock()
			}
			return entry.client, entry.err
		}
	}
	entry := &engineEntry{ready: make(chan struct{}), sshCli: sshCli}
	e.m[sessionID] = entry
	e.mu.Unlock()

	// THE HANDSHAKE RUNS WITHOUT THE LOCK HELD. It is three round trips, and holding a
	// mutex across network I/O is the failure this repository has paid for repeatedly
	// (AGENTS.md). Concurrent callers wait on `ready` — bounded by the handshake they
	// would each have paid for anyway.
	engine, err := pkgsftp.NewClient(sshCli)
	entry.client, entry.err = sftp.Shared(engine), err
	close(entry.ready)
	if entry.err != nil {
		e.mu.Lock()
		if e.m[sessionID] == entry {
			delete(e.m, sessionID)
		}
		e.mu.Unlock()
	}
	return entry.client, entry.err
}

// forget drops a session's engine. The SSH transport is closed by the same shutdown and
// the subsystem dies with it; closing the engine here would race a call still using it.
func (e *sftpEngines) forget(sessionID string) {
	e.mu.Lock()
	delete(e.m, sessionID)
	e.mu.Unlock()
}
