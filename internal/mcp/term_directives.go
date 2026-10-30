package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// runDirective dispatches one parsed statement onto the existing handlers.
//
// A directive that names a batch (a comma-separated target) produces one slot per
// entry; everything else produces exactly one. That is the "slots are response
// units" rule: N questions, N answers, each with its own status line.
//
// Namespace reachability is checked HERE, once, before dispatch. The outer tool
// call cannot do it: `term` carries its target inside `cmd`, so the guard that
// reads `shell_id`/`session_id` arguments sees nothing to resolve and admits
// everything. Directives that reach a handler still get the full admission (the
// handler path runs checkAdmission again); the ones that read a manager directly
// — info, ps, span — would otherwise be reachable across namespaces, which is
// exactly the isolation this feature promises.
func (s *Server) runDirective(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	// A HANDLE THE RECONCILER TOOK AWAY EXPLAINS ITSELF HERE, once, for every
	// directive that names one.
	//
	// The channel-level 404 used to be a bare "not found", which reads the same as
	// a typo — and docs/api.md promises the id that named an instance answers with
	// what happened to it (dogfood on #183). Answering at the entrance covers
	// read/input/key/resize/close alike: they all resolve through this one place.
	//
	// The idle clock is renewed in the guard, which already resolves every id
	// spelling — one owner for "is this channel live", not two.
	if stmt.Target != "" {
		if ghost := s.explainClosedChannel(stmt); ghost != nil {
			return []envelope.Slot{*ghost}
		}
	}
	if bad := s.refuseForeignTarget(ctx, stmt); bad != nil {
		return []envelope.Slot{*bad}
	}
	// A CHANNEL DIRECTIVE TAKES A CHANNEL, and the check happens here as well as at
	// parse time. Parse-time covers the ordinary path (the target word is visible in
	// the statement); this one covers the paths the parser cannot see — an alias
	// whose expansion supplies the target, and a target that reached dispatch by
	// some route other than parseStatement. Two enforcement points, one rule, so a
	// future entry cannot reintroduce the silent primaryShell fallback.
	if op, ok := directive.Lookup(stmt.Op); ok && op.TargetKind == directive.TargetChannel {
		if stmt.Target != "" && !directive.NamesAChannel(stmt.Target) {
			return []envelope.Slot{badRequest(stmt,
				directive.ChannelTargetProblem(op, stmt.Target)+"\n"+op.Usage)}
		}
	}
	switch stmt.Op {
	// --- verbs whose target is already the node they act on ---
	//
	// TargetChain normalised `a1f3 1` to `a1f3:1` at parse time, so these four see
	// one spelling and need no kind handling at all.
	case "run":
		return s.directiveRun(ctx, stmt, win)
	case "input":
		return s.directiveType(ctx, stmt, win)
	case "key":
		return s.directiveKey(ctx, stmt, win)
	case "resize":
		return []envelope.Slot{s.directiveResize(ctx, stmt, win)}
	case "span":
		return []envelope.Slot{s.directiveSpan(ctx, stmt, win)}
	case "reader":
		return []envelope.Slot{s.directiveReader(ctx, stmt, win)}
	case "get":
		if stmt.Kind == "task" {
			// A RECEIPT IS READ WITHOUT A SESSION behind it: the transfer it describes
			// outlived the call that started it, and the caller may have no session
			// handle at all by now. `get task <id>` therefore takes no target.
			return []envelope.Slot{s.directiveGetTask(ctx, stmt, win)}
		}
		if stmt.Kind == "host" {
			// `get host <name>` reads one entry back. Without it a caller could create,
			// edit and delete a host but never see what it holds, so a partial edit had
			// to be done blind.
			return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{
				"action": "get", "name": stmt.Key,
			}), win)}
		}
		if stmt.Kind == "ext" {
			// `get ext <name>` reads one template back, which is what makes `ls ext
			// <name>` refusable by name rather than by a shrug. Without it the only way
			// to see one extension was the full listing, so a caller that mistyped a
			// name got fourteen rows and believed it had filtered (issue #143).
			return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{
				"action": "get", "name": stmt.Key,
			}), win)}
		}
		// A PEER'S OWN STATE IS READ HERE, not from the SSH side. `get a1f3 cdp`
		// describes the browser (attached pages, frame cursor); `get a1f3 cdp:1` names
		// one page; `get a1f3 cdp protocol Page` reads the live signature the browser
		// publishes about itself. All three are answered from the peer, so they are
		// routed before the session/file branches that would otherwise refuse them.
		if isPeerKind(stmt.Kind) {
			h, index, bad := s.peerOf(stmt)
			if bad != nil {
				return []envelope.Slot{*bad}
			}
			// `protocol` IS A CHILD OF THE CONNECTION, so it arrives in one of two
			// positions depending on whether the address was qualified:
			//   get internal cdp protocol      -> Key="protocol"
			//   get internal cdp:1 protocol    -> Key="1", Args[0]="protocol"
			// Both mean the same thing; the second is what a caller copies out of a reply.
			if stmt.Key == "protocol" || (len(stmt.Args) > 0 && stmt.Args[0] == "protocol") {
				domain := ""
				if stmt.Key == "protocol" {
					if len(stmt.Args) > 0 {
						domain = stmt.Args[0]
					}
				} else if len(stmt.Args) > 1 {
					domain = stmt.Args[1]
				}
				return []envelope.Slot{s.cdpProtocol(ctx, stmt, h, domain)}
			}
			// `tools` IS THE MCP SIBLING OF `protocol`: the peer's own table, read live,
			// in the same two positions (`mcp tools` and `mcp:1 tools`). An argument names
			// one tool and prints its full input schema.
			if stmt.Kind == "mcp" && (stmt.Key == "tools" || (len(stmt.Args) > 0 && stmt.Args[0] == "tools")) {
				tool := ""
				if stmt.Key == "tools" {
					if len(stmt.Args) > 0 {
						tool = stmt.Args[0]
					}
				} else if len(stmt.Args) > 1 {
					tool = stmt.Args[1]
				}
				return []envelope.Slot{s.mcpToolList(ctx, stmt, h, win, tool)}
			}
			return []envelope.Slot{s.cdpDescribe(stmt, h, index)}
		}
		if stmt.Kind == "file" {
			// --op names a SUB-ACTION; the sfTP layer groups those under an action
			// (perm, link, fs) and dispatches again inside the group. The group is
			// derivable from the op, so the caller never has to know it — and asking for
			// `--op stat` must not reach the fs group, where the answer is "action must
			// be truncate, realpath, or statvfs".
			op, _ := stmt.Flag("op")
			if op == "" {
				return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
					"action": "stat", "remote-path": stmt.Key,
				}), win)}
			}
			action, ok := fileGroupForOp(op)
			if !ok {
				return []envelope.Slot{badRequest(stmt, "get --op does not know "+quoteOrDash(op)+
					". It takes stat, realpath, readlink, getwd, urls, or one of: chmod, chown, chtimes, truncate, statvfs.")}
			}
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
				"action": action, "op": op, "remote-path": stmt.Key,
			}), win)}
		}
		return []envelope.Slot{s.directiveInfo(ctx, stmt, win)}
	case "notify":
		return []envelope.Slot{s.directiveNotify(ctx, stmt, win)}
	case "help":
		return []envelope.Slot{s.directiveHelp(stmt, win)}

	// --- connect: a new SSH link ---
	case "connect":
		return s.directiveOpen(ctx, stmt, win, wantPTY(stmt))

	// --- open: a child of an existing session, or a peer through it ---
	case "open":
		switch stmt.Kind {
		case "forward":
			// THE DIRECTION IS A FLAG, and local is the default because it is the one a
			// caller reaches for when they say "forward this port" without qualifying it.
			// Without this, every forward was local: `--dynamic` and `--remote` were not
			// flags of `open` at all, so SOCKS5 and remote forwards became unreachable in
			// the rewrite while the detail page still described all three.
			action := "local"
			if boolFlag(stmt, "dynamic") {
				action = "dynamic"
			} else if boolFlag(stmt, "remote") {
				action = "remote"
			}
			return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, map[string]string{
				"action": action,
			}), win)}
		case "mcp", "cdp", "jrpc":
			// ATTACH RIDES ON OPEN, because attaching a page IS opening a child of the
			// browser — the same verb that opened the browser. The target id is a flag
			// because the key position already names the URL.
			if target, ok := stmt.Flag("target"); ok {
				h, _, bad := s.peerOf(stmt)
				if bad != nil {
					return []envelope.Slot{*bad}
				}
				return []envelope.Slot{s.cdpAttach(ctx, stmt, h, target)}
			}
			return s.openPeer(ctx, stmt, win, stmt.Kind)
		case "ext":
			// THE EXTENSION LAYER'S OPEN (issue #183). An extension that declares no
			// forward is not dialled until something asks for it, and this is the ask:
			// `open ext <name>` brings it up and hands back the handles, the same way
			// `open <session>` does for a session. There is no target here — the
			// reconciler owns the container, and a caller that could name one would be
			// able to open a channel on somebody else's connection.
			return []envelope.Slot{s.directiveExtensionUse(ctx, stmt, win)}
		default:
			return s.directiveOpen(ctx, stmt, win, wantPTY(stmt))
		}

	// --- ls: every collection is one verb ---
	case "ls":
		return s.directiveLS(ctx, stmt, win)

	// --- rm: a live node, or saved configuration ---
	case "rm":
		switch {
		case stmt.Kind == "file":
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
				"action":      "delete",
				"remote-path": stmt.Key,
			}), win)}
		case stmt.Kind == "host":
			return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{
				"action": "delete", "name": stmt.Key,
			}), win)}
		case stmt.Kind == "ext":
			return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{
				"action": "delete", "name": stmt.Key,
			}), win)}
		case stmt.Kind == "alias":
			return []envelope.Slot{s.directiveAlias(ctx, aliasStatement(stmt, true), win)}
		default:
			return []envelope.Slot{s.directiveRemove(ctx, stmt, win)}
		}

	// --- close: a channel, a session, a tunnel or a peer ---
	//
	// A NAMED KIND NEVER FALLS THROUGH. `close <session> forward <id>` is a tunnel;
	// `close <session>` is the container. If the kind is one this verb does not
	// handle, the answer is a 400 — not "close whatever the target happened to be".
	// It fell through once: `close <session> mcp 1` parsed the kind away, found no
	// branch, and took the session down, so a caller closing a peer destroyed every
	// channel on the link. That is the same shape as #132 (`close <session> --force`
	// closing the primary channel): a qualifier read as decoration.
	case "close":
		switch stmt.Kind {
		case "ext":
			// HANDING AN INSTANCE BACK (issue #183). An on-demand extension's instances
			// are the caller's to keep or give up, and a caller that knows it has
			// finished says so here rather than leaving a process on the target until
			// its silence runs out. The handle names which one; leaving it off means
			// "all of them", which is what a caller that did not keep the handle means.
			return []envelope.Slot{s.directiveExtensionRelease(ctx, stmt, win)}
		case "task":
			return []envelope.Slot{s.directiveCloseTask(ctx, stmt)}
		case "forward":
			// THE ID IS THE NODE, so it arrives as the key rather than as a flag: the tree
			// says `close <session> forward <id>`, and asking for `--forward-id` here made
			// the same object addressable two ways — a positional key when opening it,
			// a flag when closing it. An explicit flag still wins, so a caller who wrote
			// it out is not punished.
			flags := map[string]string{"action": "close"}
			if stmt.Key != "" {
				flags["forward-id"] = stmt.Key
			}
			return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, flags), win)}
		case "cdp", "mcp", "jrpc":
			// `close a1f3 cdp:1` detaches one page; `close a1f3 cdp` closes the peer.
			h, index, bad := s.peerOf(stmt)
			if bad != nil {
				return []envelope.Slot{*bad}
			}
			if index != "" {
				return []envelope.Slot{s.cdpDetach(ctx, stmt, h, index)}
			}
			return []envelope.Slot{s.closePeer(stmt, h)}
		case "":
			// AN EXPLICIT --forward-id NAMES A FORWARD, and it must not be swallowed by
			// the container close (dogfood finding on #159). The parser accepts the flag
			// — it is declared on this verb — and the dispatcher then ignored it, so
			// `close <session> --forward-id fw-…` answered 204 and TERMINATED THE WHOLE
			// SESSION: the caller asked to stop one tunnel and silently lost the
			// connection. `--grace` is the mirror image: a session deadline that must not
			// be ignored either.
			//
			// This is not reading intent out of a flag (the mistake `close <channel>
			// --force` was fixed for): no other object a close can name has a forward
			// id, so the flag picks the node rather than modifying it.
			if fid, ok := stmt.Flag("forward-id"); ok && fid != "" {
				if stmt.FlagState("grace") != directive.FlagAbsent {
					return []envelope.Slot{badRequest(stmt, "`--grace` is a session deadline; "+fid+
						" is a forward, and closing a tunnel has nothing to wait for.\n"+
						"Close the tunnel and the connection separately: `close "+stmt.Target+" --forward-id "+fid+
						"`, then `close "+stmt.Target+" --grace N` if that is also wanted.")}
				}
				return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, map[string]string{
					"action": "close", "forward-id": fid,
				}), win)}
			}
			if isPeerHandle(stmt.Target) {
				return s.peerChannel(ctx, stmt, win, "close", nil)
			}
			return []envelope.Slot{s.directiveClose(ctx, stmt, win)}
		default:
			return []envelope.Slot{badRequest(stmt, "close does not take a "+stmt.Kind+
				". A peer is closed by its address: `close <session> cdp`.")}
		}

	// --- read: a channel's new bytes, a file's content, or a peer's frames ---
	case "read":
		// A PIPELINE IS ONE STATEMENT WITH TWO ENDS, and it is answered by the copy
		// before any of the single-ended read paths below can claim it.
		if stmt.Pipe != nil {
			return s.pipeCopy(ctx, stmt, win)
		}
		// The right half of a pipeline is only reachable through its left half, so
		// reaching it here means something routed it alone. Refuse rather than run a
		// write against a target nobody named.
		if stmt.Piped {
			return []envelope.Slot{badRequest(stmt, "this is the right half of a `|`; it runs as part of the pipeline")}
		}
		if stmt.Kind == "file" {
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
				"action":      "read",
				"remote-path": stmt.Key,
			}), win)}
		}
		// A REGISTRY HANDLE IS ALWAYS A FRAME SOURCE; A SHELL IS BYTES UNLESS ASKED.
		//
		// A network peer has no byte stream to speak of, so `read` on one can only mean
		// "the next frame". A pipe channel is the opposite: it is usually a command's
		// output, and only sometimes a JSON-RPC server — so frames need the flag, or
		// every ordinary `read` of a command's output would be parsed as protocol.
		if isPeerKind(stmt.Kind) {
			h, index, bad := s.peerOf(stmt)
			if bad != nil {
				return []envelope.Slot{*bad}
			}
			// THE FRAME LOG HAS TWO AXES, and the flags pick one of them.
			//   --since  pages forward in TIME: what arrived after n, again and again.
			//   --at     pages inside ONE frame: that frame's bytes from b onward. A
			//            frame too big to inline is answered as a receipt naming its
			//            number, and this is the read that receipt tells you to make.
			// A plain read without either is the other question — the next frame off
			// the queue, once — which is what `send` pairs with.
			if at, ok := stmt.Flag("at"); ok {
				n, err := strconv.ParseInt(strings.TrimSpace(at), 10, 64)
				if err != nil || n < 1 {
					return []envelope.Slot{badRequest(stmt, "--at takes a frame number (the number a receipt reported as Frame), got "+strconv.Quote(at))}
				}
				offset := 0
				if raw, ok := stmt.Flag("offset"); ok {
					v, err := strconv.Atoi(strings.TrimSpace(raw))
					if err != nil || v < 0 {
						return []envelope.Slot{badRequest(stmt, "--offset takes a byte offset, got "+strconv.Quote(raw))}
					}
					offset = v
				}
				limit := 0
				if raw, ok := stmt.Flag("bytes"); ok {
					v, err := strconv.Atoi(strings.TrimSpace(raw))
					if err != nil || v <= 0 {
						return []envelope.Slot{badRequest(stmt, "--bytes takes a positive page size, got "+strconv.Quote(raw))}
					}
					limit = v
				}
				// One page per frame at a time, so the answer is a slice of a known
				// frame rather than an interleaving whose offsets mean nothing.
				return []envelope.Slot{s.pageFrame(stmt, h, n, offset, limit)}
			}
			if since, ok := stmt.Flag("since"); ok {
				n, err := parseSince(since)
				if err != nil {
					return []envelope.Slot{badRequest(stmt, err.Error())}
				}
				grep, _ := stmt.Flag("grep")
				// THE PAGE IS A FILTER, NOT A DIFFERENT STREAM. One socket carries every
				// attached page's frames interleaved, each tagged with its sessionId —
				// so `read <session> cdp:1 --since N` means "the ones that belong to this
				// page". Without the filter the index would be accepted and ignored,
				// which reads like a scoped answer and is not.
				return []envelope.Slot{s.cdpEvents(ctx, stmt, h, index, n, grep, win)}
			}
			return s.peerChannel(ctx, stmt, win, "read", nil)
		}
		if boolFlag(stmt, "frame") {
			return s.peerChannel(ctx, stmt, win, "read", nil)
		}
		return s.directiveRead(ctx, stmt, win)

	// --- write: a file's content ---
	//
	// THE FRAME FORM LIVES ON call. Both are "put bytes somewhere", but a frame is a
	// payload (verbatim, quotes and all) and a file write is a set of flags
	// (--data/--mode/--offset), and one verb cannot take both: the parser has one
	// payload rule per directive, so whichever form it picked would be wrong for the
	// other. `call` already carries a verbatim payload, which is exactly what a frame
	// needs.
	case "write":
		if stmt.Kind == "file" {
			// --old-text makes it a replace: one write verb, and whether it edits in
			// place is a property of what was supplied, not a second word to learn.
			action := "write"
			extra := map[string]string{}
			if _, ok := stmt.Flag("old-text"); ok {
				action = "replace"
			} else if op, ok := stmt.Flag("op"); ok {
				// --op NAMES THE SUB-ACTION and the group comes from the same table `get`
				// uses, so the two verbs cannot drift into disagreeing about what a chmod
				// is. --mode alone still means chmod: a mode with no content can only be
				// a permission change.
				group, known := fileGroupForOp(op)
				if !known {
					return []envelope.Slot{badRequest(stmt, "write --op does not know "+quoteOrDash(op))}
				}
				action, extra["op"] = group, op
			} else if _, ok := stmt.Flag("mode"); ok {
				if _, hasData := stmt.Flag("data"); !hasData {
					action, extra["op"] = "perm", "chmod"
				}
			}
			flags := map[string]string{"action": action, "remote-path": stmt.Key}
			for k, v := range extra {
				flags[k] = v
			}
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, flags), win)}
		}
		return []envelope.Slot{badRequest(stmt, "write needs a kind: write <session> file <path>")}

	// --- call: one request, one reply ---
	//
	// THE HANDLE MAY BE A PEER OR A SHELL. A network peer is opened with
	// `open <session> mcp <url>` and gets a registry handle; a stdio peer is a pipe
	// channel already running the server, addressed by its channel handle. Both are
	// "something that speaks JSON-RPC", and the caller should not have to know which
	// kind of pipe it is talking through — so one verb covers both.
	case "call":
		return s.peerChannel(ctx, stmt, win, "call", nil)

	// --- ls on a peer lists what it holds, not what the SSH side holds ---
	//
	// It is the same question `ls <session>` asks (`what hangs off this node`), so
	// it is the same verb. A browser's children are tabs; a jrpc peer has none.
	// (Handled in directiveLS, which every `ls` reaches.)

	// --- configuration, which outlives every session ---
	case "create", "edit":
		act := "create"
		if stmt.Op == "edit" {
			act = "edit"
		}
		switch stmt.Kind {
		case "file":
			// Only create reaches here: a file is made with mkdir, or as a symlink when
			// --target names what it points at.
			//
			// THE LINK GROUP NEEDS AN --op AS WELL AS THE ACTION, and the caller should not
			// have to supply one: --target already says this is a symlink, so requiring
			// `--op symlink` too would be a second way to say the same thing. Note the
			// argument order flips going in: the node's key is the LINK path and --target
			// is what it points at, while the handler calls those link_path and target.
			action := "mkdir"
			flags := map[string]string{"action": action, "remote-path": stmt.Key}
			if target, ok := stmt.Flag("target"); ok {
				// A HARD LINK IS THE SAME SENTENCE with --op link: the key is the new
				// name, --target is what it already exists as. Without this branch a hard
				// link had no spelling at all — `write --op link` wanted existing_path and
				// new_path, and neither is a flag of write.
				if op, _ := stmt.Flag("op"); op == "link" {
					flags["action"] = "link"
					flags["op"] = "link"
					flags["new-path"] = stmt.Key
					flags["existing-path"] = target
					return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, flags), win)}
				}
				flags["action"] = "link"
				flags["op"] = "symlink"
				flags["link-path"] = stmt.Key
				flags["target"] = target
			}
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, flags), win)}
		case "host":
			return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{
				"action": act, "name": stmt.Key,
			}), win)}
		case "ext":
			// `update` was the old name for the same write; ext kept it because the
			// config file calls the field `update`. One verb here, one word there.
			if act == "edit" {
				act = "update"
			}
			// --restart REBUILDS THE SESSION, and it is the one thing on this node that is
			// not a configuration change. An edit already rebuilds when it changes the
			// session template — the reconciler compares a fingerprint — but not when the
			// template reads the same and only the FILE it names has changed, which is
			// exactly a git pull. Then the process is still running the old script, and the
			// DSL is the only entrance an agent gets, so without this it had no way to say
			// "rebuild it": the reconciler's action was reachable from the MCP tool and the
			// admin API, but not from the language the agent actually speaks.
			if boolFlag(stmt, "restart") {
				if other := firstFlagOtherThan(stmt, "restart"); other != "" {
					return []envelope.Slot{badRequest(stmt,
						"--restart rebuilds the session and changes no configuration, so it cannot be combined with --"+
							other+"; send the edit and the rebuild as two calls")}
				}
				act = "retry"
			}
			return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{
				"action": act, "name": stmt.Key,
			}), win)}
		case "alias":
			return []envelope.Slot{s.directiveAlias(ctx, aliasStatement(stmt, false), win)}
		}
		usage := ""
		if op, ok := directive.Lookup(stmt.Op); ok {
			usage = "\n" + op.Usage
		}
		return []envelope.Slot{badRequest(stmt, stmt.Op+" needs a kind: "+stmt.Op+" host|ext|alias <name>"+usage)}
	}
	return []envelope.Slot{{
		Code:    400,
		Headers: map[string]string{"Error-Code": "unknown_directive"},
		Body:    "unknown directive: " + stmt.Op,
	}}
}

// refuseForeignTarget applies namespace isolation to every directive, including
// the ones that never reach a handler.
//
// A batch is checked entry by entry and refused as a whole: running part of it
// against somebody else's session would make the refusal partial and the outcome
// unknowable.
// explainClosedChannel answers a directive that names a channel the reconciler
// closed: the id it took away is not a typo, and a caller can only learn that from
// the reply it gets.
func (s *Server) explainClosedChannel(stmt directive.Statement) *envelope.Slot {
	if s.extensions == nil || s.sessMgr == nil {
		return nil
	}
	// Still here: nothing to explain.
	if s.sessMgr.GetChildShell(stmt.Target) != nil || s.sessMgr.GetSessionByShellID(stmt.Target) != nil {
		return nil
	}
	if _, _, _, ok := s.extensions.ExplainClosed("", stmt.Target); !ok {
		return nil
	}
	slot := failureSlot(&toolFailure{code: CodeShellNotFound, msg: s.shellNotFoundMessage(stmt.Target)})
	slot.Headers["Path"] = stmt.Path
	return &slot
}

func (s *Server) refuseForeignTarget(ctx context.Context, stmt directive.Statement) *envelope.Slot {
	callerNS := callerNamespace(ctx)
	if callerNS == "" || stmt.Target == "" {
		return nil // the operator, or a directive that names nothing
	}
	ids, _ := splitBatchIDs(stmt.Target)
	for _, id := range ids {
		if id == "" {
			continue
		}
		single := stmt
		single.Target = id
		sess := s.sessionForStatement(single)
		if sess == nil || ns.Reachable(sess.Namespace, callerNS) {
			continue
		}
		slot := failureSlot(&toolFailure{code: CodeForbidden, msg: ns.ForbiddenForeign(callerNS)})
		slot.Headers["Path"] = stmt.Path
		return &slot
	}
	return nil
}

// sessionForStatement resolves the session a directive names, for the admission
// check. A handle may be a session id, a shell id, or a locator — the same
// spellings the tool surface accepts, because a second spelling rule would be a
// second place to get it wrong.
func (s *Server) sessionForStatement(stmt directive.Statement) *session.Session {
	if stmt.Target == "" || s.sessMgr == nil {
		return nil
	}
	guard := toolGuard{server: s}
	if sess := guard.sessionForSessionID(stmt.Target); sess != nil {
		return sess
	}
	return guard.sessionForShellID(stmt.Target)
}

// The directives themselves.
//
// Each one translates to the handler that already implements it and re-renders
// the reply. Nothing below knows how to write a file, open a port or read a
// buffer; that is what keeps the single entry a re-rendering of the surface
// instead of a parallel implementation of it.

// directiveRun is one command line in one call: type the payload, press enter,
// read the window — and hand back the slot.
//
// The completion rule differs by mode, and that difference is the whole reason
// `run` exists: on a pipe channel the process exiting IS completion, so it
// returns the moment the channel closes; on a pty there is no such signal, so it
// waits out the caller's time window and returns what the terminal produced.
func (s *Server) directiveRun(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if ids, isBatch := splitBatchIDs(stmt.Target); isBatch {
		slots := make([]envelope.Slot, 0, len(ids))
		for _, id := range ids {
			single := stmt
			single.Target = id
			slots = append(slots, s.runOne(ctx, single, win))
		}
		return slots
	}
	return []envelope.Slot{s.runOne(ctx, stmt, win)}
}

// runOne executes one `run` against one handle.
func (s *Server) runOne(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// The cursor is captured BEFORE the bytes go out: the window must start where
	// this command's output begins, or a previous command's unread tail would be
	// delivered as if it belonged to this one.
	start, ok := s.outputSizeFor(stmt.Target)
	if !ok {
		// THE SAME ADVICE ON BOTH ROADS. A finished pipe can be refused here, before
		// anything is typed, or by the write below; only the second used to carry the
		// explanation, so a caller that arrived on the first got "process has exited"
		// with no way forward.
		return s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, s.probeShell(stmt.Target), s))
	}

	// THE WINDOW IS CHECKED BEFORE ANYTHING IS TYPED, because a 400 must not be a lie.
	//
	// The window is validated where it is read — inside shell_output — and that read
	// happens after this function has typed the line and pressed Enter. So a refusal
	// about the window arrives after the command has already run (issue #179), and a 4xx
	// reads as "nothing happened": the caller retries, and the command runs twice. On a
	// mutating command the retry is the damage.
	//
	// The rule that made this reachable was the upper bound on `timeout`, which only
	// the read validator had (issue #168): every other window number is checked at the
	// entrance, before dispatch, so the tool could not show the lie. Removing that bound
	// removes the trigger; this check removes the CLASS, because the next rule added to
	// the read validator would otherwise arrive after the effect again. The same
	// validator is called with the same numbers, just earlier — one rule, two callers.
	if f := s.checkWindow(stmt, win); f != nil {
		return s.slotFromFailure(ctx, stmt, f)
	}

	// THE LINE IS ONE UNIT: THE TEXT AND ITS ENTER LAND TOGETHER (issue #212).
	//
	// These are two calls into the shell layer, and nothing used to keep a second
	// `run` from landing between them -- so two concurrent runs wrote
	// `{"id":1}{"id":2}\n\n`, which is neither two lines nor one valid line, and a
	// line-protocol peer (a stdio MCP server, a JSON-RPC relay) read it as corruption
	// and exited.
	//
	// THE LOCK COVERS THE WRITES ONLY. The wait for output below must not hold it, or
	// a slow command would park every later run on this channel for the length of its
	// window -- the same reason recvReaders is per handle rather than global.
	line := s.lineLock(stmt.Target)
	line.Lock()
	_, inputFail := s.callTool(ctx, "shell_input", argsOf("shell_id", stmt.Target, "text", stmt.Payload))
	if inputFail != nil {
		line.Unlock()
		return s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, inputFail, s))
	}
	keyRes, keyFail := s.callTool(ctx, "shell_key", argsOf("shell_id", stmt.Target, "key", "enter"))
	line.Unlock()
	if keyFail != nil {
		return s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, keyFail, s))
	}
	// A held command line: review mode staged the text and queued it on the enter.
	// The reply is the review shape, not a read — there is nothing to read yet.
	if held := reviewPendingFrom(keyRes); held != nil {
		return held.slot(stmt.Path)
	}

	// --grep CHANGES WHAT THE WINDOW COUNTS, so it takes the same path `read` does
	// (matches rather than raw lines) — scoped to this command, which is the region the
	// caller just created (issue #187).
	//
	// IT WAITS FIRST, THE SAME WAY THE PLAIN PATH DOES. A search over bytes that have not
	// arrived is not a search: it scanned nothing and answered "no line matched … in the 0
	// lines scanned", which is true and useless — found by driving it, not by a test. The
	// wait is the same one the read half uses, so the two halves of a `run` cannot
	// disagree about when the command's output is there.
	if _, ok := stmt.Flag("grep"); ok {
		waited := s.awaitRunOutput(ctx, stmt, win, start)
		slot := s.readGrepFrom(ctx, stmt, win, start, readTimeout(remaining(win, waited)))
		if slot.Code < 400 {
			if slot.Headers == nil {
				slot.Headers = map[string]string{}
			}
			// A searched run is still a run: the caller needs the offset its input landed
			// at as much here as anywhere (issue #194).
			slot.Headers["Input-At"] = strconv.FormatInt(start, 10)
		}
		return slot
	}
	slot := s.readSlotAfter(ctx, stmt, win, start)
	return tailSlot(slot, stmt)
}

// probeShell resolves a handle and reports why it cannot be used, so a `run`
// against a missing handle fails before it types anything.
func (s *Server) probeShell(handle string) *toolFailure {
	if _, bad := s.requireShell(handle); bad != nil {
		return failureFromResult(bad)
	}
	return nil
}

// directiveType writes bytes without pressing enter.
func (s *Server) directiveType(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	res, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", stmt.Target, "text", stmt.Payload))
	if fail != nil {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, fail, s))}
	}
	if held := reviewPendingFrom(res); held != nil {
		return []envelope.Slot{held.slot(stmt.Path)}
	}
	headers := map[string]string{"Path": stmt.Path}
	s.addUsage(&termReply{win: win}, stmt, headers)
	return []envelope.Slot{{Code: 204, Headers: headers}}
}

// directiveKey presses a named key.
func (s *Server) directiveKey(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if len(stmt.Args) == 0 {
		return []envelope.Slot{badRequest(stmt, "key needs a key name, e.g. `key a1f3:1 enter`")}
	}
	args := argsOf("shell_id", stmt.Target, "key", stmt.Args[0])
	repeat, err := atoiFlag(stmt, "repeat", 1)
	if err != nil {
		return []envelope.Slot{numericFlagProblem(stmt, "repeat", err)}
	}
	if repeat > 1 {
		args["repeat"] = float64(repeat)
	}
	res, fail := s.callTool(ctx, "shell_key", args)
	if fail != nil {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, fail)}
	}
	if held := reviewPendingFrom(res); held != nil {
		return []envelope.Slot{held.slot(stmt.Path)}
	}
	return []envelope.Slot{{Code: 204, Headers: map[string]string{"Path": stmt.Path}}}
}

// ptyQuiet is how long a pty's stream must stand still before a wait calls the
// command done.
//
// MEASURED, NOT GUESSED. One `echo` on a PowerShell with an oh-my-posh profile
// arrives as five chunks spread over 1.1s: the echo of the typed line at 120ms,
// a first repaint at 510ms, the command's own output at 940ms. The gaps between
// chunks reach 430ms, so a shorter grace ends the wait on the echo alone and the
// reply looks like a command that printed nothing. 500ms covers the widest gap
// this shell showed and still ends a fast command in about a quarter of the 2s
// window a caller typically passes.
const ptyQuiet = 500 * time.Millisecond

// ptyPoll is how often the wait asks the stream how long it is. The wait is a
// poll rather than a subscribe because the length is already tracked per channel
// and asking for it is cheaper than registering a second reader that would then
// swallow bytes meant for `read`.
const ptyPoll = 20 * time.Millisecond

// pipeQuiet is how long a pipe's stream must stand still before a wait on a
// resident process calls the command done.
//
// SHORTER THAN ptyQuiet ON PURPOSE, because the two streams are not the same
// thing. A pty interposes a line discipline and, on the shells that reach a
// caller here, a prompt that repaints itself in several chunks over a second —
// ptyQuiet is sized for those gaps. A pipe is a byte stream from a child this
// server started: a command's output lands as it is written, with no repaint and
// no echo of the typed line to survive, so the gap it must outlast is one write
// and not five.
//
// IT IS A CALIBRATION, NOT A LAW. Raise it if a command's output ever arrives
// split across two replies; `read` returns the rest either way, and that is what
// makes a 100ms guess affordable where 500ms would be a tax on every call.
const pipeQuiet = 100 * time.Millisecond

// parseSince reads the --since cursor.
func parseSince(raw string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--since takes a frame number (the number a previous reply reported as Latest), got %q", raw)
	}
	return n, nil
}

// fileGroupForOp maps a sub-action to the action group that dispatches it.
//
// The SFTP surface has two levels — an action that names a group (perm, link, fs)
// and an op inside it — because the handlers are grouped by what they touch. A verb
// should not expose that: `--op chmod` says everything, and which group carries it
// is an implementation detail. This is the one place that detail lives.
func fileGroupForOp(op string) (string, bool) {
	switch op {
	case "stat", "getwd", "urls":
		// Their own actions, not members of a group. `urls` belongs here: it hands back
		// the download/upload addresses for a path, which is the only way to deliver a
		// file to something outside termcp, and leaving it out of the table made
		// `get <session> file <path> --op urls` answer "does not know".
		return op, true
	case "chmod", "chown", "chtimes":
		return "perm", true
	case "readlink":
		// ONLY readlink. `symlink` and `link` used to be listed here, which meant
		// `get --op symlink` (a read verb, pretending) and `write --op symlink` (refused
		// for want of --target, a flag write does not have). Both are CREATE operations
		// and both live on `create … file`, where the node's key is the link and --target
		// is what it points at.
		return "link", true
	case "truncate", "realpath", "statvfs":
		return "fs", true
	}
	return "", false
}

// peerCallWindow chooses the deadline for a peer call.
//
// Defaults to 15s: short enough that a 30s host watchdog never kills the call
// from outside, long enough for any ordinary peer operation (a CDP evaluate, an
// MCP tool list).
//
// IT HAS TO ASK WHETHER THE CALLER SPOKE, not what the value came out as. `timeout` has a
// default of its own (2s, the read window), so `win.TimeoutSeconds` is never zero and the
// 15s branch below was unreachable from the term door: every peer call was bounded by 2
// seconds, which a cold tunnel through the internal SSH server can exceed — CI caught it
// as a 504 on an ordinary LSP frame. An explicit `timeout` still wins, including one
// shorter than the default; an explicit 0 means "do not wait", which is not a deadline a
// request/response call can use, so it falls back to the peer default.
const defaultPeerWindow = 15 * time.Second
