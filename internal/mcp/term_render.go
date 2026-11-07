package mcp

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/session"
	mcpgo "github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// Generated replies.
//
// `ps`, `info`, `help` and the extension/profile listings are NOT byte streams:
// there is nothing to continue reading, so they carry no Content-Range and their
// body is terminal text — one line per object, columns, `key: value`. That is not
// cosmetic. A model reads `a1f3 running pipe go-build 12s` faster than the same
// facts nested in JSON, `grep`/`cut` work on it, and the handle in the first column
// can be copied straight into the next command. The format requirement that
// matters is that last one: every handle in `ps` must be usable verbatim as the
// target of another directive.

// directivePS lists what is running, one line per channel.

// Everything here renders. Nothing here talks to a session beyond reading a
// field off one, and nothing here needs a Server.

func usageLinesNaming(word string) string {
	word = strings.TrimSpace(word)
	if word == "" {
		return ""
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, op := range directive.Ops() {
		if seen[op.Usage] {
			continue
		}
		for _, field := range strings.FieldsFunc(op.Usage, func(r rune) bool {
			return !(r == '_' || r == '-' || r == ':' || unicode.IsLetter(r) || unicode.IsDigit(r))
		}) {
			if field == word {
				seen[op.Usage] = true
				b.WriteString(op.Usage + "\n")
				break
			}
		}
	}
	return b.String()
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// quoteOrDash renders a flag value for an error message.

func quoteOrDash(s string) string {
	if s == "" {
		return "(missing)"
	}
	return fmt.Sprintf("%q", s)
}

// handleOf renders a channel's short handle: <session>:<index>.
//
// IT ALWAYS INCLUDES THE INDEX, even for the primary channel (index 1). The old
// version returned bare sessionID for index <= 1, which made the primary channel's
// handle look identical to the session itself. That was the root cause of every
// ambiguity between "the container" and "its first channel":
//
//	close <session> --force   fell back to primary channel and became a no-op (#132)
//	run <session>             fell back to primary channel, silently guessing which
//	                          channel to use on a multi-channel session
//
// With `:1` always present, a channel handle and a session id have DIFFERENT
// shapes: `sid:1` is a channel, `sid` is the container. A directive that operates
// on a channel (run, read, type, key, resize) requires a channel shape, and a bare
// session id is refused at parse time.

func handleOf(sessionID string, index int) string {
	if index < 1 {
		index = 1
	}
	return fmt.Sprintf("%s:%d", sessionID, index)
}

// ageOf renders how long ago a timestamp was, in terminal units.

func ageOf(unixMilli int64) string {
	if unixMilli == 0 {
		return "-"
	}
	secs := (clock.Now() - unixMilli) / 1000
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%dh", secs/3600)
	}
}

// writeKV writes one `key: value` line.

func writeKV(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteByte('\n')
}

// approvalSummaryText renders a session's review state in one field.

func approvalSummaryText(info api.Session) string {
	if !info.ApprovalMode {
		return "off"
	}
	scopes := info.ApprovalScopes
	if len(scopes) == 0 {
		return fmt.Sprintf("on (%d approver, all operations)", info.ApprovalNeed)
	}
	return fmt.Sprintf("on (%d approver, %s)", info.ApprovalNeed, strings.Join(scopes, ","))
}

// compactDocument renders a handler's JSON reply as `key: value` lines, one level
// deep. Nested structures are rendered as their JSON text, which keeps a generated
// reply readable without inventing a second rendering per document.

func compactDocument(doc map[string]any) string {
	if len(doc) == 0 {
		return ""
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		switch v := doc[k].(type) {
		case string:
			b.WriteString(v)
		case nil:
			b.WriteString("-")
		case bool:
			b.WriteString(fmt.Sprint(v))
		case float64:
			if v == float64(int64(v)) {
				b.WriteString(fmt.Sprintf("%d", int64(v)))
			} else {
				b.WriteString(fmt.Sprint(v))
			}
		default:
			raw, _ := jsonMarshalIndent(map[string]any{"v": v})
			b.WriteString(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "{\n  \"v\": "), "\n}")))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// listFromDoc renders a named list inside a handler's JSON reply as one entry per
// line: the shape a caller copies into the next command.

func listFromDoc(res *mcpgo.CallToolResult, listKey, field string) string {
	doc, ok := toolResultMap(res)
	if !ok {
		return toolResultText(res)
	}
	items, _ := doc[listKey].([]any)
	var b strings.Builder
	for _, raw := range items {
		// Some lists are plain names (ssh_configs), others are objects with a
		// description beside the name. Both shapes reach here, and a listing that
		// silently drops one of them reads as "there is nothing there".
		if name, ok := raw.(string); ok {
			b.WriteString(name)
			b.WriteByte('\n')
			continue
		}
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		b.WriteString(getString(item, field, ""))
		// WHERE IT IS, on the same line as the name. The kind column is the one that
		// separates "the machine I am on" from "a machine I reach over ssh" — the
		// first thing a reader of `ls host` is deciding — and `where` answers it
		// (a local OS, or the user@host:port a remote entry dials).
		if kind := getString(item, "kind", ""); kind != "" {
			b.WriteString("\t" + kind)
			b.WriteString("\t" + dash(getString(item, "where", "")))
		}
		if desc := getString(item, "description", ""); desc != "" {
			b.WriteString("\t" + desc)
		}
		// A BROKEN ENTRY SAYS WHY, on the same line as its name. The column exists
		// because a name alone cannot answer "can I connect to this?" — the reader of
		// a listing is deciding what to use, and the reason is the part that decides.
		if why := getString(item, "broken", ""); why != "" {
			b.WriteString("\tbroken: " + why)
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "(none)\n"
	}
	return b.String()
}

// extensionListing renders `ext list` as the boot manifest: name, state, handle,
// and a one-line note. This is the call that gives a caller its toolset back after
// its context was cleared, so it must be one call and must be scannable.
//
// THE HANDLE IS THE ADDRESS, NOT THE RAW CHANNEL ID (#159 D2). The reconciler
// reports a shell id (`9ff338e1-68b`), which is a UUID fragment: it identifies the
// channel but is not what the rest of the surface takes. An address is
// `<session>:<index>` — the same spelling `ps` prints and every later command
// pastes — so a caller that copies this row can act on it directly instead of
// calling `get ext` to translate.

func (s *Server) extensionListing(res *mcpgo.CallToolResult, all bool) string {
	doc, ok := toolResultMap(res)
	if !ok {
		return toolResultText(res)
	}
	items, _ := doc["extensions"].([]any)
	// THE LIST CAPS ITSELF, AND NAMES WHAT IT WITHHELD (issue #185).
	//
	// The byte window would cut forty rows mid-line and report it in a header about
	// bytes: "Truncated: 900 bytes withheld" does not tell a caller whether it is
	// missing one extension or thirty, and the one it needs may be in the part it did
	// not see. A count plus a command is something it can act on; "and more" is a wall.
	//
	// This is a VIEW, not a limit on what can be reached: `ls ext --all` lists every
	// one, and an extension that is not shown is still addressable by name.
	const listed = 10
	hidden := 0
	if !all && len(items) > listed {
		hidden = len(items) - listed
		items = items[:listed]
	}
	var b strings.Builder
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := getString(item, "name", "")
		state := getString(item, "status", "")
		handle := getString(item, "session_id", "")
		if shell := getString(item, "shell_id", ""); shell != "" {
			// The address when it can be derived, the id otherwise: a shell-less
			// extension (an SFTP-only window) has no channel and reports its
			// session, which is exactly the address a file read takes.
			handle = s.addressOf(getString(item, "session_id", ""), shell)
		} else if peer := s.extPeerHandleOf(getString(item, "session_id", ""), name); peer != "" {
			// AN ENDPOINT EXTENSION HAS NO CHANNEL BUT DOES HAVE AN ADDRESS (issue #271).
			//
			// It is mounted as a real MCP peer, so the thing a caller types next is
			// `call <session> mcp:1 <tool> {…}` — and this column is the one place a
			// caller that lost its context can recover that address. Falling back to the
			// bare session id left the column claiming a handle that no `call` accepts,
			// so the only way back was to run `open ext` again.
			handle = peer
		}
		notes := strings.ReplaceAll(getString(item, "notes", ""), "\n", " ")
		if len(notes) > 60 {
			notes = notes[:60] + "…"
		}
		b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\n", name, dash(state), dash(handle), notes))
	}
	if b.Len() == 0 {
		return "(no extensions installed)\n"
	}
	if hidden > 0 {
		b.WriteString(fmt.Sprintf("(%d more not shown; `ls ext --all` lists every extension)\n", hidden))
	}
	return b.String()
}

// addressOf turns a session id and a channel id into the address the rest of the
// surface speaks: `<session>:<index>`.
//
// It falls back to the channel id when the session is gone or the index is unknown
// — a restored manifest can predate the index — because a handle that is merely
// less convenient still lets the caller look the channel up, while an empty column
// says nothing at all.

func (s *Server) addressOf(sessionID, shellID string) string {
	if sessionID == "" || shellID == "" {
		return dash(shellID)
	}
	sess := s.sessMgr.Get(sessionID)
	if sess == nil {
		return shellID
	}
	for _, sh := range sess.ListChildShells() {
		if sh.ID == shellID && sh.Index > 0 {
			return handleOf(sessionID, sh.Index)
		}
	}
	return shellID
}

// capGenerated applies the byte and line windows to a generated reply, in that
// order, and reports whether it had to cut.
// generatedSlot renders a generated body through the caller's window: the same
// three axes that bound a stream bound a listing or a document.

func generatedSlot(stmt directive.Statement, win termArgs, body string) envelope.Slot {
	capped, code, cut, total := capGeneratedCounted(body, win)
	headers := map[string]string{"Path": stmt.Path}
	noteTruncated(headers, code, cut, total, win)
	return envelope.Slot{Code: code, Headers: headers, Body: capped}
}

func noteTruncated(headers map[string]string, code, cut, total int, win termArgs) {
	if code != 200 && total > 0 {
		// The same Content-Range a streamed read uses, saying only how long the reply
		// would have been. A generated reply has no cursor, so `bytes */N` is the whole
		// of what it can say — but that N is what lets a caller price the page instead
		// of discovering the size by paying it. The documented cheap form
		// (`bytes_limit: 0`) used to answer with an empty 204 carrying no size at all,
		// while the same call on a channel answered `bytes */2593`.
		headers["Content-Range"] = envelope.UnsatisfiedRange(int64(total))
	}
	if cut <= 0 {
		return
	}
	headers["Truncated"] = fmt.Sprintf("%d bytes withheld by this call's window (bytes_limit %d, lines_limit %d); raise the window to see the rest — a generated reply has no cursor to continue from",
		cut, win.BytesLimit, win.LinesLimit)
}

func capGenerated(body string, win termArgs) (string, int) {
	capped, code, _, _ := capGeneratedCounted(body, win)
	return capped, code
}

// capGeneratedCounted is capGenerated plus HOW MUCH IT TOOK, because a listing cut by
// the caller's own byte window used to be indistinguishable from a complete one.
//
// A generated reply has no stream, so there is no cursor to page it with and no
// Content-Range to say where it stopped: `ls host` under `bytes_limit: 60` answered 200
// with three of six entries, and the only hint was that the reply was shorter than the
// limit — which a caller reading a table it has never seen cannot notice. The count is
// what a header can be built from.
//
// The unit is bytes, not entries: a fold ("too big, ask for less") withholds the whole
// body, and reporting a count there would suggest a partial answer that does not exist.
// foldOrTruncate already appends its own notice to a truncated body; this number is for
// the reply's headers, which is where a caller looks before reading the body at all.

func capGeneratedCounted(body string, win termArgs) (capped string, code, cut, total int) {
	total = len(body)
	if win.BytesLimit == 0 {
		// The schema's zero-byte window: streamed replies hand back a handle and
		// consume nothing; a generated reply has no stream, so it is simply empty.
		// The total still travels, because "how big is this page" is the one fact a
		// caller needs before deciding to spend a window on it.
		return "", 204, 0, total
	}
	code = 200
	cut = 0
	if win.BytesLimit > 0 && len(body) > win.BytesLimit {
		before := len(body)
		body = foldOrTruncate(body, win.BytesLimit, win.OnBytesOverflow)
		code = 206
		if len(body) < before {
			cut = before - len(body)
		}
	}
	if win.LinesLimit > 0 {
		if lines := strings.Count(body, "\n"); lines > win.LinesLimit {
			before := len(body)
			body = foldOrTruncateLines(body, win.LinesLimit, win.OnLinesOverflow)
			code = 206
			if len(body) < before {
				cut += before - len(body)
			}
		}
	}
	return body, code, cut, total
}

// setFlagString copies a string flag into a handler argument.
//
// It copies EVERY explicit value, including an empty one, and reports a flag written
// without a value instead of dropping it.
//
// The old `ok && v != ""` test is why an explicitly empty `--data` could not write
// an empty file: the caller's explicit empty value was discarded here, the handler
// saw no `data` key at all, and answered that no data had been given — for a flag
// that had been given. The other half of the same test was worse: `--remote-path`
// with no value was also discarded, so a fumbled flag looked identical to a
// forgotten one.

func flagList(op directive.Op) string {
	if len(op.Flags) == 0 {
		return ""
	}
	// The names, not the declarations: the domain annotation is how the table records
	// what a flag accepts, and `--cursor:number` is not a flag a caller can pass.
	names := op.FlagNames()
	valueless := make(map[string]bool, len(op.Valueless))
	for _, v := range op.Valueless {
		valueless[v] = true
	}
	w := make([]string, 0, len(names))
	for _, f := range names {
		if valueless[f] {
			w = append(w, "--"+f)
			continue
		}
		w = append(w, "--"+f+" <value>")
	}
	sort.Strings(w)
	return "Flags: " + strings.Join(w, "  ")
}

// flagTable renders one line per declared flag: what it takes, and what happens if
// you get it wrong.
//
// It walks op.Flags rather than the summary map, for the reason flagList is derived
// too: a hand-kept list drifts the moment a flag is added, and the drift is silent —
// the flag simply stops being mentioned. Walking the declaration makes the table
// complete by construction.
//
// A verb with no summaries renders nothing, which is how it keeps its full page: the
// surface migrates one verb at a time, and a verb that has not migrated behaves
// exactly as it did before this existed.

func flagTable(op directive.Op) string {
	if !directive.HasSummaries(op.Name) {
		return ""
	}
	valueless := make(map[string]bool, len(op.Valueless))
	for _, v := range op.Valueless {
		valueless[v] = true
	}
	spellingOf := func(f string) string {
		if valueless[f] {
			return "--" + f
		}
		return "--" + f + " <value>"
	}
	// The names, not the declarations: the summary map is keyed by the name, so the raw
	// entries would render a column of `--cursor:number` with nothing beside it.
	names := op.FlagNames()
	// The column is measured, not guessed: a fixed width misaligns the moment a flag
	// spelling outgrows it, which is how `--from-line <value>` pushed its line out.
	width := 0
	for _, f := range names {
		if n := len(spellingOf(f)); n > width {
			width = n
		}
	}
	w := make([]string, 0, len(names))
	for _, f := range names {
		line, _ := directive.FlagSummary(op.Name, f)
		w = append(w, fmt.Sprintf("  %-*s  %s", width, spellingOf(f), line))
	}
	sort.Strings(w)
	return strings.Join(w, "\n") + "\n"
}

// peerLines renders the open peers of the sessions in a listing.
//
// The address is `<session> <kind>` and is printed verbatim: it is what `call`,
// `read`, `get` and `close` accept, so a caller can copy a line out of `ls` into the
// next command without translating anything. The session half is the id in column 1
// of the session's own line, which is why the id is repeated rather than implied.

func peerLines(sessions []api.Session) string {
	if len(sessions) == 0 {
		return ""
	}
	want := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		want[sess.ID] = true
	}
	jrpcChannelsMu.Lock()
	handles := make([]*jrpcHandle, 0, len(jrpcChannels))
	for _, h := range jrpcChannels {
		if want[h.sessionID] {
			handles = append(handles, h)
		}
	}
	jrpcChannelsMu.Unlock()
	if len(handles) == 0 {
		return ""
	}
	sort.Slice(handles, func(i, j int) bool {
		if handles[i].sessionID != handles[j].sessionID {
			return handles[i].sessionID < handles[j].sessionID
		}
		return handles[i].kind < handles[j].kind
	})
	var b strings.Builder
	b.WriteString("\npeers (address, transport, endpoint, attached):\n")
	for _, h := range handles {
		b.WriteString("  " + peerAddress(h.sessionID, h.kind, h.index) + "\t" +
			h.ch.Transport().String() + "\t" + h.ch.URL())
		if h.cdp != nil {
			b.WriteString(fmt.Sprintf("\t%d page(s) attached", len(h.cdp.snapshot())))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// streamLen is one channel's stream length, asked of the record rather than of the
// cache in front of it.
//
// It falls back to the buffer when there is no log to ask (a session with persistence
// off), which is the same answer the cache gives and the only one available there.

func (s *Server) streamLen(cs *session.ChildShell) int64 {
	n := cs.BufferLen()
	if s.msgMgr == nil {
		return n
	}
	sessID := cs.ParentSessionID()
	if sessID == "" {
		return n
	}
	if m, err := s.msgMgr.OutputSize(sessID, cs.ID); err == nil && m > n {
		return m
	}
	return n
}
