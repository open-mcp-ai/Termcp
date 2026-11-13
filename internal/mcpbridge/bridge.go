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
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionHeader    = "Mcp-Session-Id" // set by mcp-go on the initialize reply; required afterwards
	cleanupTimeout   = 5 * time.Second
	recoveryTimeout  = 5 * time.Second
	recoveryAttempts = 3
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

	// Headers are extra request headers sent on every request. Token is the
	// conventional Authorization: Bearer case and stays a field of its own
	// because the command line has a flag for it; this is for the servers that
	// want something else (X-Api-Key, a tenant header). A name given here is
	// applied after the transport's own headers, so an explicit value wins.
	Headers map[string]string

	// KeepAlive, when > 0 on the streamable transport, is how often the bridge
	// pings the instance while it has no MCP session yet. The bridge process
	// itself is a client presence: an instance behind an idle countdown must
	// see an attached-but-silent bridge as a connection, or it exits under a
	// bridge that has simply not sent a message yet. Once initialize has
	// minted a session the listening GET stream is the presence and the pings
	// stop. The SSE transport needs none — its event stream is opened before
	// anything else.
	KeepAlive time.Duration

	// DialContext, when set, is the network dialer used for HTTP and SSE requests.
	// Used when the endpoint is reached through a remote host's network via an
	// SSH client dialer, rather than dialed directly from this machine.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// Via, when set, is the SSH profile name whose network carries the requests.
	// Parsed from `--via <profile>` on BuiltinCommand's argv.
	Via string
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
	// THE DIALER IS THE EGRESS MACHINE (#233). With a default client, every
	// request leaves from this process's own network, so a URL naming 127.0.0.1
	// means the machine termcp happens to run on. An extension whose target is a
	// remote host asks for the request to leave from THERE instead: same URL, same
	// HTTP, but dialled inside that host's SSH connection. That is the whole
	// semantic difference between `target = "internal"` and `target = "<host>"` for
	// a url extension — and it costs no listening port, so two extensions pointing
	// at the same endpoint cannot collide.
	client := &http.Client{}
	if cfg.DialContext != nil {
		client.Transport = &http.Transport{DialContext: cfg.DialContext}
	}
	b := &bridge{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		client: client,
		fatal:  make(chan error, 1),
		outTok: make(chan struct{}, 1),
	}

	b.outTok <- struct{}{}
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

	sessMu          sync.Mutex
	sessionID       string // streamable mode: captured from the initialize reply; "" until then
	messageURL      string // SSE mode: POST target learned from the stream's endpoint event
	streamCancel    context.CancelFunc
	readerStarted   bool
	sseRecovering   bool
	sseStreamCancel context.CancelFunc
	sseRecoveryDone chan struct{}

	handshakeMu sync.RWMutex
	initialize  []byte
	initialized []byte

	recoverMu sync.Mutex
	recovery  *sessionRecovery

	// outTok serialises writes to stdout with a door: a client that stops reading
	// stdout must not park every writer in the bridge forever (issue #220).
	// Writers give up after writeOutWait and report the failure.
	//
	// errMu stays a mutex: diagnostics are short, and unlike stdout they are not a
	// pipe an agent's harness might stop reading.
	outTok chan struct{}
	errMu  sync.Mutex
	fatal  chan error     // buffered(1): first terminal error wins
	wg     sync.WaitGroup // in-flight forwards, drained when stdin closes

	pendingMu    sync.Mutex     // guards the three fields below
	pending      map[string]int // SSE mode: forwarded request ids still owed a reply
	fatalPending bool           // SSE mode: a fatal error fired while EOF waits
	pendingCond  *sync.Cond     // signaled on every pending/fatal change and on ctx cancellation
}

type sessionRecovery struct {
	done chan struct{}
	err  error
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
	b.rememberHandshake(msg)
	sessionID := b.getSession()
	resp, err := b.postStreamableContext(b.ctx, msg, sessionID)
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
	case http.StatusNotFound:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if sessionID == "" {
			b.fail(msg, fmt.Sprintf("the server answered %s: %s", resp.Status, bytes.TrimSpace(snippet)))
			return
		}
		if err := b.recoverSession(sessionID); err != nil {
			b.fail(msg, fmt.Sprintf("MCP session was lost; the request may have run, but its result is unknown; reinitialization failed: %v", err))
			return
		}
		b.unknownResult(msg, "the MCP session was replaced; the request may have run, but its result is unknown; the connection was reinitialized without replaying it")
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		b.fail(msg, fmt.Sprintf("the server answered %s: %s", resp.Status, bytes.TrimSpace(snippet)))
	}
}

// forwardSSE POSTs one MCP message to the message endpoint named by the event
// stream. The reply (if any) arrives on that stream, not in the POST response.
func (b *bridge) forwardSSE(msg []byte) {
	b.rememberHandshake(msg)
	if b.sseIsRecovering() {
		waitCtx, cancelWait := context.WithTimeout(b.ctx, 3*recoveryTimeout)
		err := b.waitSSERecovery(waitCtx)
		cancelWait()
		if err != nil {
			b.unknownResultState(msg, "the SSE connection could not be reinitialized before this request; the request was not sent and no result is available", false)
			return
		}
	}
	target := b.getMessageURL()
	if target == "" {
		b.unknownResultState(msg, "the SSE message endpoint is unavailable; the request was not sent and no result is available", false)
		return
	}
	// Recorded before the POST: the reply can reach the relay goroutine as
	// soon as the server accepts it, and the answer would be missed as owed.
	if id, ok := requestKey(msg); ok {
		b.recordPending(id)
	}
	// A POST gets its own deadline (#158): a remote server that accepts the TCP
	// connection and sits silently would otherwise hold the goroutine until the
	// entire bridge shuts down.
	postCtx, cancelPost := context.WithTimeout(b.ctx, 30*time.Second)
	defer cancelPost()
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, target, bytes.NewReader(msg))
	if err != nil {
		b.fail(msg, fmt.Sprintf("POST %s: %v", target, err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		if b.sseIsRecovering() {
			b.unknownResultState(msg, "the SSE connection was interrupted; the request may have run, but its result is unknown and it was not replayed", false)
			return
		}
		b.fail(msg, fmt.Sprintf("POST %s: %v", target, err))
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		// The reply travels over the event stream.
		_, _ = io.Copy(io.Discard, resp.Body)
	case http.StatusNotFound, http.StatusGone:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		b.beginSSERecovery()
		b.unknownResultState(msg, fmt.Sprintf("the SSE message endpoint answered %s (%s); the request may have run, but its result is unknown and it was not replayed", resp.Status, bytes.TrimSpace(snippet)), false)
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

func (b *bridge) postStreamableContext(ctx context.Context, msg []byte, sessionID string) (*http.Response, error) {
	// NO DEADLINE HERE, deliberately: this POST's response body IS the tool's answer,
	// so a fixed cap would cut a legitimately slow call at 30s. The wait is the
	// caller's to bound, and the bridge's own context ends it at shutdown.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(sessionHeader, sessionID)
	}
	b.authorize(req)
	return b.client.Do(req)
}

func (b *bridge) rememberHandshake(msg []byte) {
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(msg, &envelope) != nil {
		return
	}
	b.handshakeMu.Lock()
	defer b.handshakeMu.Unlock()
	switch envelope.Method {
	case "initialize":
		b.initialize = append(b.initialize[:0], msg...)
	case "notifications/initialized":
		b.initialized = append(b.initialized[:0], msg...)
	}
}

func (b *bridge) recoveryMessages() ([]byte, []byte, bool) {
	b.handshakeMu.RLock()
	defer b.handshakeMu.RUnlock()
	if len(b.initialize) == 0 {
		return nil, nil, false
	}
	initialize := append([]byte(nil), b.initialize...)
	initialized := append([]byte(nil), b.initialized...)
	if len(initialized) == 0 {
		initialized = []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	}
	return initialize, initialized, true
}

func (b *bridge) recoverSession(failedSessionID string) error {
	b.recoverMu.Lock()
	current := b.getSession()
	if current != "" && current != failedSessionID {
		b.recoverMu.Unlock()
		return nil
	}
	if active := b.recovery; active != nil {
		done := active.done
		b.recoverMu.Unlock()
		select {
		case <-done:
			return active.err
		case <-b.ctx.Done():
			return b.ctx.Err()
		}
	}
	recovery := &sessionRecovery{done: make(chan struct{})}
	b.recovery = recovery
	b.recoverMu.Unlock()

	err := b.performSessionRecovery()

	b.recoverMu.Lock()
	recovery.err = err
	if b.recovery == recovery {
		b.recovery = nil
	}
	close(recovery.done)
	b.recoverMu.Unlock()
	return err
}

func (b *bridge) performSessionRecovery() error {
	initialize, initialized, ok := b.recoveryMessages()
	if !ok {
		return errors.New("the bridge did not observe an initialize request to replay into a new session")
	}
	b.clearSession()
	var lastErr error
	for attempt := range recoveryAttempts {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 150 * time.Millisecond)
			select {
			case <-timer.C:
			case <-b.ctx.Done():
				timer.Stop()
				return b.ctx.Err()
			}
		}
		ctx, cancel := context.WithTimeout(b.ctx, recoveryTimeout)
		resp, err := b.postStreamableContext(ctx, initialize, "")
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		sessionID := resp.Header.Get(sessionHeader)
		if resp.StatusCode == http.StatusOK && sessionID != "" {
			cancel()
			b.startServerStream(sessionID)
			notifyCtx, notifyCancel := context.WithTimeout(b.ctx, recoveryTimeout)
			notifyResp, notifyErr := b.postStreamableContext(notifyCtx, initialized, sessionID)
			if notifyErr == nil {
				_, _ = io.Copy(io.Discard, notifyResp.Body)
				notifyResp.Body.Close()
				if notifyResp.StatusCode == http.StatusOK || notifyResp.StatusCode == http.StatusAccepted {
					notifyCancel()
					return nil
				}
				notifyErr = fmt.Errorf("initialized notification answered %s", notifyResp.Status)
			}
			notifyCancel()
			lastErr = notifyErr
			b.clearSession()
			continue
		}
		cancel()
		lastErr = fmt.Errorf("initialize answered %s", resp.Status)
	}
	return fmt.Errorf("new MCP session could not be initialized after %d attempts: %w", recoveryAttempts, lastErr)
}

func (b *bridge) clearSession() {
	b.sessMu.Lock()
	b.sessionID = ""
	if b.streamCancel != nil {
		b.streamCancel()
		b.streamCancel = nil
	}
	b.sessMu.Unlock()
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
	for name, value := range b.cfg.Headers {
		req.Header.Set(name, value)
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

// unknownResult answers the request that discovered the lost session without
// pretending the operation failed before execution. The bridge cannot know
// whether the server committed the call before dropping the session, so it
// reports the uncertainty and deliberately does not replay the request.
func (b *bridge) unknownResult(msg []byte, text string) {
	b.unknownResultState(msg, text, true)
}

func (b *bridge) unknownResultState(msg []byte, text string, reinitialized bool) {
	b.errf("mcpbridge: %s", text)
	if id := requestID(msg); id != nil {
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32010,
				"message": "termcp bridge: " + text,
				"data": map[string]any{
					"kind":                 "termcp_session_replaced",
					"result":               "unknown",
					"reinitialized":        reinitialized,
					"request_was_replayed": false,
				},
			},
		})
		if err == nil {
			b.writeOut(payload)
		}
	}
	b.replySaw(msg)
}

// writeOutWait is how long a write to stdout may wait for the prior write to finish.
//
// BOUNDED, NOT UNBOUNDED (issue #220). The comment above outTok documents the
// danger: a client that stops reading stdout leaves the write stuck in the OS.
// Five seconds is far longer than any working pipe needs and short enough that a
// caller gets an answer rather than waiting forever.
const writeOutWait = 5 * time.Second

func (b *bridge) writeOut(msg []byte) {
	timer := time.NewTimer(writeOutWait)
	defer timer.Stop()
	select {
	case <-b.outTok:
	case <-timer.C:
		b.errf("mcpbridge: dropping output because a prior write has not finished in %s; the client is probably not reading stdout", writeOutWait)
		return
	case <-b.ctx.Done():
		return
	}
	defer func() {
		select {
		case b.outTok <- struct{}{}:
		default:
		}
	}()

	_, _ = b.cfg.Out.Write(bytes.TrimRight(msg, "\r\n"))
	_, _ = b.cfg.Out.Write([]byte{'\n'})
}

func (b *bridge) errf(format string, args ...any) {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	fmt.Fprintf(b.cfg.Err, format+"\n", args...)
}
