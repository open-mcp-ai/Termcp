package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// A server sometimes ASKS the client something — sampling/createMessage is the
// reason this exists: the program behind the server has no model of its own and
// uses the client's.
//
// That reverses the normal direction, and it is why a response has to be routable.
// A pending map is the whole mechanism: mint an id, remember where the answer
// goes, write the request down the client's stream, and let the frame that comes
// back with that id wake the waiter.

// defaultRequestTimeout bounds one server-initialised request. A client that
// never answers must not pin the goroutine that asked: the sub-MCP on the other
// side is waiting too, and an answer of "no" is one it can act on.
const defaultRequestTimeout = 5 * time.Minute

// client is the per-session state both transports share: an outbound channel, a
// table of requests waiting for answers, and the id counter that pairs them.
//
// It is embedded rather than inherited because Go has no inheritance; the two
// transports differ only in WHEN they can send (the SSE one always can, the
// streamable one only while a GET stream is open) and that difference is one
// method override.
type client struct {
	id string

	// write sends one frame to the client. It is the transport's job to know how;
	// returning an error means "this client cannot be reached right now".
	write func(ctx context.Context, frame []byte) error

	// pending holds one channel per outstanding request, keyed by id string.
	pending sync.Map

	requestSeq atomic.Uint64
	closed     atomic.Bool

	// clientInfo is what the client said about itself in initialize. It is
	// recorded rather than required: the field is optional in the handshake, and a
	// client that omits it is still a client.
	infoMu     sync.Mutex
	clientInfo Implementation
}

// noteClientInfo records what the client called itself.
func (c *client) noteClientInfo(info Implementation) {
	c.infoMu.Lock()
	c.clientInfo = info
	c.infoMu.Unlock()
}

// GetClientInfo returns what the client called itself, or zero values when it
// said nothing. A caller that displays this must tolerate empty strings.
func (c *client) GetClientInfo() Implementation {
	c.infoMu.Lock()
	defer c.infoMu.Unlock()
	return c.clientInfo
}

// SessionID identifies this client to a registry.
func (c *client) SessionID() string { return c.id }

// Notify delivers an unsolicited notification.
func (c *client) Notify(ctx context.Context, method string, params any) error {
	frame, err := json.Marshal(Notification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.write(ctx, frame)
}

// Request sends a request to the client and waits for its answer.
//
// The returned error is "the client said no" or "the client said nothing"; both
// are outcomes a caller can report, and collapsing them would hide which.
func (c *client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("mcpv1: session %s is closed", c.id)
	}
	// A string id, because a client's own ids are usually numbers and the two
	// spellings never collide.
	rawID := json.RawMessage(fmt.Sprintf(`"srv-%d"`, c.requestSeq.Add(1)))

	payload, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	frame, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: rawID, Method: method, Params: payload})
	if err != nil {
		return nil, err
	}

	answer := make(chan *Response, 1)
	c.pending.Store(IDString(rawID), answer)
	defer c.pending.Delete(IDString(rawID))

	if err := c.write(ctx, frame); err != nil {
		return nil, err
	}

	// The deadline is a hang detector, not a budget: an answer that arrives in
	// 10ms returns in 10ms.
	timer := time.NewTimer(defaultRequestTimeout)
	defer timer.Stop()
	select {
	case res := <-answer:
		if res == nil {
			return nil, fmt.Errorf("mcpv1: the client vanished before answering %s", method)
		}
		if res.Error != nil {
			return nil, fmt.Errorf("client refused %s: %s (code %d)", method, res.Error.Message, res.Error.Code)
		}
		if raw, ok := res.Result.(json.RawMessage); ok {
			return raw, nil
		}
		return json.Marshal(res.Result)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("mcpv1: the client did not answer %s within %s", method, defaultRequestTimeout)
	}
}

// deliver routes an inbound frame that is an ANSWER rather than a request.
//
// A frame with an id and no method can only be a response; a request always names
// its method. Reporting an answer nobody asked for is left to the transport: it
// means the client's state and the server's have diverged.
func (c *client) deliver(res *Response) bool {
	key := IDString(res.ID)
	if key == "" {
		return false
	}
	ch, ok := c.pending.LoadAndDelete(key)
	if !ok {
		return false
	}
	select {
	case ch.(chan *Response) <- res:
	default:
	}
	return true
}

// closeAll wakes every waiter with nothing, so no goroutine outlives the session.
func (c *client) closeAll() {
	c.closed.Store(true)
	c.pending.Range(func(k, v any) bool {
		ch, ok := v.(chan *Response)
		if ok {
			select {
			case ch <- nil:
			default:
			}
		}
		c.pending.Delete(k)
		return true
	})
}

// isResponseFrame reports whether a frame answers an outstanding request. A
// request always has a method; a response never does.
func isResponseFrame(fields map[string]json.RawMessage) bool {
	_, hasMethod := fields["method"]
	if hasMethod {
		return false
	}
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	return hasResult || hasError
}
