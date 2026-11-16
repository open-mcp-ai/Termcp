package mcp

// MCP over HTTP, PER EXTENSION (issues #234 and #328).
//
// The 18765 port already serves termcp's OWN tool surface at /stream. That surface
// has one tool (`term`) whose whole vocabulary is this project's directive language,
// so an MCP client that does not know the language cannot reach anything an
// extension provides. The extension's own MCP server — its tools, its instructions,
// its serverInfo — is reachable only from inside termcp, through `call <peer>`.
//
// /ext/{name}/stream closes that gap without teaching anybody the language: the
// client points its MCP config at the URL and talks to the EXTENSION, transparently.
// termcp stays the lifecycle owner (an on-demand extension is started by the first
// request and reaped by the same idle rule as everything else) and the policy owner
// (the capability whitelist and review gate are the extension's own, unchanged).
//
// IT REUSES THE STREAMABLE TRANSPORT. Session ids, the GET stream, DELETE and the
// POST/GET status codes are the transport's job, and they are the same job whether
// the thing behind them is termcp's own server or an extension's line-delimited
// one. What used to be here was a second, hand-written half of that transport that
// had no session, no GET stream (405), a DELETE that reported success while doing
// nothing, and a 202 for a request the peer never answered. All of it is gone; what
// remains is an extEndpoint that answers "what does Handle mean for an extension",
// and a pump that reads the channel continuously so the peer's unsolicited frames
// have somewhere to go.
//
// WHAT IS STILL DELIBERATELY NOT HERE:
//
//   - No per-topic filtering. A shared stdio channel has no session dimension, so
//     every notification is fanned out whole. Filtering by topic would first have
//     to answer "what is a subscribable resource", which is another feature.
//   - No Last-Event-ID replay. Fan-out is lossy by contract (each session's queue
//     is 64 frames); an extension that needs reliability puts a sequence number in
//     its event and the client catches up with a tools/call.
//   - No legacy /ext/{name}/sse. There is no client asking for it.

import (
	"net/http"
	"strings"
)

// ExtMCPHandler serves one extension's MCP server over streamable HTTP.
//
// The route is registered as `/ext/{name}/stream`; the name is read from the path
// rather than a query parameter so a client can configure it as an ordinary MCP
// URL. One endpoint — and therefore one session table, one pump and one cached
// initialize — exists per extension name, created on first use.
func (s *Server) ExtMCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.PathValue("name"))
		if name == "" {
			http.Error(w, "no extension named in the path: the route is /ext/{name}/stream", http.StatusNotFound)
			return
		}
		s.extEndpoint(name).transport.ServeHTTP(w, r)
	})
}

// extEndpoint returns the endpoint for a name, creating it on first use.
//
// PER NAME RATHER THAN PER REQUEST, because everything the endpoint keeps is
// per-extension: the session ids a client was handed, the cached initialize answer,
// the upstream id counter, the pump that is reading the channel right now. A fresh
// one per request would forget all of it between two POSTs of the same session.
func (s *Server) extEndpoint(name string) *extEndpoint {
	if v, ok := s.extEndpoints.Load(name); ok {
		return v.(*extEndpoint)
	}
	ep := newExtEndpoint(s, name)
	actual, _ := s.extEndpoints.LoadOrStore(name, ep)
	return actual.(*extEndpoint)
}
