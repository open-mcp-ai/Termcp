package webui

import (
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Viewer is one place someone is watching this instance from.
//
// This covers the Web UI's own WebSocket clients — the browser tabs — which is
// the other half of "who is connected?" from the MCP connections
// (internal/mcp/connections.go): those are agents driving sessions, these are
// people watching them. Different transports with different lifetimes, so they
// are reported separately rather than forced into one shape that would have to
// lie about one of them.
type Viewer struct {
	// ID is the tab's instance-local number.
	ID string `json:"id"`
	// Addr is the remote address, without its port: one person on one machine is
	// one viewer, however many ephemeral ports they use.
	Addr string `json:"addr"`
	// UserAgent is the browser's self-description, truncated. Enough to tell two
	// tabs apart in the panel.
	UserAgent string `json:"user_agent,omitempty"`
	// Watching is how many sessions this tab currently streams.
	Watching int `json:"watching"`
	// Since is when the socket opened, RFC 3339 in UTC.
	Since string `json:"since"`
	// Self marks the tab asking. Every open page is itself a viewer, so without
	// this the panel's own page always counts as one of the clients — which is
	// where "why does it say 1 when nothing is connected?" came from. The flag is
	// set per response (the same list is different for each caller), so it is
	// deliberately not stored on the entry.
	Self bool `json:"self,omitempty"`
}

// viewerRegistry tracks the Web UI's attached tabs.
//
// Only WebSocket tabs are tracked. A plain HTTP request has no lifetime, so
// listing one would mean listing "someone who was here recently" as though it
// were a live connection — the exact confusion this panel exists to remove. The
// session list and approval count push over the same socket, so every page that
// is actually open has a row here.
//
// One tab is one independent entry and the id is a counter, so the table is a
// sync.Map and the counter is atomic.
type viewerRegistry struct {
	nextID atomic.Uint64
	tabs   sync.Map // id uint64 -> *viewerEntry
}

type viewerEntry struct {
	id        uint64 // the registry key; also what the page uses to find itself
	addr      string
	userAgent string
	since     time.Time
	watching  func() int // read at list time, so the count is never stale
}

func newViewerRegistry() *viewerRegistry {
	return &viewerRegistry{}
}

// add registers a tab and returns its id for removal.
func (r *viewerRegistry) add(addr, userAgent string, watching func() int) uint64 {
	id := r.nextID.Add(1)
	r.tabs.Store(id, &viewerEntry{
		id:        id,
		addr:      addr,
		userAgent: userAgent,
		since:     time.Now().UTC(),
		watching:  watching,
	})
	return id
}

func (r *viewerRegistry) remove(id uint64) {
	r.tabs.Delete(id)
}

// list returns the attached tabs, longest-attached first.
//
// selfID is the caller's own tab, marked so a page can tell itself apart from
// everybody else. It is filtered out of the *count* by the client, not here: the
// panel should still show you your own row (that is how you know the list works),
// but "connected clients" means "clients other than the page you are looking at".
func (r *viewerRegistry) list(selfID uint64) []Viewer {
	var entries []*viewerEntry
	r.tabs.Range(func(_, v any) bool {
		if e, ok := v.(*viewerEntry); ok {
			entries = append(entries, e)
		}
		return true
	})

	sort.Slice(entries, func(i, j int) bool { return entries[i].since.Before(entries[j].since) })

	out := make([]Viewer, 0, len(entries))
	for _, e := range entries {
		v := Viewer{
			// The registry key, not the position in this list. A positional id
			// renumbers itself every time a tab opens or closes, so a client
			// holding "1" would be pointing at a different tab a second later —
			// and the page needs a stable id to recognise which row is its own.
			ID:        strconv.FormatUint(e.id, 10),
			Addr:      stripPort(e.addr),
			UserAgent: truncateUA(e.userAgent),
			Since:     e.since.Format(time.RFC3339),
			Self:      selfID != 0 && e.id == selfID,
		}
		if e.watching != nil {
			v.Watching = e.watching()
		}
		out = append(out, v)
	}
	return out
}

// Viewers reports who is watching this instance right now. Safe with none.
//
// The registry is created on first use rather than in Register so a Handler built
// by a test does not have to initialise it.
func (h *Handler) Viewers() []Viewer {
	if h == nil {
		return []Viewer{}
	}
	return h.viewerHub().list(0)
}

func (h *Handler) viewerHub() *viewerRegistry {
	h.viewerOnce.Do(func() { h.viewers = newViewerRegistry() })
	return h.viewers
}

// stripPort drops the port from host:port. A bare host is returned unchanged,
// which is what RemoteAddr looks like in tests.
func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// truncateUA bounds what a caller can put in the panel: a user agent is attacker
// controlled and unbounded.
func truncateUA(ua string) string {
	const max = 120
	if len(ua) <= max {
		return ua
	}
	return ua[:max] + "…"
}

// clientsResponse is what GET /api/clients returns.
//
// Two lists, not one: an MCP client is an agent driving sessions and holds a
// protocol session with a name it reported; a Web UI client is a browser tab
// with a socket. Merging them would need a "kind" discriminator on every field
// that only applies to one of them.
type clientsResponse struct {
	MCP   []any    `json:"mcp"`
	WebUI []Viewer `json:"webui"`
}

// handleListClients answers GET /api/clients — who is attached to this instance.
//
// Distinct from GET /api/connections, which lists the stored SSH profiles: those
// are hosts you can connect *to*, this is about the callers connected *here*.
func (h *Handler) handleListClients(w http.ResponseWriter, r *http.Request) {
	resp := clientsResponse{WebUI: h.Viewers()}
	// The MCP server is optional: a Handler built by a test, or an instance
	// running without one, still answers — it just has no MCP rows.
	if h.MCPConnections != nil {
		resp.MCP = h.MCPConnections()
	}
	if resp.MCP == nil {
		resp.MCP = []any{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// NotifyClientsChanged wakes every open page so it re-reads the session frame,
// which now carries the client lists. It is the sink for the MCP server's
// connect/disconnect hook (see main), so one broadcast path covers both "a
// session changed" and "a client attached" — the page cannot tell the
// difference and does not need to.
func (h *Handler) NotifyClientsChanged() {
	if h == nil {
		return
	}
	h.sessionHub().broadcast()
}
