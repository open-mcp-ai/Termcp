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
	"net"
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
// The mutating concerns each have a mechanism that fits it:
//
//   - serialising calls is a ONE-SLOT SEMAPHORE (callGate). It is acquired, used,
//     and released — never held while blocking on anything else.
//   - hand-off between the socket reader and callers is the DISPATCHER: one
//     goroutine that is the only reader of the wire, and that decides where each
//     frame goes. See dispatch below for why there cannot be two.
//   - the frame backlog is ev, a slice guarded by evMu, because the queue has to be
//     searched rather than popped: "the first frame with no id" is a filter, and a
//     channel can only be drained.
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

	// ev holds frames no caller was waiting for: events, and answers to calls that
	// already gave up. A slice rather than a channel because readers FILTER it —
	// "the next frame with no id" and "anything at all" are different questions
	// about the same queue, and popping to look is how a frame gets destroyed by a
	// reader that did not want it.
	evMu sync.Mutex
	ev   []Framed

	// waiters are the calls in flight, by id. The dispatcher hands an answer
	// straight to the call that asked for it, which is the whole point of being the
	// only reader: the match is decided in one place instead of raced for.
	waitMu sync.Mutex
	wait   map[string]chan Framed

	// log is the receive history: every frame, numbered, retained in a bounded
	// ring. The ev queue above is a BACKLOG (read once, then gone); this is the
	// other question — "what arrived after n" — which has to stay answerable twice
	// because CDP never replays an event.
	log *frameLog

	// shut reports that Close ran.
	shut atomic.Bool

	// sseStarted reports that the listening GET stream has been dispatched. HTTP
	// only: streamable MCP carries unsolicited server messages on a GET
	// text/event-stream, so without it a peer can push only inside the body of a
	// POST the caller happened to be waiting on.
	sseStarted atomic.Bool

	// sseRefused reports that the peer answered the listening GET with a permanent
	// refusal (405/404), so it has no stream to push on and nothing will ever arrive
	// there.
	//
	// IT IS NOT THE SAME QUESTION AS "IS THE STREAM UP". A reader that asked
	// "is the stream up RIGHT NOW" got false for the whole handshake — which is a
	// few hundred microseconds during which a pushed frame is perfectly able to
	// arrive — and returned "no frame" at once instead of waiting for it (issue
	// #213). The wait asks whether a frame COULD still arrive, so the refusal is
	// what it watches, and the handshake is allowed to finish underneath it.
	sseRefused atomic.Bool

	// sseCancel stops the listening stream on Close, without waiting for a read
	// deadline that a blocked SSE body does not honour.
	sseCancel context.CancelFunc

	// readErr carries why the socket stopped; done is closed by Close, releasing
	// blocked readers and senders; dead is closed by the read loop when the socket
	// stops delivering, so a waiter learns the difference between "nothing yet" and
	// "nothing ever again" without waiting out its whole window.
	readErr chan error
	done    chan struct{}
	dead    chan struct{}
	deadOne sync.Once

	// evCond is signalled when ev gains a frame. The dispatcher appends; readers
	// wait on it. A condition variable rather than a channel because a reader
	// FILTERS the queue ("the next frame with no id") and must put back what it did
	// not want, which a channel cannot do without spinning.
	evCond *sync.Cond
}

// deadlineFreeConn wraps a net.Conn whose SetDeadline is unimplemented.
//
// THE SSH PACKAGE DOES NOT IMPLEMENT DEADLINES, and the net.Conn it returns answers
// both SetDeadline methods with "ssh: tcpChan: deadline not supported". Callers that
// set a deadline and check the error — gorilla's websocket dialer does exactly this —
// therefore fail against an SSH tunnel with a message about the transport rather than
// about anything the caller did.
//
// The deadline is what is being dropped, not the timeout. Cancellation still works,
// because every dial through this seam already carries a context: the SSH channel is
// torn down when that context ends, which is the guarantee the deadline was reaching
// for. Pretending to accept a deadline we cannot honour is the lesser evil here —
// the alternative is refusing every websocket tunnel outright, and a websocket over
// SSH is the main reason the seam exists.
// NoDeadlineConn wraps conn so deadline calls succeed silently.
func NoDeadlineConn(c net.Conn) net.Conn { return deadlineFreeConn{c} }

type deadlineFreeConn struct{ net.Conn }

func (c deadlineFreeConn) SetDeadline(time.Time) error      { return nil }
func (c deadlineFreeConn) SetReadDeadline(time.Time) error  { return nil }
func (c deadlineFreeConn) SetWriteDeadline(time.Time) error { return nil }

// DialFunc opens a TCP connection to addr. It is the single seam through which a
// gateway reaches its peer.
//
// THE URL IS NOT RESOLVED HERE. Every address a gateway dials — "localhost:9222",
// "mcp.internal:8080" — is a name on the network OF THE HOST THE SESSION POINTS AT,
// not on the network of the termcp process. A devtools port bound to 127.0.0.1 on a
// remote box is not reachable from here and never was; the whole point of running an
// SSH session to that box is that its loopback is the loopback the URL means.
//
// Dialling from the process instead would be a second, silent rule: the same URL
// would work or fail depending on where termcp happens to run, and a caller could
// only discover which by trying. So the session is not a convenience parameter — it
// is what the URL is relative to.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Transport timeouts. They are named rather than left as zero values because a
// zero IdleConnTimeout keeps dead sockets forever and a zero TLSHandshakeTimeout
// lets a half-open peer hold a call until the caller's context expires.
const (
	maxIdleConn           = 30 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// Open connects rawURL over dial, choosing the transport by scheme: http(s) posts
// one frame per request, ws(s) opens a socket.
//
// dial is required. A nil dial is refused rather than defaulted to the process's own
// network, because that default is exactly the silent second rule DialFunc describes:
// it would make `mcp open url` mean "connect from termcp" for some callers and
// "connect from the host" for others, with nothing in the request to tell them apart.
func Open(ctx context.Context, rawURL string, dial DialFunc) (*Channel, error) {
	if dial == nil {
		return nil, fmt.Errorf("no dialer: a gateway reaches its peer through the session's SSH connection, so a session is required")
	}
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("not a URL: %s", rawURL)
	}
	c := &Channel{
		url:      rawURL,
		callGate: make(chan struct{}, 1),
		wait:     make(map[string]chan Framed),
		done:     make(chan struct{}),
		log:      newFrameLog(maxEvents*8, 8<<20),
	}
	c.evCond = sync.NewCond(&c.evMu)
	// The default transport dials from this process. Replacing its DialContext is the
	// whole of the change: every request the channel makes, and every socket it opens,
	// then travels inside the SSH connection.
	c.client = &http.Client{Transport: &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          4,
		IdleConnTimeout:       maxIdleConn,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}}
	switch u.Scheme {
	case "ws", "wss":
		c.transport = WS
		d := *websocket.DefaultDialer
		d.NetDialContext = dial
		conn, _, err := d.DialContext(ctx, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", rawURL, err)
		}
		c.conn = conn
		c.readErr = make(chan error, 1)
		c.dead = make(chan struct{})
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
// connection — and therefore also the DISPATCHER: it decides, once, where each
// frame goes.
//
// gorilla permits exactly one concurrent reader, and one reader that never stops is
// also what keeps read deadlines off the socket: a wait now happens on a channel or
// on the caller's context, never inside a read, so a window that expires leaves
// nothing behind. The goroutine ends when the socket fails or Close runs — a socket
// close makes the blocked read return, which is the only way out of it.
//
// THE ROUTING IS THE POINT. Two consumers want frames from this socket — a Call
// waiting for its own answer, and a reader (Take/Next/Recv) wanting the next frame of
// some shape — and they used to compete for the same channel. That is a race the
// caller loses silently: a Take issued while a call is in flight can lift the answer
// out of the queue first, and the call then waits out its window and reports "no
// answer" while the frame sits in a reader that did not ask for it. Deciding here, by
// id, is what makes the two questions independent: an answer goes to the call that
// asked for it, and everything else goes to the backlog.
//
// A frame is recorded in the log BEFORE any of that, so the history stays complete
// whatever the routing does with the copy.
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
			// The socket is gone: a waiter learns it here instead of waiting out its
			// whole window for an answer that can no longer arrive. (Closing on the
			// channel rather than broadcasting, because a broadcast only wakes a parked
			// reader once — a later call would park again and time out.)
			c.deadOne.Do(func() { close(c.dead) })
			// A reader parked on the backlog has to learn that nothing more is
			// coming, or it waits out its whole window for a frame that cannot arrive.
			c.evCond.Broadcast()
			return
		}
		payload := strings.TrimSpace(string(raw))
		if payload == "" {
			continue
		}
		// RECORDED AT THE WIRE, before anything consumes it. Every later reader
		// (the destructive read, a call waiting for its answer) sees a subset or
		// a copy of this; numbering here is what makes `since n` mean "what the
		// peer sent after n" rather than "what was still queued".
		c.dispatch(c.record(payload))
	}
}

// dispatch routes one frame: to the call waiting for its id, or to the backlog.
//
// AN ANSWER NOBODY IS WAITING FOR IS NOT RUBBISH. It goes to the backlog like an
// event, because it is still the frame a later reader is owed: a peer answers a
// `send`, or a call gave up just before its answer landed, and the only other copy
// is the one the wire already delivered.
func (c *Channel) dispatch(rec Framed) {
	if rec.Event {
		c.buffer(rec)
		return
	}
	key := idKey(rec.Data)
	if key == "" {
		c.buffer(rec)
		return
	}
	c.waitMu.Lock()
	ch, waiting := c.wait[key]
	c.waitMu.Unlock()
	if !waiting {
		c.buffer(rec)
		return
	}
	// A one-slot channel and a non-blocking send: the waiter is either parked on it
	// or already gone, and a blocked dispatcher would stall the only socket reader.
	select {
	case ch <- rec:
	default:
		c.buffer(rec)
	}
}

// idKey renders a frame's id for map lookup, or "" when it has none.
//
// The text form is what makes `1` and `"1"` the same key: a JSON-RPC peer may echo
// a numeric id as a string, and refusing that would be a protocol-lawyer's answer to
// a question nobody asked.
func idKey(payload string) string {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return ""
	}
	if len(probe.ID) == 0 || bytes.Equal(probe.ID, []byte("null")) {
		return ""
	}
	return unquoteID(string(probe.ID))
}

// waitKey renders an id VALUE the same way idKey renders one read off the wire, so a
// waiter registered under a key matches the frame that carries it. The two sides have
// to agree on the spelling: this side has the value it put in the frame, the other has
// the JSON text the peer echoed.
func waitKey(want any) string {
	switch v := want.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return unquoteID(jsonText(want))
	}
}

// awaitAnswer waits for the frame carrying want, up to ctx.
//
// send, when given, is called AFTER the waiter is registered and before the wait — the
// order a call needs (see wsCall). A send that fails is the call's error.
//
// THE ANSWER IS DELIVERED, NOT COMPETED FOR. This registers a one-slot channel under
// the id and lets the dispatcher hand the frame to it, so a concurrent Take cannot
// lift it out from underneath: the routing decision was already made by id, in one
// place. What this used to do was read the shared queue itself and put back what it
// did not want, which meant an answer could be taken by whoever read first.
func (c *Channel) awaitAnswer(ctx context.Context, want any, send func() error) (string, error) {
	key := waitKey(want)
	if key == "" {
		// No id on the frame we sent, so no answer can be matched to it. That is a
		// notification, and Call on one is a caller error rather than a wait.
		return "", errors.New("the frame carries no id, so no answer can be matched to it")
	}
	ch := make(chan Framed, 1)
	c.waitMu.Lock()
	c.wait[key] = ch
	c.waitMu.Unlock()
	// DEREGISTERING IS WHERE A DEADLINE RACE IS MADE HARMLESS. The dispatcher can
	// deliver in the same instant the window closes; if this simply returned, that
	// frame would sit in a channel nobody will ever read again and the result would
	// be gone. So whatever arrived is put back into the backlog on the way out,
	// where a later Take can still find it.
	defer func() {
		c.waitMu.Lock()
		delete(c.wait, key)
		c.waitMu.Unlock()
		select {
		case rec := <-ch:
			c.buffer(rec)
		default:
		}
	}()
	if send != nil {
		if err := send(); err != nil {
			return "", err
		}
	}
	select {
	case rec := <-ch:
		return rec.Data, nil
	case <-c.dead:
		// The socket stopped: an answer can no longer arrive, and this is NOT a
		// timeout — the caller has to be able to tell "the peer went away" from "the
		// peer is thinking".
		return "", c.readFailure()
	case <-c.done:
		return "", ErrClosed
	case <-ctx.Done():
		// The window expired with no answer. Nothing was damaged, so a later call
		// can still use the channel.
		return "", ctx.Err()
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
		rec := c.record(body)
		c.buffer(rec)
	}
	return nil
}

// Recv returns the next buffered frame — one that arrived while a call was
// waiting for its own answer — and false when there is none. It never blocks:
// bytes are only buffered as a side effect of a Call or a Next, so a reader that
// wants new events has to wait for them.
func (c *Channel) Recv() (string, bool) {
	if f, ok := c.pop(); ok {
		return f.Data, true
	}
	return "", false
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
	if f, ok := c.TakeFramed(ctx); ok {
		return f.Data, true
	}
	return "", false
}

// TakeFramed is Take returning the whole Framed record: data, sequence number, and
// whether it is an event. That sequence number is what lets the caller report the
// cursor it just consumed.
func (c *Channel) TakeFramed(ctx context.Context) (Framed, bool) {
	if c.shut.Load() {
		return Framed{}, false
	}
	if frame, ok := c.pop(); ok {
		return frame, true
	}
	if !c.CanWaitEvents() {
		return Framed{}, false
	}
	// Wait on the backlog until a frame arrives, Close runs, or ctx expires.
	c.evMu.Lock()
	defer c.evMu.Unlock()
	for len(c.ev) == 0 {
		// THE REFUSAL IS CHECKED INSIDE THE LOOP, not only before it: while this
		// wait was parked the peer may have answered the listening GET with
		// "no" — a fact this wait has to act on rather than spend its window on.
		if c.shut.Load() || ctx.Err() != nil || c.socketGone() || c.sseRefused.Load() {
			return Framed{}, false
		}
		// A helper goroutine releases the condition when ctx or done closes, so a
		// wait can end on the caller's deadline.
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				c.evCond.Broadcast()
			case <-c.done:
				c.evCond.Broadcast()
			case <-stop:
			}
		}()
		c.evCond.Wait()
		close(stop)
		if len(c.ev) > 0 {
			break
		}
		if c.shut.Load() || ctx.Err() != nil || c.socketGone() || c.sseRefused.Load() {
			return Framed{}, false
		}
	}
	frame := c.ev[0]
	c.ev = c.ev[1:]
	return frame, true
}

// Next waits up to the context's deadline for the next frame that is NOT an
// answer to a call, and returns it.
//
// SKIPPING IS NOT CONSUMING. An answer to a send is owed to a later Take, so Next
// leaves it in the backlog rather than dropping it or putting it back at the end of
// the queue. The queue is a slice rather than a channel precisely for this: finding
// the first event is a filter, and what is not taken stays where it was.
func (c *Channel) Next(ctx context.Context) (string, bool) {
	if c.shut.Load() {
		return "", false
	}
	if frame, ok := c.popEvent(); ok {
		return frame.Data, true
	}
	if !c.CanWaitEvents() {
		return "", false
	}
	c.evMu.Lock()
	defer c.evMu.Unlock()
	for {
		for i, f := range c.ev {
			if f.Event {
				c.ev = append(c.ev[:i], c.ev[i+1:]...)
				return f.Data, true
			}
		}
		if c.shut.Load() || ctx.Err() != nil || c.socketGone() || c.sseRefused.Load() {
			return "", false
		}
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				c.evCond.Broadcast()
			case <-c.done:
				c.evCond.Broadcast()
			case <-stop:
			}
		}()
		c.evCond.Wait()
		close(stop)
		if c.shut.Load() || ctx.Err() != nil {
			return "", false
		}
	}
}

// popEvent extracts the oldest event frame, leaving any answers in place.
func (c *Channel) popEvent() (Framed, bool) {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	for i, f := range c.ev {
		if f.Event {
			c.ev = append(c.ev[:i], c.ev[i+1:]...)
			return f, true
		}
	}
	return Framed{}, false
}

// socketGone reports that the read loop has stopped, so no further frame can
// arrive on this channel.
func (c *Channel) socketGone() bool {
	if c.dead == nil {
		return false
	}
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// pop takes the oldest buffered frame.
func (c *Channel) pop() (Framed, bool) {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	if len(c.ev) == 0 {
		return Framed{}, false
	}
	frame := c.ev[0]
	c.ev = c.ev[1:]
	return frame, true
}

// Close shuts the channel. A socket is closed with a normal closure frame so the
// peer does not log an abnormal one, when write access is free.
func (c *Channel) Close() error {
	if !c.shut.CompareAndSwap(false, true) {
		return nil
	}
	close(c.done)
	if c.sseCancel != nil {
		c.sseCancel()
	}
	if c.conn != nil {
		// THE WRITE GATE IS HONOURED, NOT RACED WITH. gorilla/websocket forbids
		// concurrent writers, so sending CloseMessage while a Call/Send is in flight
		// races on the connection's writer. If the gate is available, send the polite
		// close frame; if it is occupied, skip the frame and drop the socket directly.
		select {
		case c.callGate <- struct{}{}:
			_ = c.conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			<-c.callGate
		default:
		}
		return c.conn.Close()
	}
	return nil
}

// wsCall registers the waiter, writes one frame, and reads until its answer arrives.
//
// THE ORDER IS THE WHOLE FUNCTION: REGISTER FIRST, THEN WRITE.
//
// It used to write and then wait, and the gap between the two is a window in which the
// peer can answer. The dispatcher decides where a frame goes by looking up `wait[id]`, so
// an answer arriving inside that window found no waiter and went to the backlog — where
// nothing would read it — while the call waited for a frame that had already come.
// Observed as a 10-minute hang of `TestChannelRecordsEveryFrameAtTheWire` on one ubuntu
// runner: the peer is an in-process websocket server, so "answers faster than the caller
// can register" is reachable, and a caller whose context carries no deadline waits
// forever rather than being told anything.
//
// Registering first removes the window rather than narrowing it.
func (c *Channel) wsCall(ctx context.Context, frame map[string]any, id any) (string, error) {
	return c.awaitAnswer(ctx, id, func() error { return c.writeFrame(ctx, frame) })
}

// CanWaitEvents reports whether a frame could still arrive on this channel that no
// call is waiting for: always true on a socket, and true on HTTP once the listening
// stream has been dispatched and the peer has not refused it.
//
// IT DELIBERATELY DOES NOT REQUIRE THE STREAM TO BE UP YET. Waiting for the
// handshake before allowing a wait would make the first read after `open` lose the
// race against it and answer "no frame" while the frame was in flight (issue #213).
func (c *Channel) CanWaitEvents() bool {
	return c.transport == WS || (c.sseStarted.Load() && !c.sseRefused.Load())
}

// ensureSSEStream starts the listening GET stream for an HTTP channel once a
// session id is known.
//
// THE STREAM OPENS AFTER INITIALIZE, NOT BEFORE. Streamable MCP assigns the
// session on the first POST (initialize), and opening a GET before that makes a
// spec-compliant server mint a SECOND session rather than attaching to the one
// being initialized.
func (c *Channel) ensureSSEStream(sessionID string) {
	if !c.sseStarted.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.sseCancel = cancel
	go c.readSSEStream(ctx, sessionID)
}

// readSSEStream maintains the long-lived GET text/event-stream carrier for
// server-initiated messages.
//
// UNSOLICITED NOTIFICATIONS BELONG TO THIS STREAM. A streamable HTTP MCP server
// delivers notifications and progress here while requests run over separate
// POST calls; without this stream those frames are permanently lost.
func (c *Channel) readSSEStream(ctx context.Context, sessionID string) {
	for !c.shut.Load() && ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Mcp-Session-Id", sessionID)
		resp, err := c.client.Do(req)
		if err != nil {
			if ctx.Err() != nil || c.shut.Load() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			// A server that refuses GET text/event-stream (405 Method Not Allowed
			// or 404) does not speak streamable MCP; do not hammer it.
			if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotFound {
				c.sseRefused.Store(true)
				// WAKE THE PARKED READERS: "nothing will ever arrive here" is a
				// change they asked to be told about, and a waiter that is not woken
				// spends the rest of its window on a stream that already said no.
				c.evCond.Broadcast()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-time.After(time.Second):
				continue
			}
		}

		br := bufio.NewReader(resp.Body)
		for {
			payload, err := readEvent(br, maxFrameBytes)
			if err != nil {
				_ = resp.Body.Close()
				break
			}
			if payload == "" {
				continue
			}
			c.dispatch(c.record(payload))
		}
	}
}

// httpCall posts one frame and reads its answer.
//
// The answer is the response body when the peer answers in JSON, and one event
// when it answers on a text/event-stream body: streamable MCP is allowed to do
// either, and a server that streams is the case that used to be handled by
// reading the whole body and searching it afterwards — which hung whenever the
// stream stayed open for notifications, as a spec-compliant one does.
//
// Server-initiated messages (unsolicited notifications, progress) are delivered
// over the listening GET stream (see ensureSSEStream) which opens once a session
// id exists; frames carried there land in the backlog and are readable via Take/Next.
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
		body := strings.TrimSpace(string(raw))
		c.record(body)
		return body, nil
	}
	br := bufio.NewReader(io.LimitReader(resp.Body, maxFrameBytes))
	for {
		payload, err := readEvent(br, maxFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("the event stream ended before the reply to id %v arrived", id)
			}
			return "", err
		}
		rec := c.record(payload)
		if carriesID(payload, id) {
			return payload, nil
		}
		c.buffer(rec)
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
		c.ensureSSEStream(sid)
	} else if existing := c.sess.Load(); existing != nil && *existing != "" {
		c.ensureSSEStream(*existing)
	}
	return resp, nil
}

// buffer keeps an unmatched frame for a later reader, dropping the oldest past the
// cap.
//
// A slice rather than a channel because readers FILTER it: "the next frame with no
// id" and "anything at all" are two questions about one queue, and a queue that can
// only be popped is how a frame gets destroyed by a reader that did not want it.
//
// The cap is the documented policy for a reader that is not draining: lose the
// oldest, never block the only socket reader.
func (c *Channel) buffer(frame Framed) {
	if frame.Data == "" {
		return
	}
	c.evMu.Lock()
	c.ev = append(c.ev, frame)
	if len(c.ev) > maxEvents {
		c.ev = append(c.ev[:0], c.ev[len(c.ev)-maxEvents:]...)
	}
	c.evCond.Broadcast()
	c.evMu.Unlock()
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
//
// `max` IS PER EVENT, NOT PER CONNECTION. A listening stream lives for as long as
// the channel does and is expected to deliver an unbounded number of frames, so a
// limit wrapped around the body would end the stream once the total passed it —
// silently turning "the peer has been chatty for an hour" into "the peer stopped
// talking" (issue #213). Bounding each frame is the thing the limit was for.
func readEvent(br *bufio.Reader, max int) (string, error) {
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
				if max > 0 && data.Len() > max {
					return "", fmt.Errorf("an event exceeded %d bytes", max)
				}
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
