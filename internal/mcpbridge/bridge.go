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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionHeader  = "Mcp-Session-Id" // set by mcp-go on the initialize reply; required afterwards
	cleanupTimeout = 5 * time.Second
)

// Config describes one bridge run. In and Out are required.
type Config struct {
	// URL is the full URL of the MCP HTTP endpoint, e.g.
	// http://127.0.0.1:18765/stream, or .../sse with SSE set.
	URL string
	// SSE selects the SSE transport: one GET event stream carries the message
	// endpoint (its first `endpoint` event), every reply, and server-initiated
	// messages; stdin lines are POSTed to the discovered message URL.
	SSE   bool
	Token string    // optional bearer credential: a plaintext token, or the hash string a hash-configured instance accepts
	In    io.Reader // MCP stdio input: newline-delimited JSON
	Out   io.Writer // MCP stdio output: MCP messages only
	Err   io.Writer // diagnostics; never carries protocol messages

	// KeepAlive, when > 0 on the streamable transport, is how often the bridge
	// pings the instance while it has no MCP session yet. The bridge process
	// itself is a client presence: an instance behind an idle countdown must
	// see an attached-but-silent bridge as a connection, or it exits under a
	// bridge that has simply not sent a message yet. Once initialize has
	// minted a session the listening GET stream is the presence and the pings
	// stop. The SSE transport needs none — its event stream is opened before
	// anything else.
	KeepAlive time.Duration
}

// Run bridges Config.In to the MCP endpoint until stdin reaches EOF, the
// server fails, or ctx is canceled. A nil return is a clean shutdown (stdin
// EOF, once in-flight replies have been flushed, or ctx cancellation); any
// other error is terminal for the bridge process.
func Run(ctx context.Context, cfg Config) error {
	if cfg.URL == "" {
		return errors.New("mcpbridge: empty endpoint URL")
	}
	if cfg.Err == nil {
		cfg.Err = io.Discard
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &bridge{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		client: &http.Client{},
		fatal:  make(chan error, 1),
	}

	b.pendingCond = sync.NewCond(&b.pendingMu)
	if !cfg.SSE && cfg.KeepAlive > 0 {
		go b.keepAliveLoop()
	}
	readDone := make(chan error, 1)
	if cfg.SSE {
		// The stdin reader starts only once the stream's endpoint event names
		// where messages go; runSSE does that.
		go b.runSSE(readDone)
		// Cancellation must reach a waitPendingReplies already parked on the
		// condition variable.
		go func() { <-b.ctx.Done(); b.pendingCond.Broadcast() }()
	} else {
		go func() { readDone <- b.readStdin() }()
	}
	return b.settle(readDone)
}

// settle waits for the first terminal event — a fatal relay error, stdin EOF,
// or ctx cancellation — and tears the bridge down. On stdin EOF, replies to
// messages already read may still be in flight: the bridge outlives the pipe
// until the answers are on stdout.
func (b *bridge) settle(readDone <-chan error) error {
	var err error
	select {
	case err = <-b.fatal:
	case err = <-readDone:
		if err == nil {
			b.wg.Wait() // every forwarded message has been handed to the server
			if b.cfg.SSE {
				b.waitPendingReplies() // SSE replies ride the event stream, not the POST
			}
			select {
			case err = <-b.fatal:
			default:
			}
		}
	case <-b.ctx.Done():
	}
	b.cancel()  // stop in-flight POSTs and unblock the listening stream
	b.cleanup() // streamable mode: best-effort session teardown over HTTP
	return err
}

type bridge struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	client *http.Client

	sessMu     sync.Mutex
	sessionID  string // streamable mode: captured from the initialize reply; "" until then
	messageURL string // SSE mode: POST target learned from the stream's endpoint event

	// outMu and errMu are separate on purpose: a client that stops reading stdout
	// blocks writeOut on its pipe, and diagnostics must not block with it.
	outMu sync.Mutex
	errMu sync.Mutex
	fatal chan error     // buffered(1): first terminal error wins
	wg    sync.WaitGroup // in-flight forwards, drained when stdin closes

	pendingMu    sync.Mutex     // guards the three fields below
	pending      map[string]int // SSE mode: forwarded request ids still owed a reply
	fatalPending bool           // SSE mode: a fatal error fired while EOF waits
	pendingCond  *sync.Cond     // signaled on every pending/fatal change and on ctx cancellation
}

// readStdin forwards every newline-delimited message. Malformed lines are NOT
// rejected here — the relay is dumb; the MCP server answers parse errors.
//
// On the streamable transport the first message is forwarded in order and
// awaited: the session id every later POST must present rides on the
// initialize reply, and a pipelined batch (a script, unlike interactive
// clients that wait for the reply) would race it. The SSE transport gets the
// same ordering for free — its stdin reader only starts once the stream has
// named the message endpoint.
func (b *bridge) readStdin() error {
	r := bufio.NewReader(b.cfg.In)
	first := true
	for {
		line, err := r.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			if first && !b.cfg.SSE {
				first = false
				b.forward(msg)
			} else {
				first = false
				b.wg.Go(func() { b.forward(msg) })
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("mcpbridge: reading stdin: %w", err)
	}
}

// forward dispatches one MCP message over the configured transport.
func (b *bridge) forward(msg []byte) {
	if b.cfg.SSE {
		b.forwardSSE(msg)
		return
	}
	b.forwardStreamable(msg)
}

// forwardStreamable POSTs one MCP message to the streamable-HTTP endpoint and
// relays the reply. Failures are terminal: a client waiting on a bridge whose
// session is gone must be told, not hung.
func (b *bridge) forwardStreamable(msg []byte) {
	resp, err := b.postStreamable(msg)
	if err != nil {
		b.fail(msg, fmt.Sprintf("POST %s: %v", b.cfg.URL, err))
		return
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get(sessionHeader); sid != "" {
		b.startServerStream(sid)
	}
	switch resp.StatusCode {
	case http.StatusAccepted:
		// Notification or response: the protocol expects no reply.
		_, _ = io.Copy(io.Discard, resp.Body)
	case http.StatusOK:
		if err := b.relayBody(resp); err != nil {
			b.fail(msg, err.Error())
		}
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		b.fail(msg, fmt.Sprintf("the server answered %s: %s", resp.Status, bytes.TrimSpace(snippet)))
	}
}

// forwardSSE POSTs one MCP message to the message endpoint named by the event
// stream. The reply (if any) arrives on that stream, not in the POST response.
func (b *bridge) forwardSSE(msg []byte) {
	target := b.getMessageURL()
	if target == "" {
		b.fail(msg, "the server has not named a message endpoint")
		return
	}
	// Recorded before the POST: the reply can reach the relay goroutine as
	// soon as the server accepts it, and the answer would be missed as owed.
	if id, ok := requestKey(msg); ok {
		b.recordPending(id)
	}
	req, err := http.NewRequestWithContext(b.ctx, http.MethodPost, target, bytes.NewReader(msg))
	if err != nil {
		b.fail(msg, fmt.Sprintf("POST %s: %v", target, err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		b.fail(msg, fmt.Sprintf("POST %s: %v", target, err))
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		// The reply travels over the event stream.
		_, _ = io.Copy(io.Discard, resp.Body)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		b.fail(msg, fmt.Sprintf("the server answered %s: %s", resp.Status, bytes.TrimSpace(snippet)))
	}
}

// relayBody writes one streamable-HTTP POST reply to stdout. Replies arrive
// either as a single JSON object or as an SSE stream: once any notification
// has been delivered to this session, mcp-go answers every POST with
// text/event-stream.
func (b *bridge) relayBody(resp *http.Response) error {
	switch ct := resp.Header.Get("Content-Type"); {
	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading reply: %w", err)
		}
		b.writeOut(body)
		return nil
	case strings.HasPrefix(ct, "text/event-stream"):
		return b.relaySSE(resp.Body)
	default:
		return fmt.Errorf("unexpected reply content type %q", ct)
	}
}

// relaySSE forwards every `data:` payload of an SSE stream to stdout as one
// MCP message per event (the streamable transport frames POST replies and
// server messages this way).
func (b *bridge) relaySSE(r io.Reader) error {
	return readEvents(r, func(_ string, data []byte) { b.writeOut(data) })
}

// readEvents parses one SSE stream, invoking onEvent once per event with the
// event name ("" when unnamed) and the joined data payload. ReadBytes, not
// bufio.Scanner: a single MCP message can far exceed any fixed line limit.
func readEvents(r io.Reader, onEvent func(event string, data []byte)) error {
	br := bufio.NewReader(r)
	var event string
	var data []byte
	dispatch := func() {
		if data != nil {
			onEvent(event, data)
		}
		event, data = "", nil
	}
	for {
		line, err := br.ReadBytes('\n')
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
			dispatch()
		case bytes.HasPrefix(trimmed, []byte("event:")):
			event = string(bytes.TrimSpace(trimmed[len("event:"):]))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			payload := bytes.TrimPrefix(trimmed[len("data:"):], []byte(" "))
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, payload...)
		}
		if err == nil {
			continue
		}
		dispatch()
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("reading event stream: %w", err)
	}
}

// startServerStream opens the listening GET stream once a session id exists:
// it is the carrier for server-initiated messages (notifications, sampling
// requests). Opening it before initialize would make mcp-go mint a second
// session instead of attaching to this one.
func (b *bridge) startServerStream(sessionID string) {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	if b.sessionID != "" {
		return
	}
	b.sessionID = sessionID
	go b.readServerStream(sessionID)
}

func (b *bridge) readServerStream(sessionID string) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, b.cfg.URL, nil)
	if err != nil {
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, sessionID)
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		if b.ctx.Err() != nil {
			return // the bridge is shutting down, not failing
		}
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.fail(nil, fmt.Sprintf("GET %s: the server answered %s", b.cfg.URL, resp.Status))
		return
	}
	err = b.relaySSE(resp.Body)
	if b.ctx.Err() != nil {
		return // shutdown unblocked the stream; not a failure
	}
	if err != nil {
		b.fail(nil, err.Error())
		return
	}
	b.fail(nil, fmt.Sprintf("GET %s: the server closed the event stream", b.cfg.URL))
}

// runSSE serves the SSE transport: one GET event stream carries everything.
// Its first `endpoint` event names where messages are POSTed; from then on
// stdin lines are relayed to that URL, and every `message` event on the
// stream — replies and server-initiated messages alike — goes to stdout.
func (b *bridge) runSSE(readDone chan error) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, b.cfg.URL, nil)
	if err != nil {
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		if b.ctx.Err() != nil {
			return // the bridge is shutting down, not failing
		}
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.fail(nil, fmt.Sprintf("GET %s: the server answered %s", b.cfg.URL, resp.Status))
		return
	}
	err = readEvents(resp.Body, func(event string, data []byte) {
		switch event {
		case "endpoint":
			b.startSSEReader(data, readDone)
		case "", "message":
			b.writeOut(data) // before replySaw: the reply must be out before EOF may finish
			b.replySaw(data)
		}
	})
	if b.ctx.Err() != nil {
		return // shutdown unblocked the stream; not a failure
	}
	if err != nil {
		b.fail(nil, err.Error())
		return
	}
	b.fail(nil, fmt.Sprintf("GET %s: the server closed the event stream", b.cfg.URL))
}

// startSSEReader records the message endpoint from the stream's endpoint event
// and starts relaying stdin to it (once; later endpoint events are ignored).
func (b *bridge) startSSEReader(endpoint []byte, readDone chan error) {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	if b.messageURL != "" {
		return
	}
	b.messageURL = resolveEndpoint(b.cfg.URL, strings.TrimSpace(string(endpoint)))
	go func() { readDone <- b.readStdin() }()
}

// resolveEndpoint resolves the endpoint event's URL against the stream's URL
// (servers typically send a relative path like /message?sessionId=...).
func resolveEndpoint(streamURL, endpoint string) string {
	base, err := url.Parse(streamURL)
	if err != nil {
		return endpoint
	}
	ref, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	return base.ResolveReference(ref).String()
}

func (b *bridge) postStreamable(msg []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodPost, b.cfg.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid := b.getSession(); sid != "" {
		req.Header.Set(sessionHeader, sid)
	}
	b.authorize(req)
	return b.client.Do(req)
}

func (b *bridge) getSession() string {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	return b.sessionID
}

func (b *bridge) getMessageURL() string {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	return b.messageURL
}

func (b *bridge) authorize(req *http.Request) {
	if b.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.Token)
	}
}

// fail records a terminal error: diagnostics to stderr, a JSON-RPC error to
// the client when the failed message expects a reply, and a signal to Run.
func (b *bridge) fail(msg []byte, text string) {
	b.errf("mcpbridge: %s", text)
	if id := requestID(msg); id != nil {
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32000, "message": "termcp bridge: " + text},
		})
		if err == nil {
			b.writeOut(payload)
		}
	}
	select {
	case b.fatal <- errors.New("mcpbridge: " + text):
	default:
	}
	// Waking a waitPendingReplies parked on the condition variable: the owed
	// replies it waits for may never arrive after a failure.
	b.pendingMu.Lock()
	b.fatalPending = true
	b.pendingMu.Unlock()
	b.pendingCond.Broadcast()
}

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
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	if b.pending == nil {
		b.pending = make(map[string]int)
	}
	b.pending[id]++
}

// replySaw clears one owed reply when a relayed message carries its id.
func (b *bridge) replySaw(msg []byte) {
	id := requestID(msg)
	if id == nil {
		return
	}
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	n, ok := b.pending[string(id)]
	if !ok {
		return
	}
	if n == 1 {
		delete(b.pending, string(id))
	} else {
		b.pending[string(id)] = n - 1
	}
	if len(b.pending) == 0 {
		b.pendingCond.Broadcast()
	}
}

// waitPendingReplies blocks until every request forwarded over the SSE
// transport has seen its reply on the event stream, the bridge failed, or ctx
// was canceled. Unlike the streamable transport — where the POST response is
// the reply — SSE replies arrive decoupled from POST acceptance, so stdin EOF
// alone does not mean the batch is complete.
func (b *bridge) waitPendingReplies() {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	for len(b.pending) > 0 && !b.fatalPending && b.ctx.Err() == nil {
		b.pendingCond.Wait()
	}
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

func (b *bridge) writeOut(msg []byte) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.cfg.Out.Write(bytes.TrimRight(msg, "\r\n"))
	_, _ = b.cfg.Out.Write([]byte{'\n'})
}

func (b *bridge) errf(format string, args ...any) {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	fmt.Fprintf(b.cfg.Err, format+"\n", args...)
}
