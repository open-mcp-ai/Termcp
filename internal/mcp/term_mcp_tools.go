package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// The mcp peer's TOOL surface: the half of the protocol a caller actually uses.
//
// `call <session> mcp:1 '{"jsonrpc":"2.0","id":1,"method":"tools/call",…}'` is the
// escape hatch and stays exactly as it was — a frame is a frame, and notifications,
// progress and sampling have no high-level spelling. But the overwhelming majority
// of what a caller sends a peer is "run this tool with these arguments", and making
// that a hand-written envelope costs tokens on every call and gets the shape wrong
// at least once (#159).
//
// So the door answers to a tool name too. The split is unambiguous by construction:
// a frame is a JSON OBJECT and therefore starts with '{', and a tool name never
// does. Nothing is guessed, and the escape hatch keeps working for every frame that
// is not a tool call.

// mcpToolCall builds the tools/call envelope for `call <session> mcp:1 <tool> <json>`
// and renders the answer as the TEXT the tool returned.
func (s *Server) mcpToolCall(ctx context.Context, stmt directive.Statement, h *jrpcHandle, payload string, win termArgs) envelope.Slot {
	payload, badPayload := objectArgsPayload(stmt, payload, win)
	if badPayload != nil {
		return *badPayload
	}
	name, args, bad := splitToolCall(stmt, payload)
	if bad != nil {
		return *bad
	}
	if err := s.jrpcReady(ctx, h); err != nil {
		return jrpcError(stmt, err)
	}
	// A FIRE-AND-FORGET TOOL CALL still goes through Call: the channel assigns the
	// id, and `--send` needs the frame written without waiting for its answer.
	frame := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	}
	if boolFlag(stmt, "send") {
		if err := h.ch.Send(ctx, frame); err != nil {
			return jrpcError(stmt, err)
		}
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path},
			Body: "sent `" + name + "`; the answer arrives on `read " + peerAddress(h.sessionID, h.kind, h.index) + "`"}
	}
	reply, err := h.ch.Call(ctx, frame)
	if err != nil {
		return jrpcError(stmt, err)
	}
	if boolFlag(stmt, "json") {
		// --json is the escape hatch back to the frame: the content items, the
		// isError flag and any structuredContent the peer attached are all still
		// there, in the shape the peer sent them.
		return s.frameReply(ctx, stmt, h, reply, win)
	}
	return s.mcpResultText(ctx, stmt, h, reply, win)
}

// objectArgsPayload folds an `args` OBJECT into the payload the tool call reads.
//
// THE ARGUMENTS SLOT HAS ONE SOURCE, and the object is one of them: it fills the slot
// the line leaves empty (`call <peer> <tool>` with nothing after the tool name). A line
// that spells arguments in both places is refused rather than silently preferring one —
// the reply is a 200 either way, so a caller whose parameters were dropped has nothing
// to notice.
func objectArgsPayload(stmt directive.Statement, payload string, win termArgs) (string, *envelope.Slot) {
	if len(win.ArgsObject) == 0 {
		return payload, nil
	}
	if toolArgsWritten(payload) {
		slot := badRequest(stmt, "this call names its arguments twice: `args` is an object, and the line "+
			"also spells them after the tool name. An object `args` IS the arguments of a `call <peer> <tool>` "+
			"line that leaves the slot empty; pick one source")
		return "", &slot
	}
	// THE OBJECT RIDES IN THE POSITION THE SLOT ALWAYS HAD, so one reader
	// (splitToolCall) handles both spellings and the two cannot drift.
	return strings.TrimSpace(payload) + " " + string(win.ArgsObject), nil
}

// toolArgsWritten says the line spelled arguments after the tool name, which is the
// other source the `args` object cannot share a call with.
//
// A TOOL NAME IS ONE WORD: the payload is normalized to "<tool> <json>", so anything
// after the first word IS written arguments — including a second word, which
// splitToolCall would then refuse as unparseable JSON. Refusing here names the real
// conflict instead of the symptom.
func toolArgsWritten(payload string) bool {
	trimmed := strings.TrimSpace(payload)
	if i := strings.IndexAny(trimmed, " \t\n"); i >= 0 {
		return strings.TrimSpace(trimmed[i+1:]) != ""
	}
	return false
}

// splitToolCall reads the tool name and its arguments out of the payload.
//
// The name is the first word and everything after it is the arguments object,
// verbatim: that is the only split that lets a JSON body containing spaces reach
// the parser intact, and it is the same "first word, then the rest" rule `create`
// and `edit` already use for their trailing value.
func splitToolCall(stmt directive.Statement, payload string) (string, map[string]any, *envelope.Slot) {
	return splitToolCallAt(stmt, payload, "call <session> mcp:<n> <tool> [<json-args>]")
}

// splitToolCallAt is splitToolCall with the address spelling named by the caller, so a
// refusal teaches the line the caller actually has: an extension's stdio channel is
// called as `call <channel> <tool> {…}`, and answering that caller with the HTTP form's
// `mcp:<n>` address would send them looking for a peer that does not exist (issue #316).
func splitToolCallAt(stmt directive.Statement, payload, shape string) (string, map[string]any, *envelope.Slot) {
	payload = strings.TrimSpace(payload)
	name, rest := payload, ""
	if i := strings.IndexAny(payload, " \t\n"); i >= 0 {
		name, rest = payload[:i], strings.TrimSpace(payload[i+1:])
	}
	name = unquoteWord(name)
	if name == "" {
		return "", nil, ptrSlot(badRequest(stmt, "needs a tool name: "+shape))
	}
	args := map[string]any{}
	if rest != "" {
		if err := json.Unmarshal([]byte(unquoteWord(rest)), &args); err != nil {
			return "", nil, ptrSlot(badRequest(stmt, "the arguments are not a JSON object: "+err.Error()+
				"\nA tool's arguments are one object, bare or in single quotes:\n"+
				"  "+shape))
		}
	}
	return name, args, nil
}

// mcpResultText renders a tools/call answer as what the tool said.
//
// THE ENVELOPE IS NOISE HERE. A response is {"result":{"content":[{"type":"text",
// "text":"…"}],"isError":false}}, and every caller that asked for a tool's text had
// to walk that structure itself — in a shell, with no JSON parser. The text is the
// answer; the envelope is transport.
func (s *Server) mcpResultText(ctx context.Context, stmt directive.Statement, h *jrpcHandle, reply string, win termArgs) envelope.Slot {
	// A SHAPE THIS DOES NOT RECOGNISE IS SHOWN AS IT ARRIVED, not summarised. That
	// covers a peer error (frameReply turns it into a failing reply with its hints)
	// and any method whose result is not a tool result.
	body, isError, ok := unpackToolResult(reply)
	if !ok {
		return s.frameReply(ctx, stmt, h, reply, win)
	}
	if isError {
		// A TOOL THAT REPORTED FAILURE DID NOT SUCCEED (issue #159). MCP carries that
		// in the result rather than in an error object, so a status-only caller read
		// 200 and concluded the work was done.
		return envelope.Slot{Code: 500, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "tool_error",
		}, Body: "ERROR: " + body}
	}
	// THE SAME THRESHOLD AS THE OTHER DOOR, and for the same reason (issue #204): this
	// was a second hardcoded 4096, so `call <peer> mcp:1 <tool> --bytes 32768` and
	// `call <peer> cdp:1 …` would have disagreed about whether the same frame fits.
	// A receipt is for "it does not fit the window the caller chose", not for "it is
	// bigger than a number this file picked".
	inlineLimit := win.BytesLimit
	if inlineLimit <= 0 {
		inlineLimit = defaultBytesLimit
	}
	if len(body) > inlineLimit {
		if rec, ok := h.ch.LastAnswer(); ok {
			return envelope.Slot{Code: 200, Headers: map[string]string{
				"Path": stmt.Path, "Frame": fmt.Sprint(rec.N),
				"Bytes": fmt.Sprint(len(rec.Data)), "Decoded-Bytes": fmt.Sprint(len(body)),
			}, Body: receiptBody(rec.N, body, len(rec.Data), inlineLimit, stmt, h)}
		}
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: body}
}

// unpackToolResult renders a tools/call answer as the text the tool returned.
//
// ONE IMPLEMENTATION FOR EVERY PEER (issue #316): the HTTP form reaches the same server
// over a socket and the stdio form over a pipe, and a caller that switched transports
// must not find that "the text of the result" changed shape. ok is false when the frame
// is not a tool result at all — a peer error, a notification, any other method — and
// each caller then decides how to show it.
func unpackToolResult(reply string) (body string, isError bool, ok bool) {
	var doc struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				MimeType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(reply), &doc); err != nil || doc.Error != nil ||
		doc.Result == nil || doc.Result.Content == nil {
		return "", false, false
	}
	var parts []string
	for _, c := range doc.Result.Content {
		switch {
		case c.Type == "text" || c.Text != "":
			parts = append(parts, c.Text)
		case c.Data != "":
			// Base64 inline is not readable and not short: name it rather than
			// dropping it. `--json` is the way to get the bytes.
			parts = append(parts, fmt.Sprintf("[%s: %s, %s base64]", orDash(c.Type), orDash(c.MimeType), humanBytes(int64(len(c.Data)))))
		default:
			parts = append(parts, "["+orDash(c.Type)+"]")
		}
	}
	return strings.Join(parts, "\n"), doc.Result.IsError, true
}

// mcpToolList answers `get <session> mcp:1 tools` — the peer's own table, read live.
//
// WHY IT IS READ RATHER THAN REMEMBERED: the same reason `cdp protocol` is. A cached
// table is a second truth, and it drifts the moment the peer is upgraded — the
// caller then fills in a parameter that no longer exists and reads the refusal as
// their own mistake. The peer publishes its own signature; this asks for it.
//
// AN ARGUMENT NAMES ONE TOOL and prints its full input schema. The list is compact
// on purpose (a name, one line, the required keys) because a full schema per tool is
// most of the context window and almost none of the decision.
func (s *Server) mcpToolList(ctx context.Context, stmt directive.Statement, h *jrpcHandle, win termArgs, tool string) envelope.Slot {
	waitCtx, cancel := context.WithTimeout(ctx, peerCallWindow(win))
	defer cancel()
	if err := s.jrpcReady(waitCtx, h); err != nil {
		return jrpcError(stmt, err)
	}
	reply, err := h.ch.Call(waitCtx, map[string]any{"jsonrpc": "2.0", "method": "tools/list"})
	if err != nil {
		return jrpcError(stmt, err)
	}
	tools, ok := parseToolList(reply)
	if !ok {
		return s.frameReply(ctx, stmt, h, reply, win)
	}
	return renderToolTable(stmt, tools, peerAddress(h.sessionID, h.kind, h.index),
		stmt.Target+" "+h.kind+":"+h.index, tool)
}

// toolEntry is one row of a peer's tools/list answer.
type toolEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// parseToolList reads a tools/list answer.
//
// IT IS PARSED HERE RATHER THAN AT EACH DOOR because the answer is the same whichever
// transport carried it (issue #316): an HTTP peer and an extension's stdio channel run
// the same server, and a second parser would be a second place for the shape to drift.
func parseToolList(reply string) ([]toolEntry, bool) {
	var doc struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			Tools []toolEntry `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(reply), &doc); err != nil || doc.Error != nil || doc.Result == nil {
		return nil, false
	}
	return doc.Result.Tools, true
}

// renderToolTable prints a peer's tool table, and one tool's full schema when a name is
// given.
//
// ref IS HOW THE CALLER ADDRESSES THE PEER, spelled the way that peer is addressed: `sid
// mcp:1` for an HTTP peer, the channel handle for an extension's stdio server. The
// "next call" lines are built from it, because a table that ends by teaching an address
// the caller cannot use is worse than no hint at all.
func renderToolTable(stmt directive.Statement, tools []toolEntry, addr, ref, tool string) envelope.Slot {
	if tool != "" {
		for _, t := range tools {
			if t.Name != tool {
				continue
			}
			var b strings.Builder
			b.WriteString(t.Name)
			if desc := firstLineOf(t.Description); desc != "" {
				b.WriteString(" — " + desc)
			}
			b.WriteString("\n\n")
			schema := strings.TrimSpace(string(t.InputSchema))
			if schema == "" || schema == "null" {
				schema = "{}"
			}
			b.WriteString(indentJSON(schema))
			b.WriteString("\n\ncall " + ref + " " + t.Name + " {…}")
			return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
		}
		var names []string
		for _, t := range tools {
			names = append(names, t.Name)
		}
		return envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "not_found",
		}, Body: "no tool named " + tool + " on " + addr + ". It lists: " + strings.Join(names, ", ")}
	}

	if len(tools) == 0 {
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path},
			Body: "0 tools. The peer answered tools/list with an empty table."}
	}
	width := 0
	for _, t := range tools {
		if n := utf8.RuneCountInString(t.Name); n > width {
			width = n
		}
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d tool(s) on %s\n", len(tools), addr))
	for _, t := range tools {
		b.WriteString("  " + t.Name + strings.Repeat(" ", width-utf8.RuneCountInString(t.Name)))
		if desc := firstLineOf(t.Description); desc != "" {
			b.WriteString("  " + desc)
		}
		if req := requiredKeys(t.InputSchema); len(req) > 0 {
			b.WriteString("   required: " + strings.Join(req, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\none tool's full schema: get " + ref + " tools <name>")
	b.WriteString("\ncall one:              call " + ref + " <tool> {…}")
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// requiredKeys reads the `required` array out of a tool's input schema, which is the
// one part of the schema a caller needs before the first call.
func requiredKeys(schema json.RawMessage) []string {
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &doc); err != nil {
		return nil
	}
	return doc.Required
}

// firstLine flattens a description to the sentence a list row can hold: peers often
// ship several paragraphs, and a table of them is unreadable.
func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if utf8.RuneCountInString(s) > 100 {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:97])) + "…"
	}
	return s
}

// indentJSON pretty-prints a schema for a reply that a human reads. A schema that
// does not parse is passed through: showing the peer's own bytes beats showing
// nothing.
func indentJSON(raw string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "  ", "  "); err != nil {
		return "  " + raw
	}
	return "  " + buf.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "?"
	}
	return s
}
