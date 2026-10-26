package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// The mcp directive's stdio form: an MCP server running ON a shell channel.
//
// This is the half of `mcp` that is not a transport. A handle that names a shell
// is a process the server already started (`pipe mac --argv npx -y …`), and
// driving it is typing a frame at it and reading the answer back — no socket, no
// HTTP, and nothing to open. The URL forms live in term_jrpc.go, where the three
// frame-channel doors share one implementation.
//
// Keeping this here is what lets `mcp` stay one directive after gaining a
// registry: the handle decides which half runs, and a caller does not have to
// know which kind it holds.

// mcpStdio speaks to the process running on a shell channel.
//
// A stdio peer is just a line-oriented pipe: `run` writes the frame, `read` gets the
// reply. That is the whole implementation, and it is why an MCP server needs no
// registration ceremony — `pipe --argv` starts it, and this types at it.
func (s *Server) mcpStdio(ctx context.Context, stmt directive.Statement, sub string, handle string, rest []string, win termArgs) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	switch sub {
	case "tools", "send", "call":
		var frame string
		switch sub {
		case "tools":
			frame = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		default:
			payload := unquoteWord(strings.TrimSpace(strings.Join(rest, " ")))
			if payload == "" {
				return badRequest(stmt, "mcp "+sub+" needs a JSON-RPC frame")
			}
			// VALIDATE BEFORE WRITING, the same way the URL forms already do.
			//
			// A stdio peer reads one frame per line, so anything unparseable is either
			// ignored or answered with a parse error — and the caller saw 202 "sent"
			// either way, which says the frame landed when in fact it was never a frame.
			// `cdp send 'not json'` already answered 400; the two used to disagree about
			// what "sent" means.
			if !json.Valid([]byte(payload)) {
				return badRequest(stmt, "the frame is not JSON\n"+frameSpelling)
			}
			frame = payload
		}
		// One frame, one line: the peer reads line-delimited JSON-RPC.
		if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", handle, "text", frame)); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", handle, "key", "enter")); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		// `call` WAITS FOR THE REPLY IT CAUSED; `send` DOES NOT.
		//
		// A URL peer's `call` already waits (peerChannel sets a deadline for it), while
		// this stdio branch wrote the frame and returned at once — so the same verb
		// behaved differently depending on how the peer had been started, and the caller
		// could not see which it had. Every MCP round trip against a stdio server cost
		// two calls, and the second one needed the caller to correlate replies by hand:
		// replies arrive in completion order, so the second recv may answer the second
		// request (issue #175).
		//
		// The correlation is here rather than in the caller's head because the id is
		// already in the frame this function just wrote. A frame whose id does NOT match
		// is still returned — it is a notification or another request's reply, and
		// dropping it would be data loss — but the reply says which one it is, so the
		// caller is never silently handed an answer to a question it did not ask.
		if sub == "send" || boolFlag(stmt, "send") {
			return envelope.Slot{Code: 202, Headers: headers, Body: "sent; read the reply with `read " + handle + " --frame`"}
		}
		if id := frameID(frame); id != "" {
			return s.mcpAwaitReply(ctx, stmt, handle, id, win, headers)
		}
		// A notification has no answer to wait for, so waiting would be a lie.
		return envelope.Slot{Code: 202, Headers: headers, Body: "sent (a notification has no reply); read frames with `read " + handle + " --frame`"}
	case "recv":
		return s.mcpRecvFrame(ctx, stmt, handle, win)
	case "close":
		// The cursor has to go with the handle: handles are minted monotonically
		// (jrpcHandleSeq), so a reopened channel never reuses this key and the map
		// would grow by one entry per channel the process ever opened.
		s.dropRecvCursor(handle)
		return envelope.Slot{Code: 204, Headers: headers}
	}
	return badRequest(stmt, "unknown mcp subcommand: "+sub)
}

// mcpRecvFrame reads exactly one JSON-RPC frame and advances this handle's cursor
// past it.
//
// The "exactly one" part is the contract: a frame is complete when the bytes parsed
// as JSON, and a partial one must be held rather than acted on. The read is
// therefore retried while the buffer does not yet parse — never a blocking read
// that may return half a message.
//
// The cursor is the whole point, and it is kept HERE rather than left to
// shell_output. shell_output has two modes and neither is a queue consumer: with
// an offset it is a stateless positional read, and without one it mints a NEW
// reader at the current end of the stream. A recv that passed neither therefore
// read from "now" on every call, so a first call that ended mid-frame reported
// 204 — and the next call started at the new end, skipping the frames that had
// been waiting behind it. Measured on three queued replies: recv delivered one and
// silently dropped the other two.
//
// With an explicit offset read the position is unambiguous and stateless on the
// server side, so the map below is the only state and it advances by exactly the
// frame that was returned. Bytes between frames (the newlines a line-delimited
// peer writes) are skipped by the trim in firstMCPMessage, and the offset advances
// over them too, so a resumed read starts at the next '{'.
func (s *Server) mcpRecvFrame(ctx context.Context, stmt directive.Statement, handle string, win termArgs) envelope.Slot {
	// ONE READER AT A TIME, FOR THE WHOLE READ (#183). The lock spans the poll loop
	// because that is where the duplication happens: two readers that both start
	// before the frame arrives both find it. It is held across the recursive call's
	// setup but NOT across the recursion itself, which re-enters here.
	reader := s.recvReader(handle)
	reader.Lock()
	defer reader.Unlock()
	for {
		slot, readAgain := s.mcpRecvFrameOnce(ctx, stmt, handle, win)
		if !readAgain {
			return slot
		}
		// The sub-MCP sampling request was answered underneath and the caller still
		// has no frame, so another one is taken — INSIDE the lock, because this
		// reader is still the one reading.
	}
}

// mcpRecvFrameOnce is one frame, with the caller holding this handle's reader lock.
// readAgain asks for another read because the frame it took was answered on the
// caller's behalf (a sub-MCP's sampling request) rather than delivered.
func (s *Server) mcpRecvFrameOnce(ctx context.Context, stmt directive.Statement, handle string, win termArgs) (envelope.Slot, bool) {
	from := s.recvCursor(handle)
	// A whole frame has to fit the window, or a single large reply could never be
	// delivered: the window is a cap on what one recv will consider, not a
	// statement about how long a frame may be.
	window := defaultBytesLimit
	if window < minRecvWindow {
		window = minRecvWindow
	}
	// POLL, BECAUSE THE READ ITSELF DOES NOT WAIT FOR AN OFFSETTED WINDOW.
	//
	// shell_output's timeout is honoured only by its live-cursor mode; the
	// offset-addressed mode returns what is buffered right now, and nothing else. A
	// recv that asked once therefore answered "no complete frame yet" while the peer's
	// bytes were still in flight — and a caller had to guess how long to keep asking.
	// The retry loop is here, with a bound, so one recv means one frame rather than
	// one sample.
	//
	// THE BOUND IS THE CALLER'S WINDOW, not this file's constant.
	//
	// It used to be a hardcoded 2s, which made 204 unreadable: "nothing has arrived
	// yet" and "the peer is slow" were the same answer, and an MCP server over stdio
	// routinely takes longer than 2s for a tools/call that does real work. The caller
	// was told "no complete frame yet" while the reply was in flight, so the only way
	// to get it was to poll — and every poll is a whole assistant turn, which costs
	// ~1000× what the call does (issue #174).
	//
	// The peer-call path right next door already answers to the caller
	// (peerCallWindow); two waits in one file answering to different clocks was the
	// bug. Only the fallback differs from a peer call, and only because the verbs
	// differ: a caller that names no window gets the READ default (recvWindow), not
	// the peer default — fifteen seconds of silence is not what reading a channel
	// means, and it is not what this directive did before.
	deadline := time.Now().Add(recvWindow(win))
	var doc map[string]any
	var body string
	for {
		res, fail := s.callTool(ctx, "shell_output", argsOf(
			"shell_id", handle, "offset", float64(from),
			"max_bytes", float64(window), "timeout", float64(0), "strip_ansi", false))
		if fail != nil {
			return s.slotFromFailure(ctx, stmt, fail), false
		}
		var ok bool
		doc, ok = toolResultMap(res)
		if !ok {
			return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)}), false
		}
		body = getString(doc, "output", "")
		if _, complete := firstMCPMessage(body); complete {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: ctx.Err().Error()}), false
		case <-time.After(recvPollInterval):
		}
	}
	// rawLen, not len(body): the window's byte span is what the next read's offset
	// is measured in. len(body) counts the RENDERED string, which differs whenever
	// the reply was ANSI-stripped or escaped, so advancing by it lands mid-frame.
	// windowResult names this quantity raw_bytes for exactly this reason.
	rawLen := int64(getFloatAny(doc, "raw_bytes", 0))
	frame, complete := firstMCPMessage(body)
	total := int64(getFloatAny(doc, "total_bytes", 0))
	if !complete {
		// `has_more` says the channel holds bytes this window did not return, which
		// means a frame is longer than the window rather than absent. Reporting 204
		// there would be a lie the caller cannot act on; a 413 names the problem.
		if getBool(doc, "has_more", false) && total > from {
			return envelope.Slot{Code: 413, Headers: map[string]string{
				"Path": stmt.Path, "Error-Code": "frame_too_large",
				"Content-Range": envelope.UnsatisfiedRange(total),
			}, Body: fmt.Sprintf("a frame at offset %d is longer than the %d-byte window", from, window)}, false
		}
		// THE EXPLANATION IS A HEADER, NOT A BODY (see noContentSaying). "no complete
		// frame yet" is true of a slow peer and of a peer whose bytes will never parse,
		// and only one of those is worth waiting on — so the quote of what was actually
		// in front of the scan is the load-bearing half of this reply, and it has to
		// reach the caller. A 204 body would have carried it while contradicting the
		// status line above it; Hint carries it with the status line intact.
		return noContentSaying(map[string]string{
			"Path":          stmt.Path,
			"Content-Range": envelope.ContentRange(from, from+rawLen-1, total),
		}, "no complete frame yet"+recvHint(win)+notAFrame(body)), false
	}
	start := from + int64(strings.Index(body, frame))
	end := start + int64(len(frame))
	// Advance past the frame, and past the whitespace that separated it from the
	// next one, so a resumed read starts on a value rather than on a newline.
	next := end
	for next < from+rawLen && int(next-from) < len(body) && strings.IndexByte(" \t\r\n", body[next-from]) >= 0 {
		next++
	}
	s.setRecvCursor(handle, next)

	// A SUB-MCP'S SAMPLING REQUEST IS ANSWERED HERE, NOT RETURNED TO THE CALLER.
	//
	// `sampling/createMessage` is the one request that flows the other way: the
	// server being driven asks the client's model to generate something. A sub-MCP
	// started on a shell channel has no model, so it asks its host — and this is the
	// host. Returning that frame to the caller instead would leave the sub-MCP
	// blocked on an answer nobody was going to write, and the caller reading a
	// request it did not ask for.
	//
	// Forwarding is transparent on purpose: the caller asked for one frame and gets
	// one frame, after the round trip to the Agent has happened underneath. The
	// answer is written back to the sub-MCP's stdin and the cursor moves past it.
	if handled, slot := s.forwardSubMCPSampling(ctx, stmt, handle, frame); handled {
		if slot != nil {
			return *slot, false
		}
		// Answered and written back: read again, so this recv still delivers a frame.
		return envelope.Slot{}, true
	}

	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Content-Range": envelope.ContentRange(start, end-1, end)},
		Body:    frame,
	}, false
}

// mcpAwaitReply reads one frame for a `call` that has already been written, and says
// whether that frame is the answer to it.
//
// The read is mcpRecvFrame, unchanged: it polls for the caller's window and advances
// the same per-handle cursor, so `call` and `read --frame` share one position and a
// frame is never delivered twice. What this adds is the one fact the caller cannot
// recover from the bytes alone in the moment it matters — whether this frame answers
// the id it just sent. Replies arrive in completion order, which is correct JSON-RPC
// and means the frame that lands first may belong to an earlier request (issue #175).
func (s *Server) mcpAwaitReply(ctx context.Context, stmt directive.Statement, handle, id string, win termArgs, headers map[string]string) envelope.Slot {
	slot := s.mcpRecvFrame(ctx, stmt, handle, win)
	if slot.Code >= 400 || slot.Body == "" {
		return slot
	}
	if got := frameID(slot.Body); got == id {
		return slot
	} else if got == "" {
		slot.Body += "\n\n[this frame has no id: it is a notification or an event, not the answer to id " + id + ". The answer is still on the channel — `read " + handle + " --frame` again.]"
		return slot
	} else {
		slot.Body += "\n\n[this frame answers id " + got + ", not the id " + id + " you just sent: the peer replied in completion order. The answer to " + id + " is still on the channel — `read " + handle + " --frame` again.]"
		return slot
	}
}

// frameID returns the id of a JSON-RPC frame as its own spelling, or "" when the frame
// carries none (a notification) or cannot be read.
//
// The raw text is compared rather than a decoded value on purpose: an id may be a
// string or a number, and `"1"` and `1` are different spellings of different ids. A
// comparison that normalised them would report a match for two frames the peer itself
// considers distinct.
func frameID(frame string) string {
	var env struct {
		ID *json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(frame), &env); err != nil || env.ID == nil {
		return ""
	}
	return strings.TrimSpace(string(*env.ID))
}

// forwardSubMCPSampling answers an inbound `sampling/createMessage` from a sub-MCP
// by asking the Agent that is driving this server, and writes the result back.
//
// handled=false means the frame is not a sampling request and the caller should
// treat it normally. handled=true with a non-nil slot means the round trip failed
// and that slot is the reply; handled=true with a nil slot means the answer was
// written back and the caller should keep reading.
//
// The Agent session is taken from the CONTEXT, not from a stored handle: this runs
// inside the tool call the Agent made, so ctx carries exactly the client that is
// waiting for the reply. A stored session would be wrong as soon as two Agents
// drive the same daemon.
func (s *Server) forwardSubMCPSampling(ctx context.Context, stmt directive.Statement, handle, frame string) (bool, *envelope.Slot) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &req); err != nil {
		return false, nil // not JSON we understand; let the caller see it
	}
	if req.Method != "sampling/createMessage" {
		return false, nil
	}
	// No id: a notification, not a request. Nothing to answer.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return false, nil
	}

	var params mcpgo.CreateMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		// Malformed params: answer the sub-MCP with a JSON-RPC error rather than
		// leaving it blocked on a request that will never be answered.
		return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, nil, "invalid sampling params: "+err.Error())
	}

	target := mcpserver.ClientSessionFromContext(ctx)
	if target == nil {
		return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, nil,
			"no Agent session on this call; a sub-MCP's sampling request can only be forwarded while an Agent is driving the tool call")
	}

	// Method is a PROMOTED field, so it cannot be set in the literal under the
	// module's Go version; assign it after construction.
	createReq := mcpgo.CreateMessageRequest{CreateMessageParams: params}
	createReq.Method = "sampling/createMessage"
	res, err := s.ForwardSampling(ctx, target, createReq)
	if err != nil {
		return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, nil, err.Error())
	}
	return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, res, "")
}

// replyToSubMCP writes a JSON-RPC answer back to the sub-MCP's stdin.
//
// A nil result produces an error object, which is what the sub-MCP is waiting for
// either way: silence would leave its request outstanding forever.
func (s *Server) replyToSubMCP(ctx context.Context, stmt directive.Statement, handle string, id json.RawMessage, res *mcpgo.CreateMessageResult, errMsg string) *envelope.Slot {
	answer := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if errMsg != "" {
		answer["error"] = map[string]any{"code": -32603, "message": errMsg}
	} else {
		answer["result"] = res
	}
	payload, err := json.Marshal(answer)
	if err != nil {
		slot := envelope.Slot{Code: 500, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "sampling_answer_unencodable",
		}, Body: "could not encode the sampling answer: " + err.Error()}
		return &slot
	}
	if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", handle, "text", string(payload))); fail != nil {
		slot := s.slotFromFailure(ctx, stmt, fail)
		return &slot
	}
	if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", handle, "key", "enter")); fail != nil {
		slot := s.slotFromFailure(ctx, stmt, fail)
		return &slot
	}
	return nil
}

// How often one recv looks again, while it waits.
//
// The WAIT is the caller's window (see mcpRecvFrame); this is only the interval, so a
// frame that arrives mid-window is handed over as soon as it lands rather than at the
// next whole second.
const recvPollInterval = 25 * time.Millisecond

// defaultRecvWindow is how long a recv waits when the caller named no window.
//
// TWO SECONDS: the read default, and what this directive has always used. Not the
// fifteen a peer call falls back to — the two verbs answer different questions. A peer
// call is a request/response, while a recv that finds nothing is a read that found
// nothing, and a silent channel is not a peer that is thinking.
const defaultRecvWindow = 2 * time.Second

// recvWindow is how long one recv waits for a frame.
//
// IT ASKS WHETHER THE CALLER SPOKE, for the reason peerCallWindow does: `timeout` has a
// default, so the value alone cannot tell "the caller asked for two seconds" from
// "nobody asked". A window the caller named is used as given, including one longer than
// any default here — capping it would be this server deciding how long the caller may
// wait, which is the decision the bug was made of (#174).
func recvWindow(win termArgs) time.Duration {
	if win.TimeoutSet && win.TimeoutSeconds > 0 {
		return time.Duration(win.TimeoutSeconds * float64(time.Second))
	}
	return defaultRecvWindow
}

// minRecvWindow is the smallest window a recv will read with.
//
// defaultBytesLimit is the tool's default for a READ, where a caller usually wants
// a screenful. A JSON-RPC frame is a different unit: an initialize result with
// instructions is routinely larger than that, and a window smaller than one frame
// makes it undeliverable. 1 MiB is far past any legitimate frame and still a bound.
const minRecvWindow = 1 << 20

// recvCursor and setRecvCursor keep one read position per channel.
//
// Keyed by handle without the server's own offset bookkeeping: two callers reading
// the same channel is a race the protocol does not describe, and serialising here
// means neither silently loses a frame to the other's window.
// recvReader returns the mutex that makes one handle's frames have one reader at a
// time, creating it on first use.
func (s *Server) recvReader(handle string) *sync.Mutex {
	s.recvCursorMu.Lock()
	defer s.recvCursorMu.Unlock()
	if s.recvReaders == nil {
		s.recvReaders = make(map[string]*sync.Mutex)
	}
	m, ok := s.recvReaders[handle]
	if !ok {
		m = &sync.Mutex{}
		s.recvReaders[handle] = m
	}
	return m
}

func (s *Server) recvCursor(handle string) int64 {
	s.recvCursorMu.Lock()
	defer s.recvCursorMu.Unlock()
	return s.recvCursors[handle]
}

// dropRecvCursor forgets a closed handle's read position.
func (s *Server) dropRecvCursor(handle string) {
	s.recvCursorMu.Lock()
	defer s.recvCursorMu.Unlock()
	delete(s.recvCursors, handle)
	// The reader lock goes too: handles are minted monotonically, so a reopened
	// channel never reuses this key and the map would grow by one entry per channel
	// the process ever opened.
	delete(s.recvReaders, handle)
}

func (s *Server) setRecvCursor(handle string, at int64) {
	s.recvCursorMu.Lock()
	defer s.recvCursorMu.Unlock()
	if s.recvCursors == nil {
		s.recvCursors = make(map[string]int64)
	}
	s.recvCursors[handle] = at
}

// notAFrame says what the buffer holds when nothing in it parsed as a value, so a
// caller can tell a peer that writes a banner from a peer that is merely slow.
//
// recvHint names the knob, because a caller told "not yet" has no way to learn that the
// wait was this server's choice rather than its own. Silent when the caller already
// named a window: it then knows how long it asked for, and repeating the number back
// adds nothing.
func recvHint(win termArgs) string {
	if win.TimeoutSet {
		return ""
	}
	return " (this recv waited " + recvWindow(win).String() + "; name `timeout` to wait longer)"
}

// "no complete frame yet" is the same sentence for both, and only one of them is
// worth waiting on. A caller that retries a banner retries for ever.
// notAFrame is the tail of the Hint on a 204 recv: what the scan saw instead of a
// frame. It is a header VALUE, so every branch below is one line by construction.
func notAFrame(body string) string {
	s := strings.TrimSpace(body)
	if s == "" {
		return ""
	}
	if s[0] == '{' || s[0] == '[' {
		// THE START OF A VALUE. If it is a complete value that simply is not a message,
		// saying so is the difference between "wait" and "this will never become a
		// frame": `[0]` is legal JSON, so a caller left with "not yet" would wait out
		// its whole window for bytes that already arrived (#184).
		if json.Valid([]byte(s)) && !isMCPMessage(s) {
			return " — these bytes are JSON, but not a JSON-RPC message (no `id` or `method`), so no frame was taken"
		}
		// Empty, or the start of a value that is simply not finished: the honest
		// "not yet", and nothing to explain.
		return ""
	}
	line := s
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	const limit = 160
	if len(line) > limit {
		line = line[:limit]
		// Never end the quote on half a character.
		for len(line) > 0 && !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
		line += "\u2026"
	}
	return " — the bytes in front are not a frame: " + strconv.Quote(line)
}

// firstMCPMessage returns the first complete JSON-RPC message in raw.
//
// It scans for a complete object or array rather than trusting a newline: a frame
// may be pretty-printed, and a line-delimited peer may still emit a value with
// embedded newlines inside a string.
//
// A STDIO PEER WRITES ITS OWN NOISE ONTO THE SAME STREAM, AND THAT NOISE CAN START
// WITH THE BYTE A FRAME STARTS WITH. Measured twice, on two unrelated servers:
// chrome-devtools-mcp announces itself with "[chrome-devtools-mcp] The connecting
// client did not negotiate the MCP roots capability…", and
// @modelcontextprotocol/server-filesystem with "…using allowed directories set from
// server args: [ 'C:\\tc' ]". A scan that trusted the first '[' in the buffer was
// then parsing the banner — for ever. The bytes were never missing; the parse
// started in the wrong place, and the caller was told "not yet" about a frame that
// had already arrived, with no way to act on it.
//
// SO EVERY CANDIDATE IS TRIED, in the order the buffer holds them, and the first one
// that is a MESSAGE wins. A candidate that is not JSON fails within its first byte or
// two, which is what keeps the scan cheap.
//
// IN BUFFER ORDER, NOT GROUPED BY OPENER. Trying every '{' before any '[' looks
// equivalent and is not: a batch array `[{...}]` starts with '[', so scanning '{'
// first would return the object INSIDE the batch as if it were the frame.
//
// AND A CANDIDATE HAS TO BE A MESSAGE, NOT MERELY JSON (#184). The criterion used to
// be "it parses", which is a different question and has an everyday wrong answer: a
// PowerShell error line reads `InputObject[0]: some text`, `[0]` is legal JSON, and a
// caller waiting for a reply was handed `[0]` with a 200 — a plausible wrong answer,
// which is worse than "not yet". The caller's own contract is the message: this verb
// parses ONE JSON-RPC FRAME, and `[0]` is not one.
func firstMCPMessage(raw string) (string, bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' && raw[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw[i:]))
		var value json.RawMessage
		err := dec.Decode(&value)
		if err == nil {
			if isMCPMessage(string(value)) {
				return strings.TrimSpace(string(value)), true
			}
			// A COMPLETE VALUE THAT IS NOT A MESSAGE IS SKIPPED, NOT RETURNED: the
			// caller asked for a frame, and ordinary output is allowed to hold JSON.
			continue
		}
		// THE ONE FAILURE THAT MEANS "WAIT" RATHER THAN "WRONG PLACE". The input
		// ending inside a value is a frame still arriving; anything else is a
		// candidate that was never a frame, so the scan moves on.
		//
		// Stopping here is also what bounds the cost. A partial 30 KB frame holds
		// hundreds of '{', and the poll loop would rescan the whole buffer for each
		// of them, every 25 ms, for the whole budget.
		if errors.Is(err, io.ErrUnexpectedEOF) || isSyntaxEOF(err) {
			return "", false
		}
	}
	return "", false
}

// isMCPMessage reports whether value has the shape of a JSON-RPC message: an object
// carrying `id` or `method`, or an array of such objects (the batch form the spec
// allows).
//
// THE SHAPE IS THE WHOLE TEST, and it is deliberately not "it parses": a caller that
// is handed a value it did not ask for cannot tell it from an answer. `id` OR `method`
// rather than both, because a notification carries only `method` and a reply only
// `id`. A missing `jsonrpc` field is NOT checked: JSON-RPC 1.0 has no such field, and
// refusing a peer over it would turn a working channel into an unreadable one.
func isMCPMessage(value string) bool {
	if isMessageObject(value) {
		return true
	}
	var batch []json.RawMessage
	if err := json.Unmarshal([]byte(value), &batch); err != nil {
		return false
	}
	for _, item := range batch {
		if !isMessageObject(string(item)) {
			return false
		}
	}
	return len(batch) > 0
}

// isMessageObject is isMCPMessage for the object form, which a batch is a list of.
func isMessageObject(value string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &obj); err != nil || obj == nil {
		return false
	}
	if _, ok := obj["id"]; ok {
		return true
	}
	_, ok := obj["method"]
	return ok
}

// isSyntaxEOF reports whether a JSON decode failed only because the input ended.
func isSyntaxEOF(err error) bool {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// closedReadCursor and setClosedReadCursor keep one read position per CLOSED
// channel, which is where the default `read` of a persisted stream starts.
//
// A live channel's position lives in its ring-buffer reader; a closed one has no
// such reader, and a position that is not kept anywhere is a default read that
// replays the same bytes forever (#159 C1).
func (s *Server) closedReadCursor(key string) int64 {
	s.closedReadMu.Lock()
	defer s.closedReadMu.Unlock()
	return s.closedReads[key]
}

func (s *Server) setClosedReadCursor(key string, at int64) {
	s.closedReadMu.Lock()
	defer s.closedReadMu.Unlock()
	if s.closedReads == nil {
		s.closedReads = make(map[string]int64)
	}
	s.closedReads[key] = at
}
