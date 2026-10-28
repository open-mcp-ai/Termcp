package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// toolGuard enforces a session's Restriction before a tool handler runs. It is
// the single choke point for every tool that acts on an existing session or
// shell: the wrapper in withLogging resolves the target from the call's own
// arguments, so the twenty-odd handlers do not each re-implement the check —
// and a tool added later is covered by construction instead of by memory.
//
// What it enforces comes from the call's capability row (internal/capability),
// not from a table in this package: the operation's id decides the whitelist
// entry, and its row decides which arguments carry a path. That is why there is
// no global path-argument list here any more — a capability declares its own.
//
// An ordinary session has an unrestricted (zero-value) Restriction and the guard
// never fires, so the cost for the common path is one map lookup per argument
// name. A restricted session is a strict subset of that surface.
type toolGuard struct {
	server *Server
}

// check returns a tool error result when the call must be refused, or nil when
// it may proceed. toolName is the registered tool name; g is its capability row
// (g.known is false for a name this build does not describe); request carries the
// arguments the guard resolves the target from.
//
// The target is resolved with the same set of spellings the handlers accept —
// a raw session id, a raw shell id, or a termcp:// locator, in either argument —
// because the guard and the handler MUST agree on which session a call names. A
// guard that resolved fewer spellings than the handler would be a whitelist that
// the caller can walk around by respelling the id, which is exactly what the
// first version of this file did (it handled a raw shell id only, while
// requireShell also accepts a session id and a locator).
//
// Only arguments that actually name the target are consulted, and a call that
// takes neither session_id nor shell_id passes through untouched — some tools
// (session_start, shell_detect, the extension tools themselves) legitimately
// operate outside any existing session, and gating them would have nothing to
// gate against.
//
// ctx carries the caller's namespace (see namespace.go). It is a second, coarser
// gate than the Restriction: a Restriction trims what a session may do, and this
// decides whether the caller may reach the session at all. Both live here rather
// than in the handlers so every tool that names a session is covered by
// construction, including one added later.
func (g toolGuard) check(ctx context.Context, toolName string, c gate, request mcpgo.CallToolRequest) *mcpgo.CallToolResult {
	args := request.GetArguments()
	if args == nil || g.server == nil {
		return nil
	}

	// BEFORE ANY CONFIGURATION IS READ. The access directory holds the signing key,
	// and the caller may be the very agent that authored the whitelist it would be
	// checked against — so the answer cannot be "ask the whitelist". See
	// protected.go for why this is unconditional and why it covers remote paths
	// too.
	if bad := g.refuseProtectedPath(c, args); bad != nil {
		return bad
	}

	callerNS := callerNamespace(ctx)

	// A shell-addressed tool resolves through its parent session, since the
	// restriction is a property of the container the shell was created under.
	//
	// Both arguments take a comma-separated batch, and the HANDLERS split it
	// themselves (session_terminate/session_delete for session_id, shell_input /
	// shell_key for shell_id). The guard therefore has to split it with the same
	// helper before it resolves anything: resolving the raw string "a,b" finds no
	// session, and "no session" is the one answer that means "nothing to
	// gate" — so the whole batch reached the handler unchecked and every entry in
	// it was acted on, including a session whose whitelist forbids the tool and a
	// session in another caller's namespace. A single id splits to itself, so the
	// ordinary case is the same loop of one.
	if shellID := getString(args, "shell_id", ""); shellID != "" {
		ids, _ := splitBatchIDs(shellID)
		return g.checkEach(ids, g.sessionForShellID, callerNS, toolName, c, args)
	}

	if sessionID := getString(args, "session_id", ""); sessionID != "" {
		ids, _ := splitBatchIDs(sessionID)
		return g.checkEach(ids, g.sessionForSessionID, callerNS, toolName, c, args)
	}

	// ONE TOOL NAMES WHAT IT ACTS ON BY THE RESOURCE'S OWN ID rather than by the
	// session it belongs to: forward(action=close) takes forward_id. That argument
	// is not one the branches above read, so the call resolved to no session and
	// reached the handler unchecked — the same hole the REST forward-close route
	// had, on the surface an extension handle actually reaches. The owner is
	// resolved from the same managers the handlers use.
	if id := getString(args, "forward_id", ""); id != "" {
		if sess := g.sessionForForwardID(id); sess != nil {
			if bad := refuseForeignNamespace(sess, callerNS); bad != nil {
				return bad
			}
			return g.checkSession(sess, toolName, c, args)
		}
	}

	return nil
}

// checkEach applies both gates — reachability, then the session's own whitelist —
// to every id a call names.
//
// A batch is refused WHOLESALE when any entry fails: running part of a batch
// against a session the caller may not touch would make the refusal partial and
// the outcome confusing, and a caller that asked for ten things would have to
// read ten results to learn which two were allowed.
//
// An id that resolves to nothing is skipped rather than refused: that is the
// handler's not-found error to report, with its own code and its own wording.
func (g toolGuard) checkEach(ids []string, resolve func(string) *session.Session, callerNS, toolName string, c gate, args map[string]any) *mcpgo.CallToolResult {
	for _, id := range ids {
		if id == "" {
			continue
		}
		sess := resolve(id)
		if sess == nil {
			continue
		}
		if bad := refuseForeignNamespace(sess, callerNS); bad != nil {
			return bad
		}
		if bad := g.checkSession(sess, toolName, c, args); bad != nil {
			return bad
		}
	}
	return nil
}

// refuseForeignNamespace is the isolation gate: a caller that claimed a namespace
// may only touch resources carrying it. It is a separate function from
// checkSession because the two answer different questions — "may this caller
// reach it" and "may this session do it" — and because the answer for a refusal
// must not depend on the session's whitelist, which is somebody else's business.
//
// The message names what the caller is, not what the resource is: an id it was
// handed once must not become a way to enumerate other namespaces by reading
// error text.
func refuseForeignNamespace(sess *session.Session, callerNS string) *mcpgo.CallToolResult {
	if sess == nil || ns.Reachable(sess.Namespace, callerNS) {
		return nil
	}
	return nsRefusal(callerNS)
}

// sessionForForwardID resolves the session a forward belongs to: a forward is
// keyed by the session that opened it, and that session is what the whitelist
// governs. An id the manager does not know resolves to nothing and is left to
// the handler's own not-found error.
func (g toolGuard) sessionForForwardID(id string) *session.Session {
	if g.server.forwardMgr == nil {
		return nil
	}
	for _, fw := range g.server.forwardMgr.List() {
		if fw.ForwardID == id {
			return g.server.sessMgr.Get(fw.SessionID)
		}
	}
	return nil
}

// sessionForShellID resolves a shell_id argument the way requireShell does: a
// live or retained shell id, a raw session id (its primary channel), a
// termcp:// locator naming either, or the short `sid:N` handle `ps` prints. It
// deliberately does not apply requireShell's status check — a closed session's
// retained shell is still the container whose whitelist governs a read of its
// output, and letting the guard skip it would leave that path ungated.
//
// Recognising every spelling requireShell recognises is a SECURITY requirement,
// not a convenience: checkEach SKIPS an id that resolves to nothing (that is the
// handler's not-found error to report), so any spelling the handler accepts but
// this function does not is a hole straight through the whitelist.
func (g toolGuard) sessionForShellID(shellID string) *session.Session {
	if cs := g.server.sessMgr.GetChildShell(shellID); cs != nil {
		// THE CHANNEL IS THE PRECISE ANSWER, so it is the one renewed: two instances
		// of one extension share a session, and renewing by session would tell the
		// daemon that both are in use when only one is.
		if g.server.extensions != nil {
			g.server.extensions.Touch(shellID, cs.ParentSessionID())
		}
		return g.server.sessMgr.Get(cs.ParentSessionID())
	}
	// GetSessionByShellID also searches retained DEAD/restored snapshots, which
	// is the read-only case above; a live shell is found by it too, so this is
	// the one scan the guard needs.
	if sess := g.server.sessMgr.GetSessionByShellID(shellID); sess != nil {
		return sess
	}
	return g.sessionForSessionID(shellID)
}

// sessionForSessionID resolves a session_id argument the way requireSession
// does: a raw session id, a shell id belonging to it, a termcp:// locator, or the
// short `sid:N` handle. The same security note applies: a spelling the resolver
// misses is a spelling the guard does not gate.
func (g toolGuard) sessionForSessionID(id string) *session.Session {
	sess := g.lookupSessionID(id)
	if sess != nil {
		// THE DAEMON'S ONLY "STILL IN USE" SIGNAL. Resolving a session by id is
		// what every tool call that drives one does first, so it is the one place
		// that learns somebody is still using an on-demand extension — and without
		// it the idle clock would measure "nobody called open recently" instead of
		// "nobody is driving it", reaping a channel in the middle of a long read
		// (issue #183).
		if g.server.extensions != nil {
			g.server.extensions.Touch("", sess.ID)
		}
	}
	return sess
}

func (g toolGuard) lookupSessionID(id string) *session.Session {
	if sess := g.server.sessMgr.Get(id); sess != nil {
		return sess
	}
	if sess := g.server.sessMgr.GetByShellID(id); sess != nil {
		return sess
	}
	if looksLikeResourceLocator(id) {
		p, err := parseResourceURL(id)
		if err == nil && p.Kind != resourceURLEntry {
			return g.server.sessMgr.Get(p.SessionID)
		}
		return nil
	}
	// The short handle from ps. Without this, `shell_id: "sid:2"` resolved here to
	// nothing, checkEach skipped it as "no session to gate", and the call went on
	// to a handler that DOES understand the spelling — the whitelist bypassed by
	// punctuation.
	if h := locator.SplitHandle(id); h.Index > 0 {
		return g.server.sessMgr.Get(h.SessionID)
	}
	return nil
}

// checkSession applies one session's whitelist to the call: the capability
// itself, then every path argument that capability declares.
//
// The namespace check is separate and comes first: a caller that may not reach
// the session at all is not told what the session's whitelist says.
func (g toolGuard) checkSession(sess *session.Session, toolName string, c gate, args map[string]any) *mcpgo.CallToolResult {
	if !c.known {
		// An operation with no capability row has no entry a whitelist could
		// name, so a restricted session cannot reach it.
		return refuseSession(sess, "\x00unknown", toolName)
	}
	if bad := refuseSession(sess, c.ID, toolName); bad != nil {
		return bad
	}
	r := sess.Restriction()
	// Path arguments are declared per capability; reading is as gated as writing,
	// because the whitelist is about reachability, not direction.
	for _, key := range c.PathArgs {
		path := getString(args, key, "")
		if path == "" {
			continue
		}
		if !r.AllowsPath(path) {
			return toolError(CodeForbidden, "%s", session.ForbiddenPath(key, path))
		}
	}
	return nil
}
