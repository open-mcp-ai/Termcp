// Package jrpc is one JSON-RPC frame channel over two transports.
//
// Write a frame, read frames until the one carrying your id arrives, and keep
// the rest. That loop is the whole protocol, and it is the same loop for a
// socket and for an HTTP endpoint: the transports differ in exactly two places —
// how a frame goes out, and how frames come in — while the id allocation, the
// "a frame with no id is an event, not an answer", the buffer those events land
// in, and the session id HTTP needs and WS does not are shared.
//
// It is one type with a transport field rather than an interface with two
// implementations, because the interface would have one method per transport and
// every caller would still branch on which one it holds.
//
// It speaks JSON-RPC, not MCP. Nothing here knows what `tools/list` or
// `Page.navigate` mean, which is what lets a peer that spells the envelope
// differently work: CDP frames carry no `jsonrpc` field, need no handshake, and
// route by a `sessionId` INSIDE the frame — that is a property of the peer, not
// of this layer, so it is passed through rather than modelled.
package jrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Transport is how frames move.
type Transport int

const (
	// HTTP is the streamable-MCP shape: every frame is a POST, and the answer is
	// either the POST's JSON body or one event on its text/event-stream body. A
	// Mcp-Session-Id handed out by the first reply must ride on every later
	// request; the channel remembers it so no caller has to.
	HTTP Transport = iota

	// WS is the socket shape: frames flow both ways on one connection with no
	// session header and no pairing beyond the id. A peer that pushes events
	// between requests (DevTools) only works here.
	WS
)

func (t Transport) String() string {
	if t == WS {
		return "ws"
	}
	return "http"
}

const (
	// maxFrameBytes caps one reply body, so a peer that never closes its stream
	// cannot spend the process's memory to answer one call.
	maxFrameBytes = 1 << 20

	// maxEvents is how many unmatched frames a channel holds for Recv before the
	// oldest is dropped.
	//
	// ponytail: drop-oldest ring, no per-event subscriber. Events are a debugging
	// and lifecycle aid (waiting for `Page.loadEventFired`), not a log — a reader
	// that misses one can re-ask the peer. Grow this into a real queue when a
	// caller's correctness starts depending on not losing a specific event.
	maxEvents = 256

	// callTimeout bounds a Call when the context carries no deadline of its own.
	// A socket read cannot be interrupted by context cancellation, so without a
	// deadline a silent peer would hang the call — and a hang here is a hang in
	// the agent's turn, the most expensive place to have one.
	callTimeout = 30 * time.Second
)

// ErrClosed reports a call on a closed channel.
var ErrClosed = errors.New("the channel is closed")

// Channel is one open peer.
//
// NO MUTEX. Every mutable field is either atomic, a channel, or immutable after
// Open — because this type's operations WAIT, and a lock held across a wait is a
// lock that Close cannot take. That was the actual failure: a reader parked inside
// a wait with mu held made Close block until the reader's window expired, so
// "stop now" became "stop in a few seconds".
//
// The three mutating concerns each have a mechanism that fits it:
//
//   - serialising calls is a ONE-SLOT SEMAPHORE (callGate). It is acquired, used,
//     and released — never held while blocking on anything else.
//   - hand-off between the socket reader and callers is the in channel. gorilla
//     allows one reader, and readLoop is it.
//   - the frame backlog is ev, itself a channel, so appending and popping need no
//     coordination at all.
type Channel struct {
	url       string
	transport Transport
	client    *http.Client

	// callGate serialises calls: one call at a time, which is what a request/response
	// peer wants and what keeps each reply matched to its request.
	callGate chan struct{}

	conn *websocket.Conn

	// sess is the Mcp-Session-Id, HTTP only. Atomic because httpPost writes it on one
	// call and later calls read it while a peer may be answering concurrently.
	sess atomic.Pointer[string]

	// next is the next id to hand out.
	next atomic.Int64

	// ev holds frames that arrived while a call waited for its own answer. A channel
	// rather than a slice: it is the only structure that lets one goroutine append
	// while another pops without a lock.
	ev chan string

	// shut reports that Close ran.
	shut atomic.Bool

	// in carries every frame the socket delivers; readErr carries why it stopped.
	in      chan string
	readErr chan error
	done    chan struct{} // closed by onceClose, releasing blocked readers and senders
	closeIn sync.Once     // closes c.in exactly once, whichever path ends the loop
}

// Open connects rawURL, choosing the transport by scheme: http(s) posts one
// frame per request, ws(s) opens a socket.
func Open(ctx context.Context, rawURL string) (*Channel, error) {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("not a URL: %s", rawURL)
	}
	c := &Channel{
		url:      rawURL,
		client:   &http.Client{},
		callGate: make(chan struct{}, 1),
		ev:       make(chan string, maxEvents),
		done:     make(chan struct{}),
	}
	switch u.Scheme {
	case "ws", "wss":
		c.transport = WS
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", rawURL, err)
		}
		c.conn = conn
		c.in = make(chan string, maxEvents)
		c.readErr = make(chan error, 1)
		go c.readLoop()
	case "http", "https":
		c.transport = HTTP
	default:
		return nil, fmt.Errorf("unsupported scheme %q: http(s) posts a frame per request, ws(s) opens a socket", u.Scheme)
	}
	return c, nil
}

// Transport reports how this channel moves frames.
func (c *Channel) Transport() Transport { return c.transport }

// URL is the address the channel was opened on.
func (c *Channel) URL() string { return c.url }

// readLoop is the channel's ONLY reader of the socket, for the life of the
// connection.
//
// gorilla permits exactly one concurrent reader, and one reader that never stops is
// also what keeps read deadlines off the socket: a wait now happens on c.in or on the
// caller's context, never inside a read, so a window that expires leaves nothing
// behind. The goroutine ends when the socket fails or Close runs — a socket close
// makes the blocked read return, which is the only way out of it.
func (c *Channel) readLoop() {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			// Report once, then stop. The channel stays usable for Call/Send as far
			// as this layer knows; what is gone is the ability to receive.
			select {
			case c.readErr <- err:
			default: // a reason was already recorded
			}
			c.closeIn.Do(func() { close(c.in) })
			return
		}
		payload := strings.TrimSpace(string(raw))
		if payload == "" {
			continue
		}
		// The send can block when nobody is reading and the buffer is full, and
		// closing the socket does NOT release it — the read already succeeded. done is
		// what Close closes, so this goroutine always ends.
		select {
		case c.in <- payload:
		case <-c.done:
			c.closeIn.Do(func() { close(c.in) })
			return
		}
	}
}

// awaitFrame returns the next frame from the socket, waiting no longer than ctx.
//
// nil and true mean a frame arrived; "" and false mean the wait itself expired (or
// the socket is gone) — which is an answer about THIS WINDOW, not about the socket.
// Nothing here can damage the connection.
func (c *Channel) awaitFrame(ctx context.Context) (string, bool) {
	select {
	case frame, ok := <-c.in:
		if !ok {
			return "", false
		}
		return frame, true
	case <-c.done:
		// Close ran, so nothing more will arrive. Without this arm a waiting reader
		// would only ever see its window expire, turning "stop now" into "stop in a few
		// seconds".
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

// awaitAnswer waits for the frame carrying want, buffering anything else, up to ctx.
func (c *Channel) awaitAnswer(ctx context.Context, want any) (string, error) {
	for {
		select {
		case frame, ok := <-c.in:
			if !ok {
				return "", c.readFailure()
			}
			if carriesID(frame, want) {
				return frame, nil
			}
			c.buffer(frame)
		case <-c.done:
			return "", ErrClosed
		case <-ctx.Done():
			// The window expired with no answer. Nothing was damaged, so a later call
			// can still use the channel.
			return "", ctx.Err()
		}
	}
}

// readFailure reports why the socket stopped reading, falling back to ErrClosed.
func (c *Channel) readFailure() error {
	select {
	case err := <-c.readErr:
		return err
	default:
		return ErrClosed
	}
}

// writeFrame sends one frame with a write deadline.
//
// Only the WRITE side keeps a deadline. Writes have no sticky state — a write error
// fails that write — while a write that can hang would hang the caller's turn.
func (c *Channel) writeFrame(ctx context.Context, frame map[string]any) error {
	if err := c.conn.SetWriteDeadline(deadlineOf(ctx)); err != nil {
		return err
	}
	return c.conn.WriteJSON(frame)
}

// Call sends frame and returns the reply carrying its id.
//
// An id is allocated when the frame has none, so a caller can write the frame it
// means and not think about numbering. Frames that arrive while waiting and do
// not carry this id are NOT answers — a CDP peer interleaves events with
// replies — so they are buffered for Recv rather than dropped.
func (c *Channel) Call(ctx context.Context, frame map[string]any) (string, error) {
	if err := c.acquire(ctx); err != nil {
		return "", err
	}
	defer c.release()

	if c.shut.Load() {
		return "", ErrClosed
	}
	id, ok := frame["id"]
	if !ok || id == nil {
		n := c.next.Add(1) - 1
		id = int(n)
		frame["id"] = id
	}
	if c.transport == WS {
		return c.wsCall(ctx, frame, id)
	}
	return c.httpCall(ctx, frame, id)
}

// acquire takes the one-slot call semaphore, or gives up when ctx does.
func (c *Channel) acquire(ctx context.Context) error {
	select {
	case c.callGate <- struct{}{}:
		return nil
	case <-c.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Channel) release() { <-c.callGate }

// Send writes frame and returns at once, without waiting for an answer: a
// notification has none, and a caller that wants to poll can ask for it with
// Recv instead of holding a call open.
func (c *Channel) Send(ctx context.Context, frame map[string]any) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	if c.shut.Load() {
		return ErrClosed
	}
	if c.transport == WS {
		return c.writeFrame(ctx, frame)
	}
	resp, err := c.httpPost(ctx, frame)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return err
	}
	// A notification is owed no answer, but a peer that answers anyway must not have
	// its frame dropped on the floor: Send is "do not wait", not "do not look".
	if body := strings.TrimSpace(string(raw)); body != "" {
		c.buffer(body)
	}
	return nil
}

// Recv returns the next buffered frame — one that arrived while a call was
// waiting for its own answer — and false when there is none. It never blocks:
// bytes are only buffered as a side effect of a Call or a Next, so a reader that
// wants new events has to wait for them.
func (c *Channel) Recv() (string, bool) {
	return c.pop()
}

// Take returns the next frame of ANY kind: a buffered one first, then whatever the
// socket delivers before ctx expires.
//
// Next deliberately skips answers, which is right for an event reader but wrong for
// a caller that paired Send with recv: that answer is the frame it is waiting for,
// and Next would file it into the buffer that only Recv reads. Take is the door that
// sees both, and it is what `recv` uses so a `send` can be answered.
//
// Buffered frames come first because they arrived first; the socket is only touched
// when nothing is already in hand.
func (c *Channel) Take(ctx context.Context) (string, bool) {
	if c.shut.Load() {
		return "", false
	}
	if frame, ok := c.pop(); ok {
		return frame, true
	}
	if c.transport != WS {
		return "", false
	}
	return c.awaitFrame(ctx)
}

// Next waits up to the context's deadline for the next frame that is NOT an
// answer to a call, and returns it.
//
// This is what makes recv worth having. An event arrives on its own schedule — a
// page finishing a load fires Page.loadEventFired AFTER Page.navigate answered —
// so a reader that only looks at what a previous call happened to catch sees
// nothing. Next reads the socket itself, so the wait is the caller's window and
// not a guess about how long a peer takes.
//
// On a channel with no push side (HTTP answers per request) it reports "nothing"
// immediately rather than waiting out a deadline that could not be met.
func (c *Channel) Next(ctx context.Context) (string, bool) {
	if c.shut.Load() {
		return "", false
	}
	if frame, ok := c.pop(); ok {
		return frame, true
	}
	if c.transport != WS {
		return "", false
	}
	// THE WAIT IS ON A CHANNEL, NOT ON THE SOCKET. A read deadline that fires leaves
	// gorilla's cached read error set forever, so one expired window made the channel
	// permanently deaf while the peer's frame sat unread on the wire.
	for {
		frame, ok := c.awaitFrame(ctx)
		if !ok {
			return "", false
		}
		if hasID(frame) {
			// An answer to a call nobody is waiting for is still data: keep it.
			c.buffer(frame)
			continue
		}
		return frame, true
	}
}

// pop takes the oldest buffered frame. The caller holds mu.
func (c *Channel) pop() (string, bool) {
	select {
	case frame := <-c.ev:
		return frame, true
	default:
		return "", false
	}
}

// Close shuts the channel. A socket is closed with a normal closure frame so the
// peer does not log an abnormal one.
func (c *Channel) Close() error {
	if !c.shut.CompareAndSwap(false, true) {
		return nil
	}
	close(c.done)
	if c.conn != nil {
		_ = c.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		return c.conn.Close()
	}
	return nil
}

// wsCall writes one frame and reads until its answer arrives.
func (c *Channel) wsCall(ctx context.Context, frame map[string]any, id any) (string, error) {
	if err := c.writeFrame(ctx, frame); err != nil {
		return "", err
	}
	return c.awaitAnswer(ctx, id)
}

// httpCall posts one frame and reads its answer.
//
// The answer is the response body when the peer answers in JSON, and one event
// when it answers on a text/event-stream body: streamable MCP is allowed to do
// either, and a server that streams is the case that used to be handled by
// reading the whole body and searching it afterwards — which hung whenever the
// stream stayed open for notifications, as a spec-compliant one does.
//
// ponytail: one POST sees only the events carried by its own response. Server
// messages that arrive while no request is open need the listening GET stream,
// which this does not open. Add it when a caller depends on unsolicited MCP
// notifications; a request/response peer (and CDP, which uses a socket) does not.
func (c *Channel) httpCall(ctx context.Context, frame map[string]any, id any) (string, error) {
	resp, err := c.httpPost(ctx, frame)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
		return string(raw), fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if !isEventStream(resp.Header.Get("Content-Type")) {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	br := bufio.NewReader(io.LimitReader(resp.Body, maxFrameBytes))
	for {
		payload, err := readEvent(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("the event stream ended before the reply to id %v arrived", id)
			}
			return "", err
		}
		if carriesID(payload, id) {
			return payload, nil
		}
		c.buffer(payload)
	}
}

// httpPost sends one frame and hands back the response, whose body is still
// open. The caller closes it.
func (c *Channel) httpPost(ctx context.Context, frame map[string]any) (*http.Response, error) {
	body, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid := c.sess.Load(); sid != nil && *sid != "" {
		req.Header.Set("Mcp-Session-Id", *sid)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sess.Store(&sid)
	}
	return resp, nil
}

// buffer keeps an unmatched frame for Recv, dropping the oldest past the cap.
//
// A non-blocking send: a full backlog means the reader is not draining, and the
// documented policy for that is "lose the oldest" — blocking here would stall the
// only socket reader instead.
func (c *Channel) buffer(frame string) {
	if frame == "" {
		return
	}
	for {
		select {
		case c.ev <- frame:
			return
		default:
		}
		// Full: drop the oldest and retry.
		select {
		case <-c.ev:
		default:
		}
	}
}

// deadlineOf is the read/write deadline for one call: the context's own, or the
// default cap when it has none.
func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(callTimeout)
}

// carriesID reports whether payload is a JSON-RPC message whose id is want.
//
// Ids are compared by their text rather than by type because a peer may echo `1`
// where the caller sent `1`, and a JSON-RPC peer is free to answer a numeric id
// as a string; refusing that would be a protocol-lawyer's answer to a question
// nobody asked. A message with no id is an EVENT, never an answer.
func carriesID(payload string, want any) bool {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return false
	}
	if len(probe.ID) == 0 || bytes.Equal(probe.ID, []byte("null")) {
		return false
	}
	return unquoteID(string(probe.ID)) == unquoteID(jsonText(want))
}

// hasID reports whether payload is a request or a response rather than an
// event: an event is the one shape with no id.
func hasID(payload string) bool {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return false
	}
	return len(probe.ID) != 0 && !bytes.Equal(probe.ID, []byte("null"))
}

// unquoteID strips the JSON quotes off a string id, so `"7"` and `7` compare
// equal.
func unquoteID(s string) string { return strings.Trim(s, `"`) }

// jsonText renders an id the way it went out on the wire.
func jsonText(v any) string {
	if s, ok := v.(string); ok {
		raw, _ := json.Marshal(s)
		return string(raw)
	}
	return fmt.Sprint(v)
}

// isEventStream reports whether a Content-Type is an SSE body.
func isEventStream(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/event-stream")
}

// readEvent reads one complete `data:` payload from an SSE body.
//
// Multi-line data is joined with newlines, which is what the spec says and what
// a pretty-printed JSON frame produces; a comment line (`:keepalive`) is skipped
// rather than concatenated, which is what makes a long-lived stream usable.
func readEvent(br *bufio.Reader) (string, error) {
	var data strings.Builder
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if payload, ok := sseDataLine(line); ok {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(payload)
			}
		} else if data.Len() > 0 {
			// The blank line that ends an event.
			return strings.TrimSpace(data.String()), nil
		}
		if err != nil {
			if data.Len() == 0 {
				return "", err
			}
			return strings.TrimSpace(data.String()), nil
		}
	}
}

// sseDataLine returns the payload of a `data:` line.
func sseDataLine(line string) (string, bool) {
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(payload), true
}
