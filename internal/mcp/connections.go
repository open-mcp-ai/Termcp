package mcp

import (
	"sort"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Connection is one MCP client currently attached to this instance.
//
// "Attached" is deliberately narrower than "has ever called": mcp-go announces a
// session when a client initializes over either HTTP transport and unregisters it
// when the transport drops, so this is the set of clients holding a live MCP
// session right now — the answer to "who is driving this instance?".
type Connection struct {
	// SessionID is mcp-go's own id for the transport session. Opaque, and not a
	// termcp session id; it is here so two clients with the same name can be
	// told apart.
	SessionID string `json:"session_id"`
	// Name and Version come from the client's `initialize` request (e.g.
	// "claude-ai" / "0.1.0"). Both empty means the client did not say, which the
	// MCP spec allows: clientInfo is optional.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Namespace is the caller's namespace claim, when it made one. Empty is the
	// instance owner, which is the ordinary case.
	Namespace string `json:"namespace,omitempty"`
	// ConnectedAt is when the session was registered, RFC 3339 in UTC.
	ConnectedAt string `json:"connected_at"`
	// seq is the registration order, and it is what the list actually sorts on.
	// ConnectedAt renders whole seconds, so two clients that attach inside the
	// same second compare equal — and a sort with equal keys over a map's
	// iteration order is free to put them either way, which made "oldest first"
	// a coin flip for exactly the pair a test (and a reader) cares about. It is
	// unexported because it is an ordering detail, not something a client asked
	// for; connected_at remains the human-readable form.
	seq uint64
}

// connRegistry tracks live MCP client sessions.
//
// Why a registry at all: mcp-go owns the session map and only ever exposes
// individual sessions by id, so there is no way to ask it what is connected. The
// register/unregister hooks carry the ClientSession, so this keeps its own small
// map fed by those.
type connRegistry struct {
	mu    sync.Mutex
	seq   uint64
	conns map[string]*connEntry
	// pending holds facts learned before the session was registered. mcp-go
	// answers initialize *first* and registers the session after ("Register
	// session after successful initialization"), so the namespace claim — which
	// rides the initialize request — arrives before there is a row to put it on.
	// Without this the claim was dropped and every client looked unnamespaced.
	pending map[string]string
}

// connEntry keeps the mcp-go session object rather than a copy of its fields.
//
// The client's name is not known when the session registers: mcp-go registers
// first and the client's `initialize` arrives after, so a name captured in the
// register hook would be empty for every client. mcp-go stores it on the session
// itself, so the read path asks the session — which is also why this holds the
// object and not a snapshot.
type connEntry struct {
	session     mcpserver.ClientSession
	namespace   string
	connectedAt time.Time
	seq         uint64
}

func newConnRegistry() *connRegistry {
	return &connRegistry{conns: make(map[string]*connEntry), pending: make(map[string]string)}
}

// add records a newly registered session. It runs before the client has said
// anything, so only the session itself is known here.
func (r *connRegistry) add(s mcpserver.ClientSession) {
	if s == nil || s.SessionID() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.conns[s.SessionID()] = &connEntry{
		session:     s,
		namespace:   r.pending[s.SessionID()],
		connectedAt: time.Now().UTC(),
		seq:         r.seq,
	}
	delete(r.pending, s.SessionID())
}

// remove forgets a session that went away.
func (r *connRegistry) remove(s mcpserver.ClientSession) {
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, s.SessionID())
}

// noteNamespace records the namespace the client claimed in its initialize
// request.
//
// The claim is per request (withRequestContext puts it in the context), and
// initialize is the first request that carries it — but mcp-go registers the
// session only after answering initialize, so the row usually does not exist yet.
// The claim is therefore held for the registration to pick up, and applied
// directly when the session is already known (the SSE transport registers
// first).
//
// The pending map is bounded: an entry is only ever created by an initialize and
// is consumed by the registration that follows, so anything still in it is the
// residue of a client that initialized and then dropped. Clearing it at a cap
// keeps that from growing without a sweeper.
func (r *connRegistry) noteNamespace(sessionID, namespace string) {
	if sessionID == "" || namespace == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.conns[sessionID]; e != nil {
		e.namespace = namespace
		return
	}
	// ponytail: hard cap instead of a sweeper; three in flight is already more
	// than any real client count, and the cost of being wrong is one missing
	// namespace label, not a wrong one.
	if len(r.pending) >= 64 {
		r.pending = make(map[string]string)
	}
	r.pending[sessionID] = namespace
}

// list returns every live connection, oldest first.
//
// The rows are built under the lock rather than from pointers collected under it:
// namespace is a plain field that noteNamespace writes, so copying pointers and
// reading their fields afterwards is a data race with a client that is
// initializing at the same moment (caught by this package's own -race test).
func (r *connRegistry) list() []Connection {
	// Snapshot the rows under the lock, but call external methods (mcp-go's
	// SessionID and GetClientInfo) OUTSIDE it. Calling into third-party interfaces
	// inside our mutex risks foreign locking or stalls locking us out.
	r.mu.Lock()
	entries := make([]connEntry, 0, len(r.conns))
	for _, e := range r.conns {
		entries = append(entries, *e)
	}
	r.mu.Unlock()

	out := make([]Connection, 0, len(entries))
	for _, e := range entries {
		c := Connection{
			SessionID:   e.session.SessionID(),
			Namespace:   e.namespace,
			ConnectedAt: e.connectedAt.Format(time.RFC3339),
			seq:         e.seq,
		}
		if withInfo, ok := e.session.(mcpserver.SessionWithClientInfo); ok {
			info := withInfo.GetClientInfo()
			c.Name, c.Version = info.Name, info.Version
		}
		out = append(out, c)
	}

	// Sorted after unlocking: the timestamps are copies by now, and holding the
	// lock through a sort would block a client connecting for no reason.
	//
	// The timestamp is the primary key because it is what the panel shows, and the
	// registration sequence breaks its ties: two clients that attach within one
	// second are still ordered by who arrived first, rather than by whichever
	// order the map happened to yield.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConnectedAt != out[j].ConnectedAt {
			return out[i].ConnectedAt < out[j].ConnectedAt
		}
		return out[i].seq < out[j].seq
	})
	return out
}

// count reports how many clients are attached, without building the slice.
func (r *connRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// Connections lists the MCP clients currently attached to this instance,
// oldest first. Safe to call with none: it returns an empty slice, not nil.
func (s *Server) Connections() []Connection {
	if s == nil || s.connections == nil {
		return []Connection{}
	}
	return s.connections.list()
}

// SetConnectionsListener installs a callback fired whenever a client attaches or
// detaches, so the Web UI can refresh without polling. Called from the hooks, so
// it must not block.
func (s *Server) SetConnectionsListener(fn func()) {
	s.connListener = fn
}

func (s *Server) notifyConnectionsChanged() {
	if s != nil && s.connListener != nil {
		s.connListener()
	}
}
