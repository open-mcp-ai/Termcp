package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// isPeerKind reports whether a kind names a frame peer rather than a file or a
// forward. Peers are the three doors that move JSON-RPC frames.
func isPeerKind(kind string) bool {
	switch kind {
	case "cdp", "mcp", "jrpc":
		return true
	}
	return false
}

// isPeerHandle reports whether target is the old bare-handle spelling of a peer.
//
// KEPT BECAUSE THE CHANNEL FORM STILL USES IT: a stdio MCP server runs ON a shell, so
// `read a1f3:1 --frame` and `call a1f3:1 '{…}'` address a channel, not a session plus
// a kind. A registry key is `<session> <kind>`, which a channel handle can never be,
// so the check stays exact.
func isPeerHandle(target string) bool {
	jrpcChannelsMu.Lock()
	_, found := jrpcChannels[target]
	jrpcChannelsMu.Unlock()
	return found
}

// peerOf resolves the peer a statement addresses: `<session> <kind>:<index>[@<target>]`.
//
// THE SESSION HALF IS RESOLVED, NOT MATCHED. A caller names a session by its id
// (what `ls` prints) or by its host (`internal`), and both mean the same link; the
// registry is keyed by the id, so the name is resolved here exactly once.
//
// THE INDEX IS THE CONNECTION, and `@<target>` is the tab inside it. The bare
// spelling (`internal cdp`) is accepted only while exactly one connection exists —
// with two, an unqualified address would have to pick one, and picking is how a
// caller ends up driving the wrong browser while the reply says it worked.
func (s *Server) peerOf(stmt directive.Statement) (*jrpcHandle, string, *envelope.Slot) {
	if !isPeerKind(stmt.Kind) {
		slot := badRequest(stmt, "not a peer: "+stmt.Kind)
		return nil, "", &slot
	}
	sess, bad := s.resolveTunnelSession(stmt.Target)
	if bad != nil {
		slot := envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": CodeSessionNotRunning,
		}, Body: "no running session named " + stmt.Target + ", so there is no " + stmt.Kind +
			" peer on it. `ls` lists the live sessions; `connect " + stmt.Target + "` makes one."}
		return nil, "", &slot
	}

	// `<index>@<target>`: the index is the connection, everything after the `@` is the
	// browser's own target id. Splitting on the LAST `@` keeps a target id containing
	// one (none do today, but the rule should not depend on that).
	connIndex, target := "", ""
	if stmt.Key != "" {
		if at := strings.IndexByte(stmt.Key, '@'); at >= 0 {
			connIndex, target = stmt.Key[:at], stmt.Key[at+1:]
			if target == "" {
				slot := badRequest(stmt, "`"+stmt.Target+" "+stmt.Kind+"…@` names no target. "+
					"A page address is `<session> "+stmt.Kind+":<n>@<targetId>`; `ls "+stmt.Target+" "+stmt.Kind+
					":<n>` prints the ids.")
				return nil, "", &slot
			}
		} else if idx, isIndex := directive.ChannelIndex(stmt.Key); isIndex {
			connIndex = idx
		} else if !peerSubcommand(stmt.Kind, stmt.Key) {
			// THE WORD AFTER THE ADDRESS MAY BE A SUBCOMMAND (`protocol`, `tools`), and
			// it is accepted only where one exists: `@targetId` belongs to a browser, so
			// `cdp:1@x` is an address while `mcp:1@x` is a mistake (refused below).
			slot := badRequest(stmt, "`"+stmt.Target+" "+stmt.Kind+" "+stmt.Key+"` names nothing. "+
				"An address is `<session> "+stmt.Kind+":<n>` (a connection)"+pageSuffix(stmt.Kind)+
				", and the words that may follow it are: "+strings.Join(peerSubcommands(stmt.Kind), ", ")+
				".\n"+
				"Write `"+stmt.Target+" "+stmt.Kind+":<n> "+stmt.Key+"` — the address comes first, and "+
				"`"+stmt.Kind+":` with no number is the address too when there is exactly one connection.")
			return nil, "", &slot
		}
	}

	h, refusal := s.peerConn(stmt, sess.ID, stmt.Kind, connIndex)
	if refusal != nil {
		return nil, "", refusal
	}
	// ONLY A BROWSER HAS PAGES.
	if target != "" && stmt.Kind != "cdp" {
		slot := badRequest(stmt, "`"+stmt.Target+" "+stmt.Kind+":"+h.index+"@"+target+"` names nothing: only a cdp peer has "+
			"targets, and `"+stmt.Kind+"` is one connection with no children.")
		return nil, "", &slot
	}
	return h, target, nil
}

// peerConn finds one connection by index, or the only one when no index is given.
func (s *Server) peerConn(stmt directive.Statement, sessionID, kind, index string) (*jrpcHandle, *envelope.Slot) {
	jrpcChannelsMu.Lock()
	defer jrpcChannelsMu.Unlock()
	if index != "" {
		if h, ok := jrpcChannels[peerKey(sessionID, kind, index)]; ok {
			return h, nil
		}
		live := liveAddrsLocked(sessionID, kind)
		body := "this session has no " + kind + ":" + index + ", so that address names nothing."
		if len(live) > 0 {
			body += " Live connections: " + strings.Join(live, ", ") + "."
		}
		slot := envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "not_found",
		}, Body: body}
		return nil, &slot
	}
	// No index: allowed only while the answer is unique.
	var found *jrpcHandle
	n := 0
	for _, h := range jrpcChannels {
		if h.sessionID == sessionID && h.kind == kind {
			found, n = h, n+1
		}
	}
	switch n {
	case 1:
		return found, nil
	case 0:
		slot := envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "not_found",
		}, Body: "this session has no " + kind + " connection, so `" + kind + "` names nothing. " +
			"Open one first: `open <session> " + kind + " <url>` (a browser's devtools endpoint is http://host:port)."}
		return nil, &slot
	default:
		live := liveAddrsLocked(sessionID, kind)
		slot := envelope.Slot{Code: 400, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "ambiguous",
		}, Body: "this session has " + fmt.Sprint(n) + " " + kind + " connections, so `" + kind +
			"` alone is ambiguous. Name one: " + strings.Join(live, ", ") + "  (`ls` prints them too)."}
		return nil, &slot
	}
}

// liveAddrsLocked renders a session's connections of one kind, sorted.
func liveAddrsLocked(sessionID, kind string) []string {
	out := make([]string, 0, 2)
	for key := range jrpcChannels {
		if strings.HasPrefix(key, sessionID+" "+kind+":") {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// peerAddress renders a peer's address for a reply: `<session> <kind>[:<index>]`.
func peerAddress(sessionID, kind, index string) string {
	return sessionID + " " + kind + ":" + index
}

// pageAddress renders one attached tab: the peer, then the browser's own target id.
//
// THE PAGE IS NOT A NUMBERED NODE. A cdp peer manages a whole browser instance, and
// the browser already names its tabs — `Target.getTargets` returns that id, the tab
// keeps it for its lifetime, and no other tab will ever reuse it. Numbering them here
// would be a second naming scheme for something the peer already names, and it would
// make `:N` mean both "which browser" and "which tab".
func pageAddress(h *jrpcHandle, target string) string {
	return peerAddress(h.sessionID, h.kind, h.index) + "@" + target
}

// closePeersBySession releases every peer that tunnels through sessionID.
//
// A PEER'S TRANSPORT IS THE SESSION'S SSH CHANNEL, so when the session goes dead the
// peer is a socket to nowhere: no frame can ever arrive again, but the handle stays
// addressable and keeps its frame ring — up to 2048 frames or 8 MiB each — for the
// life of the process. Forwards already cascade this way (forwardMgr.CloseBySession);
// peers did not, and the two views disagreed: `ls cdp` hid the dead peer while
// `read <handle>` still answered from it.
//
// Closing outside the lock matters: ch.Close() talks to the transport, and a dead
// transport is exactly the case that can block.
func closePeersBySession(sessionID string) {
	if sessionID == "" {
		return
	}
	jrpcChannelsMu.Lock()
	var doomed []*jrpcHandle
	for key, h := range jrpcChannels {
		if h.sessionID == sessionID {
			doomed = append(doomed, h)
			delete(jrpcChannels, key)
		}
	}
	jrpcChannelsMu.Unlock()
	for _, h := range doomed {
		// A handle without a channel is one that failed before it was usable. This runs
		// on the session-death path, where a nil dereference would take the process down
		// with the session it was cleaning up after.
		if h.ch != nil {
			_ = h.ch.Close()
		}
	}
}

// closePeer ends a peer and forgets it.
func (s *Server) closePeer(stmt directive.Statement, h *jrpcHandle) envelope.Slot {
	jrpcChannelsMu.Lock()
	delete(jrpcChannels, peerKey(h.sessionID, h.kind, h.index))
	jrpcChannelsMu.Unlock()
	_ = h.ch.Close()
	return noContent(stmt)
}

// peerChannel routes read/call/close at a frame peer onto the jrpc layer.
//
// TWO SHAPES ARRIVE HERE. A peer opened as `<session> <kind>` (a browser, a network
// MCP server) is resolved from the statement's own target and kind; a stdio MCP
// server running ON a shell is addressed by its channel handle (`a1f3:1`) and never
// reaches the registry at all. Both speak JSON-RPC, and the caller should not have
// to know which kind of pipe it is talking through — so the mapping happens here,
// once, and the one-time MCP handshake stays where it is (per handle).
func (s *Server) peerChannel(ctx context.Context, stmt directive.Statement, win termArgs, sub string, rest []string) []envelope.Slot {
	// `read`/`write` are this tool's general words for taking and putting bytes; on a
	// peer they are its recv and send.
	switch sub {
	case "read":
		sub = "recv"
	case "write":
		sub = "send"
	}
	frame, frameBad := callFrameBytes(stmt)
	if frameBad != nil {
		return []envelope.Slot{*frameBad}
	}

	var h *jrpcHandle
	index := ""
	if isPeerKind(stmt.Kind) {
		var bad *envelope.Slot
		h, index, bad = s.peerOf(stmt)
		if bad != nil {
			return []envelope.Slot{*bad}
		}
	} else {
		// A CHANNEL HANDLE. It is usually a command's output, only sometimes a
		// JSON-RPC server — and when it is not a peer, typing at the process is the
		// right thing to do: no handshake, because a handshake would consume the very
		// frames the caller is about to read.
		jrpcChannelsMu.Lock()
		found, ok := jrpcChannels[stmt.Target]
		jrpcChannelsMu.Unlock()
		if !ok {
			return []envelope.Slot{s.mcpStdio(ctx, stmt, sub, stmt.Target, []string{frame}, win)}
		}
		h = found
	}

	// THE WINDOW IS THIS SIDE'S OWN DEADLINE (#158). Without it the only bound on a
	// peer call is the caller's context, which belongs to a host watchdog this daemon
	// cannot see — so a peer that never answers hangs until the host kills the turn,
	// and what the caller reads is a watchdog abort rather than "the peer is not
	// answering". A waiter gets the caller's window, or 15s when none was asked for.
	callCtx, stopCall := ctx, context.CancelFunc(func() {})
	if sub == "call" || sub == "recv" {
		callCtx, stopCall = context.WithTimeout(ctx, peerCallWindow(win))
		defer stopCall()
	}

	// A BROWSER SESSION IS NOT A CHANNEL: the frame is written by the caller, and the
	// one thing this layer adds is the sessionId. Everything else (the method table,
	// the parameters) stays the caller's, which is why there is no CDP command enum
	// anywhere in this file.
	if h.kind == "cdp" && (sub == "call" || sub == "send") {
		f, bad := parseJRPCFrame(stmt, []string{frame})
		if bad != nil {
			return []envelope.Slot{*bad}
		}
		if sub == "send" || boolFlag(stmt, "send") {
			return []envelope.Slot{s.cdpSend(callCtx, stmt, h, index, f)}
		}
		return []envelope.Slot{namePeerTimeout(callCtx, s.cdpCall(callCtx, stmt, h, win, index, f))}
	}
	addr := peerKey(h.sessionID, h.kind, h.index)
	out := stmt
	out.Op = h.kind
	return []envelope.Slot{namePeerTimeout(callCtx, s.jrpcChannel(callCtx, out, win, h.kind, sub, []string{addr, frame}))}
}

// callFrameBytes is the frame this statement sends: the payload as written, or the bytes
// of the file it names with `@<path>` (#354).
//
// WHY A PATH AND NOT ONLY A PAYLOAD. A frame is sliced out of the command line, and that
// same line must also be one valid shell line — so a frame of a few KB, or one whose own
// bytes contain the shell's quoting, has no spelling that satisfies both readers. A path
// is a short word with no quotes in it, and its bytes are read AFTER the line is parsed,
// so nothing between the caller and the peer can touch them.
//
// `@` CANNOT BE MISTAKEN FOR AN ADDRESS: a peer's `@<targetId>` is part of the ADDRESS
// (`cdp:1@7D87`), which the parser has already consumed as the key by the time the
// payload starts, and a JSON frame never begins with `@`.
func callFrameBytes(stmt directive.Statement) (string, *envelope.Slot) {
	frame := strings.TrimSpace(stmt.Payload)
	// --file AND @<path> ARE TWO SPELLINGS OF ONE THING, so they are refused together
	// when both are present: guessing which one the caller meant is how a frame is sent
	// that nobody wrote (issue #354).
	if flagPath, ok := stmt.Flag("file"); ok {
		if frame != "" {
			slot := badRequest(stmt, "`--file` and an inline frame are two different frames, and this call "+
				"carries both.\nDrop one: `call <peer> --file "+flagPath+"` sends the file's bytes, or "+
				"`call <peer> '<frame>'` sends the line's.")
			return "", &slot
		}
		if strings.TrimSpace(flagPath) == "-" {
			slot := badRequest(stmt, "`--file -` would read this process's standard input, and that is not a "+
				"frame channel: under a stdio MCP transport fd 0 carries the protocol itself, and a frame read "+
				"from it would eat the next request.\nWrite the frame to a file instead — "+
				"`read <session> file <path> --raw | write internal file /tmp/frame.json` then "+
				"`call <peer> --file /tmp/frame.json` — or pass it inline when it is short.")
			return "", &slot
		}
		body, err := readFrameFile(flagPath)
		if err != nil {
			slot := badRequest(stmt, "cannot read the frame from "+flagPath+": "+err.Error()+"\n"+
				"A --file frame is read on the host running termcp, so the path has to exist there.")
			return "", &slot
		}
		var probe any
		if err := json.Unmarshal([]byte(body), &probe); err != nil {
			slot := badRequest(stmt, "the frame in "+flagPath+" is not JSON: "+err.Error()+"\n"+frameSpelling)
			return "", &slot
		}
		return body, nil
	}
	path, isPath := frameFilePath(frame)
	if !isPath {
		return frame, nil
	}
	if path == "" {
		slot := badRequest(stmt, "an @ frame names a path: `call <peer> @/tmp/frame.json`.\n"+
			"The frame then travels as the file's bytes instead of as the line's, which is how a large "+
			"frame or one full of quotes is sent without a quoting layer touching it.")
		return "", &slot
	}
	body, err := readFrameFile(path)
	if err != nil {
		slot := badRequest(stmt, "cannot read the frame from "+path+": "+err.Error()+"\n"+
			"An @<path> frame is read on the host running termcp, so the path has to exist there. "+
			"Pass the frame inline when it is short: "+`call <peer> '{"method":"…"}'`)
		return "", &slot
	}
	// THE FILE'S OWN BYTES ARE CHECKED HERE, so the refusal can name the PATH. The frame
	// parser below would say "the frame is not JSON" and the caller would go back to the
	// command line, which no longer holds those bytes.
	var probe any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		slot := badRequest(stmt, "the frame in "+path+" is not JSON: "+err.Error()+"\n"+frameSpelling)
		return "", &slot
	}
	return body, nil
}

// frameFilePath reports the path a frame payload names with `@<path>`, and whether it
// names one at all.
//
// QUOTING IS GROUPING HERE TOO: a path with a space in it is written in quotes, and the
// one layer of them is peeled before the path is read — the same reading every other
// frame spelling gets.
func frameFilePath(frame string) (string, bool) {
	switch {
	case strings.HasPrefix(frame, "@"):
		return strings.TrimSpace(frame[1:]), true
	case strings.HasPrefix(frame, "'@"), strings.HasPrefix(frame, `"@`):
		unquoted := strings.TrimSpace(unquoteWord(frame))
		if strings.HasPrefix(unquoted, "@") {
			return strings.TrimSpace(unquoted[1:]), true
		}
	}
	return "", false
}

// maxFrameFileBytes bounds a frame read through `@<path>`.
//
// The path is the caller's and the bytes land in this process before they are sent, so
// the read is bounded the way a peer's own frame buffer is (8 MiB). A file over the limit
// is refused rather than truncated: half a JSON document is not a smaller frame.
const maxFrameFileBytes = 8 << 20

// readFrameFile reads one frame from disk, bounded and trimmed.
//
// A trailing newline is not part of a frame: an editor writes one, a JSON parser ignores
// it, and trimming keeps the echoed payload the caller's bytes rather than their editor's.
func readFrameFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return "", fmt.Errorf("it is a directory, and a frame is one file's bytes")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFrameFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxFrameFileBytes {
		return "", fmt.Errorf("it is larger than the %d MiB limit for a frame read from a file", maxFrameFileBytes>>20)
	}
	return strings.TrimSpace(string(raw)), nil
}

// openPeer opens a peer through a session and returns the handle.
func (s *Server) openPeer(ctx context.Context, stmt directive.Statement, win termArgs, kind string) []envelope.Slot {
	out := stmt
	out.Op = kind
	return []envelope.Slot{s.jrpcOpen(ctx, out, kind, stmt.Target, stmt.Key)}
}

func peerCallWindow(win termArgs) time.Duration {
	if win.TimeoutSet && win.TimeoutSeconds > 0 {
		return time.Duration(win.TimeoutSeconds * float64(time.Second))
	}
	return defaultPeerWindow
}

// namePeerTimeout turns a deadline expiry into a 504 that names the peer.
func namePeerTimeout(ctx context.Context, slot envelope.Slot) envelope.Slot {
	if ctx.Err() == context.DeadlineExceeded && (slot.Code == 500 || slot.Code == 0) {
		slot.Code = 504
		slot.Headers["Error-Code"] = "peer_timeout"
		slot.Body = "the peer did not answer within the window. `call` waited its full budget; " +
			"give the call a larger `timeout` if this method needs longer, or use `--send` for fire-and-forget."
	}
	return slot
}

// peerSubcommands lists the words that may follow an address of this kind, before the
// address itself is resolved.
//
// IT IS A FUNCTION OF THE KIND, not one shared list: `protocol` reads a document only a
// browser publishes, and `tools` lists a table only an MCP peer has. Answering "tools"
// for `cdp` would send the caller to a subcommand that cannot exist, and the refusal it
// then reads blames the address.
//
// `tools` WAS MISSING FROM THE WHITELIST, and the cost was invisible: the word reached
// this check, was refused as part of the address, and the branch that implements it
// (term_directives.go: `mcp tools` and `mcp:1 tools` in the same two positions) could
// never be reached — while the help pages, the per-turn instructions and the repo's own
// agent docs all taught `get <session> mcp tools`. Documented, implemented, unreachable
// (#356).
func peerSubcommands(kind string) []string {
	switch kind {
	case "mcp":
		return []string{"tools"}
	case "cdp":
		return []string{"protocol"}
	default:
		// jrpc is the raw frame peer: it has no self-describing table to read, so the
		// address is the whole grammar and the list is empty rather than invented.
		return nil
	}
}

// peerSubcommand reports whether word is one of them. An empty list accepts nothing,
// which is the correct grammar for a peer whose only address is its handle.
func peerSubcommand(kind, word string) bool {
	for _, s := range peerSubcommands(kind) {
		if s == word {
			return true
		}
	}
	return false
}

// pageSuffix names the `@targetId` form where it exists. Only a browser has pages, and
// printing it for `mcp` would teach an address the next call refuses.
func pageSuffix(kind string) string {
	if kind == "cdp" {
		return " or `<session> " + kind + ":<n>@<targetId>` (a page in it)"
	}
	return ""
}
