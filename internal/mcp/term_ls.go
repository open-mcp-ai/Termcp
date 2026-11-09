package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
	"github.com/open-mcp-ai/termcp/term/locator"
)

// directiveLS is the one listing verb, for every collection in the tree.
//
// IT SHEDS THE KIND BEFORE LISTING. `ls host` and `ls ext` name a collection, not a
// node: the parser put the word in Kind because that is where the resource tree says
// it goes, and this translates it into the action the collection's handler expects.
func (s *Server) directiveLS(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	switch stmt.Kind {
	case "task":
		// TASKS ARE NOT A SESSION'S COLLECTION. A transfer belongs to the process, not
		// to a session handle, so `ls task` takes no target — the same shape as
		// `ls host`.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls task` lists every transfer; for one of them use `get task "+
				stmt.Key+"` (pass a `timeout` on the call and it waits).")}
		}
		return []envelope.Slot{s.directiveLsTask(stmt)}
	case "host":
		// A NAME HERE WOULD BE SILENTLY DROPPED: the handler lists every entry, so
		// `ls host prod` answered the whole table and the caller believed it had been
		// filtered. One entry is what `get host` is for.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls` lists every host; for one entry use `get host "+
				stmt.Key+"`.")}
		}
		return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{"action": "list"}), win)}
	case "ext":
		// A NAME HERE WOULD BE SILENTLY DROPPED, exactly as it was on `ls host` and
		// `ls <session> forward`. `ls ext evrything` answered the whole list, so a
		// caller checking that an extension exists read fourteen rows as confirmation.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls` lists every extension; for one of them use `get ext "+
				stmt.Key+"`.")}
		}
		return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{"action": "list"}), win)}
	case "alias":
		return []envelope.Slot{s.directiveAlias(ctx, stmt, win)}
	case "forward":
		// A KEY HERE WOULD BE SILENTLY DROPPED. The handler lists every forward on the
		// session, so `ls <session> forward fw-1` would answer a full listing and the
		// caller would believe the key had scoped it. Saying so is cheaper than letting
		// them read a list that looks filtered and is not.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls` lists the whole collection; it does not take a "+
				"forward id. Use `ls "+stmt.Target+" forward` for all of them.")}
		}
		return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, map[string]string{"action": "list"}), win)}
	case "file":
		// THERE IS NO DIRECTORY LISTING, and this says so instead of guessing.
		//
		// `file --action find` was added and then reverted; the word `ls <session>
		// file <path>` survived in the usage line and here, wrapped around the fs group,
		// whose only members are truncate/realpath/statvfs. A caller asking to list a
		// directory got "file --action fs needs the sub-action", which names a layer
		// they never mentioned and says nothing about the capability being absent.
		//
		// The kind stays DECLARED so it can be refused by name: dropping it would make
		// `file` a stray positional argument and the command would silently list the
		// session instead — the same fall-through that closed a session from `close`.
		return []envelope.Slot{badRequest(stmt, "there is no directory listing here. `ls` enumerates "+
			"nodes termcp knows (sessions, channels, forwards, hosts, extensions), not files: a directory "+
			"is the target's own business, so ask the shell — `run "+stmt.Target+":1 $1` with args [\"ls -la "+
			stmt.Key+"\"]. For one path's metadata use `get "+stmt.Target+" file "+stmt.Key+"`.")}
	}
	if stmt.Target == "" {
		// Bare `ls`: every live link and every channel on it.
		return []envelope.Slot{s.directivePS(ctx, withFlags(stmt, map[string]string{"shells": "true"}), win)}
	}
	// `ls <peer>` ASKS THE PEER what hangs off it. For a browser those are tabs; the
	// question is the same one `ls <session>` asks about channels, so it is the same
	// verb — and it is answered by the peer rather than by the SSH side, which knows
	// nothing about tabs.
	if isPeerKind(stmt.Kind) {
		// `ls <session> cdp` NAMES THE CONNECTIONS; `ls <session> cdp:1` names the tabs
		// inside one. That is the tree rule — a listing shows a node's children — and it
		// is also the only way to discover that a second browser exists.
		if stmt.Key == "" {
			return []envelope.Slot{s.listPeers(stmt)}
		}
		h, _, bad := s.peerOf(stmt)
		if bad != nil {
			return []envelope.Slot{*bad}
		}
		return []envelope.Slot{s.cdpList(ctx, stmt, h)}
	}
	// `ls <session>`: that link's channels. directivePS lists everything, so the
	// statement is narrowed by pointing the handler at one session through --all and
	// filtering in psOne; see directivePS for the filter.
	//
	// A CHANNEL ADDRESS IS NOT A COLLECTION, AND IT USED TO BE ANSWERED AS ONE
	// (issue #148). The parser folds `ls s1 5` into the address `s1:5`, so the index
	// arrives on the target rather than in Key. Dropped here, `ls s1 5` answered the
	// whole listing — or, on a session with no live channels, the global "(nothing is
	// running)", which is a sentence about a different collection. A caller using a
	// stale index to ask "is my channel up" read that as "the session is dead".
	if idx := locator.SplitHandle(stmt.Target).Index; idx > 0 {
		sid := locator.SplitHandle(stmt.Target).SessionID
		return []envelope.Slot{badRequest(stmt, "`ls "+sid+"` lists every channel; for one of them "+
			"use `get "+sid+":"+strconv.Itoa(idx)+"`.")}
	}
	// A TARGET NAMES A COLLECTION, SO A TARGET THAT NAMES NOTHING MUST SAY SO.
	//
	// `ls nosuchthing` (and `ls <a session that was rm'd>`) fell through to the same
	// filtered listing a real session gets, the filter matched nothing, and the reply was
	// the GLOBAL sentence "(nothing is running)" plus the hint about `ls host`. That
	// sentence is about a different collection and answers a different question: a caller
	// asking "what is on this link" was told the machine has no sessions at all. Same
	// shape as #148 one branch up — a stale or mistyped target read as a fact about the
	// world (found by dogfooding).
	//
	// THE PREDICATE IS THE LISTING'S OWN. `ls <target>` narrows to a session by id OR by
	// profile name — `ls internal` is how a caller lists the session on the internal
	// profile — so a check that only knew about ids refused a spelling that had been
	// working, and the browser test caught it. A target that names neither is the one
	// case where the empty listing is a lie about the collection.
	if !s.lsTargetNames(stmt.Target) {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, &toolFailure{
			code: CodeSessionNotFound,
			msg: fmt.Sprintf("no session %q, so there is nothing to list for it. `ls` lists every live "+
				"link, `ls --all` includes the ones that have exited.", stmt.Target),
		})}
	}
	return []envelope.Slot{s.directivePS(ctx, withFlags(stmt, map[string]string{
		"shells": "true", "only-session": stmt.Target, "all": "true",
	}), win)}
}

// lsTargetNames reports whether a target names a session the listing can show: its id,
// or the profile it was opened on.
//
// Both spellings are what directivePS's own filter accepts, and keeping the two in step
// is the point — a check that answered a narrower question than the filter would refuse
// targets the filter handles.
func (s *Server) lsTargetNames(target string) bool {
	for _, sess := range s.sessMgr.ListAll() {
		if sess.ID == target || sess.SSHConfig == target {
			return true
		}
	}
	return false
}

// wantPTY decides whether a new channel gets a terminal.
//
// THE RULE IS SSH'S OWN, which is why it needs no explaining: `ssh host` gives you a
// terminal, `ssh host cmd` runs cmd with plain pipes, and `ssh -t host cmd` forces a
// terminal back on. So an --argv means pipe unless --pty asks otherwise, and no
// --argv means pty unless --pipe asks otherwise.
//
// It matters beyond bookkeeping. A pty has no "the command finished" signal, so an
// exit code is unreadable on one; a caller that asked for a single command and got a
// terminal would read an empty reply as "no output" instead of "it is still running".
func wantPTY(stmt directive.Statement) bool {
	if boolFlag(stmt, "pipe") {
		return false
	}
	if boolFlag(stmt, "pty") {
		return true
	}
	argv, _ := stmt.Flag("argv")
	return argv == ""
}

// aliasStatement rebuilds the payload the alias handler reads.
//
// The handler takes "<name> <expansion>" verbatim, so the expansion is never
// re-quoted: what the caller wrote is what the alias runs. The parser splits that
// into a key and positional words — `create alias ll ls a1f3` — and this puts them
// back together, with --off for a removal, so the handler keeps its one contract.
func aliasStatement(stmt directive.Statement, off bool) directive.Statement {
	out := stmt
	parts := append([]string{stmt.Key}, stmt.Args...)
	body := strings.Join(parts, " ")
	if off {
		body = stmt.Key + " --off"
	}
	out.Payload = body
	return out
}

// listPeers renders every connection of one kind under a session.
//
// IT IS A LISTING, NOT A RESOLUTION: with two browsers open, `ls internal cdp` must
// still answer — that is exactly the moment a caller needs the addresses. The verbs
// that act on one peer refuse an unqualified address in that case; this one hands
// them the names to qualify with.
func (s *Server) listPeers(stmt directive.Statement) envelope.Slot {
	sess, bad := s.resolveTunnelSession(stmt.Target)
	if bad != nil {
		return envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": CodeSessionNotRunning,
		}, Body: "no running session named " + stmt.Target + ", so it holds no " + stmt.Kind + " connections."}
	}
	addrs := liveAddrs(sess.ID, stmt.Kind)
	handles := make([]*jrpcHandle, 0, len(addrs))
	for _, addr := range addrs {
		if v, ok := jrpcChannels.Load(addr); ok {
			if h, _ := v.(*jrpcHandle); h != nil {
				handles = append(handles, h)
			}
		}
	}

	var b strings.Builder
	if len(handles) == 0 {
		hint := map[string]string{
			"cdp":  "a browser's devtools endpoint, http://host:port.",
			"mcp":  "an MCP endpoint, http(s):// or ws(s)://.",
			"jrpc": "any JSON-RPC endpoint over http(s):// or ws(s)://.",
		}[stmt.Kind]
		b.WriteString("no " + stmt.Kind + " connections on this session.\n\n")
		b.WriteString("Next: `open " + stmt.Target + " " + stmt.Kind + " <url>` — " + hint + "\n")
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
	}
	b.WriteString(fmt.Sprint(len(handles)) + " " + stmt.Kind + " connection(s):\n")
	for _, h := range handles {
		b.WriteString("  " + peerAddress(h.sessionID, h.kind, h.index) + "\t" +
			h.ch.Transport().String() + "\t" + h.ch.URL())
		if h.cdp != nil {
			b.WriteString(fmt.Sprintf("\t%d target(s) attached", len(h.cdp.snapshot())))
		}
		b.WriteString("\n")
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// --- task: the receipt side of an asynchronous transfer (#158) ---
