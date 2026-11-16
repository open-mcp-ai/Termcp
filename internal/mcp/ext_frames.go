package mcp

// Frame surgery for the extension stream (issue #328).
//
// Three jobs, all of them byte-level rather than decode-and-re-encode:
//
//  1. Pull complete JSON-RPC frames out of a byte stream that also carries a
//     banner, a shell prompt, an echo and whatever else the peer printed.
//  2. Replace ONLY the `id` member of a frame, leaving every other byte where the
//     peer put it. Re-encoding through a struct would reorder members and change
//     number formatting, and a client comparing a reply to what its server said
//     would see a difference that is ours, not the server's.
//  3. Decide what a frame IS — notification, request, or answer — without
//     assuming the fields arrive in any particular order.

import (
	"bytes"
	"encoding/json"
	"strings"
)

// frameFields is the classification of one frame: enough to route it, no more.
type frameFields struct {
	// id is the raw JSON spelling of the id member, or nil when there is none.
	id json.RawMessage
	// method is the method member, empty for a frame that answers something.
	method string
}

// parseFrame reads the two members routing needs. A frame that is not JSON, or is
// not an object, reports ok=false and the caller treats it as noise.
func parseFrame(raw []byte) (frameFields, bool) {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return frameFields{}, false
	}
	return frameFields{id: probe.ID, method: probe.Method}, true
}

// nextFrame finds the first complete JSON-RPC message in buf.
//
// It returns the frame, how many bytes of buf it consumed (frame included, plus
// whatever noise preceded it), and whether more bytes are needed. `needMore` is
// the ONLY reason to keep buffering: a value that ended inside itself is a frame
// still arriving, while a complete value that is not a message is just output that
// happens to be JSON and is consumed and forgotten.
//
// A batch (a top-level array) is consumed and refused. 2025-06-18 has no batch,
// and `isMCPMessage` accepts the array form because the `read --frame` scanner has
// to hand a batch to a caller that asked for one — but this endpoint's routing has
// no per-element fan-out, and forwarding a batch as if it were a notification
// would deliver a client an array it never asked for.
func nextFrame(buf []byte) (frame []byte, consume int, needMore bool) {
	for i := 0; i < len(buf); i++ {
		if buf[i] != '{' && buf[i] != '[' {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(buf[i:]))
		var value json.RawMessage
		err := dec.Decode(&value)
		if err != nil {
			if isSyntaxEOF(err) || isUnexpectedEOF(err) {
				// The value ends past what has arrived. Everything before it is noise
				// and can go; the candidate itself must stay.
				return nil, i, true
			}
			// Not a value at all: a stray '{' in a log line. Move on.
			continue
		}
		end := i + int(dec.InputOffset())
		if !isMCPMessage(string(value)) {
			continue
		}
		return value, end, false
	}
	return nil, len(buf), false
}

// isUnexpectedEOF reports whether a decode stopped only because the input ended
// mid-value. It is isSyntaxEOF's sibling: that one asks the same question of an
// error the caller already knows is a JSON error, this one takes the error as it
// comes out of Decode.
func isUnexpectedEOF(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "unexpected EOF") ||
		strings.Contains(err.Error(), "unexpected end of JSON input"))
}

// isBatch reports whether a frame is the array form, which this endpoint refuses.
func isBatch(frame []byte) bool {
	for _, b := range frame {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

// rewriteFrameID replaces the top-level `id` member's value and changes nothing
// else.
//
// THE REST OF THE BYTES ARE THE PEER'S. A reply is forwarded verbatim because that
// is the promise of this endpoint: the client is talking to the extension, so the
// frame it reads must be the extension's own. Only the id is ours to touch — it is
// the one field the multiplexing rewrote on the way in.
func rewriteFrameID(frame, newID []byte) ([]byte, bool) {
	start, end, ok := topLevelIDSpan(frame)
	if !ok {
		return frame, false
	}
	out := make([]byte, 0, len(frame)-(end-start)+len(newID))
	out = append(out, frame[:start]...)
	out = append(out, newID...)
	out = append(out, frame[end:]...)
	return out, true
}

// topLevelIDSpan locates the value of the `id` member of a top-level object.
//
// A hand-written scan rather than a decode, for the reason rewriteFrameID gives:
// the caller needs the SPAN, and encoding/json does not report one. The scan
// tracks string escapes and bracket depth, so a nested object holding a comma does
// not end the member early.
func topLevelIDSpan(frame []byte) (int, int, bool) {
	i := 0
	skipWS := func() {
		for i < len(frame) {
			switch frame[i] {
			case ' ', '\t', '\r', '\n':
				i++
			default:
				return
			}
		}
	}
	skipString := func() bool { // frame[i] is the opening quote
		i++
		for i < len(frame) {
			switch frame[i] {
			case '\\':
				i += 2
				continue
			case '"':
				i++
				return true
			}
			i++
		}
		return false
	}

	skipWS()
	if i >= len(frame) || frame[i] != '{' {
		return 0, 0, false
	}
	i++
	for {
		skipWS()
		if i >= len(frame) || frame[i] == '}' || frame[i] != '"' {
			return 0, 0, false
		}
		keyStart := i
		if !skipString() {
			return 0, 0, false
		}
		keyEnd := i
		skipWS()
		if i >= len(frame) || frame[i] != ':' {
			return 0, 0, false
		}
		i++
		skipWS()
		valueStart := i
		if i < len(frame) && frame[i] == '"' {
			if !skipString() {
				return 0, 0, false
			}
		} else {
			depth := 0
			for i < len(frame) {
				c := frame[i]
				if c == '"' {
					if !skipString() {
						return 0, 0, false
					}
					continue
				}
				if c == '{' || c == '[' {
					depth++
					i++
					continue
				}
				if c == '}' || c == ']' {
					if depth == 0 {
						break
					}
					depth--
					i++
					continue
				}
				if depth == 0 && c == ',' {
					break
				}
				i++
			}
		}
		valueEnd := i
		if keyEnd-keyStart == 4 && string(frame[keyStart+1:keyEnd-1]) == "id" {
			return valueStart, valueEnd, true
		}
		skipWS()
		if i < len(frame) && frame[i] == ',' {
			i++
			continue
		}
		return 0, 0, false
	}
}

// jsonRPCError builds one error frame. It exists because the mcp package's
// encoder is unexported and this endpoint needs a frame to hand back when the peer
// cannot answer — the alternative, a nil return, means "notification" and would
// leave the client waiting.
func jsonRPCError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	frame, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"cannot encode error"}}`)
	}
	return frame
}
