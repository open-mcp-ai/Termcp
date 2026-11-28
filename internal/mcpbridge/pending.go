// Package mcpbridge relays MCP JSON-RPC messages between a stdio client and an
// MCP HTTP endpoint: termcp's streamable-HTTP /stream (the default) or its SSE
// transport (/sse), or any other MCP server speaking either one.
//
// It is deliberately a dumb relay: every stdin line is POSTed verbatim, every
// reply is written to stdout verbatim, and no MCP semantics are interpreted —
// the client sees exactly what the server answers. stdout therefore carries
// nothing but MCP messages; diagnostics go to stderr.
package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// requestKey returns the JSON-RPC id of a message that is a request — it
// carries both an id and a method — and so has exactly one reply coming. Nil
// for notifications (no id) and responses to server-initiated requests (no
// method).
func requestKey(msg []byte) (string, bool) {
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method json.RawMessage `json:"method"`
	}
	if err := json.Unmarshal(msg, &env); err != nil || len(env.ID) == 0 ||
		bytes.Equal(env.ID, []byte("null")) || len(env.Method) == 0 {
		return "", false
	}
	return string(env.ID), true
}

// recordPending notes a forwarded SSE request that is still owed a reply.
func (b *bridge) recordPending(id string) {
	v, _ := b.pending.LoadOrStore(id, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

// replySaw clears one owed reply when a relayed message carries its id.
func (b *bridge) replySaw(msg []byte) {
	id := requestID(msg)
	if id == nil {
		return
	}
	v, ok := b.pending.Load(string(id))
	if !ok {
		return
	}
	if counter := v.(*atomic.Int64); counter.Add(-1) <= 0 {
		// COMPARE-AND-DELETE, so two replies racing on the last count cannot delete a
		// third forward's fresh entry: only the pointer this call saw is removed.
		b.pending.CompareAndDelete(string(id), v)
		b.wakePending()
	}
}

// waitPendingReplies blocks until every request forwarded over the SSE
// transport has seen its reply on the event stream, the bridge failed, or ctx
// was canceled. Unlike the streamable transport — where the POST response is
// the reply — SSE replies arrive decoupled from POST acceptance, so stdin EOF
// alone does not mean the batch is complete.
func (b *bridge) waitPendingReplies() {
	for b.ctx.Err() == nil && !b.fatalPending.Load() {
		// THE GENERATION IS LOADED BEFORE THE MAP IS READ: a reply that arrives
		// between the two would otherwise be seen by the read but its wake by neither
		// side, and the wait would settle on a queue that had just emptied.
		generation := b.pendingWakeGen()
		if !b.anyPending() {
			return
		}
		select {
		case <-generation:
		case <-b.ctx.Done():
			return
		}
	}
}

// anyPending reports whether any forwarded request is still owed a reply.
func (b *bridge) anyPending() bool {
	outstanding := false
	b.pending.Range(func(_, v any) bool {
		if v.(*atomic.Int64).Load() > 0 {
			outstanding = true
			return false
		}
		return true
	})
	return outstanding
}

// requestID returns the message's JSON-RPC id when it carries one (requests);
// nil for notifications and responses.
func requestID(msg []byte) json.RawMessage {
	if len(msg) == 0 {
		return nil
	}
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(msg, &env); err != nil {
		return nil
	}
	if len(env.ID) == 0 || bytes.Equal(env.ID, []byte("null")) {
		return nil
	}
	return env.ID
}

// keepAliveLoop pings the instance on KeepAlive until a session exists — the
// listening stream then takes over as the presence — or the bridge shuts
// down. Failures are ignored: the pings are not the relay's business, and the
// next real message reports an unreachable instance properly.
func (b *bridge) keepAliveLoop() {
	t := time.NewTicker(b.cfg.KeepAlive)
	defer t.Stop()
	target := presenceURL(b.cfg.URL)
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		if b.getSession() != "" {
			return
		}
		req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, target, nil)
		if err != nil {
			return
		}
		b.authorize(req)
		resp, err := b.client.Do(req)
		if err != nil {
			if b.ctx.Err() != nil {
				return
			}
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// presenceURL is the keep-alive target: the instance root's version endpoint,
// a session-free GET that counts as ordinary activity for an idle countdown.
func presenceURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	u.Path, u.RawQuery, u.Fragment = "/api/version", "", ""
	return u.String()
}

// cleanup terminates the server-side session so the instance does not keep a
// registered client that will never return. Best-effort: shutdown must not
// depend on the server answering. The SSE transport has no teardown call —
// canceling the GET stream is its cleanup.
func (b *bridge) cleanup() {
	if b.cfg.SSE {
		return
	}
	sid := b.getSession()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.cfg.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set(sessionHeader, sid)
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		b.errf("mcpbridge: terminating session: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
