package webui

import (
	"net/http"
	"slices"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// The REST half of a session's Restriction.
//
// The whitelist was enforced on the MCP tool surface only, which is half a
// boundary: the extension layer's whole promise is that "a call the template did
// not whitelist is genuinely refused", and every capability its presets name —
// terminal I/O, reading files, opening a channel — exists on the HTTP surface
// too, on the proof that this is "the operator's own interface". That reasoning
// answers whether review mode should gate the path; it does not answer whether a
// template's author granted it. An agent handed an extension handle can reach
// 127.0.0.1:$PORT/api/shells/{id}/input with the same deployment token, so the
// template's whitelist has to be enforced here as well, with the same names and
// the same refusal text.
//
// Why this is still a whitelist check and nothing else: review mode never gates
// the operator's own REST calls (capability.GateOperator / GateDevice), so the
// only admission question left on this surface is whether the template granted
// the capability. That answer lives in internal/session (Allows); this file just
// renders it.
//
// These are deliberately thin: the decisions live in internal/session and
// internal/capability, so the surfaces cannot drift into disagreeing about what
// a template permits.

// restRefuseTool answers the request when sess's whitelist does not carry the
// capability capID, and reports whether it did. A nil session (or an
// unrestricted one) passes. A scoped token's own tools whitelist (auth
// principal) applies here too: the route's capability must be in it.
//
// capID is a capability id (capability.FileWrite, …), not a tool name: the
// template's allow list and this check speak the same vocabulary, and the
// refusal is rendered from the capability table rather than from a string a
// handler happened to spell.
func restRefuseTool(w http.ResponseWriter, r *http.Request, sess *session.Session, capID string) bool {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && p != nil && len(p.Tools) > 0 {
		if !slices.Contains(p.Tools, capID) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error_code": "forbidden",
				"error":      "this token's scope does not include " + capID,
			})
			return true
		}
	}
	if sess == nil || sess.Restriction().Allows(capID) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error_code": "forbidden",
		"error":      session.ForbiddenTool(capability.ToolForID(capID)),
	})
	return true
}

// restRefuseToolAny is restRefuseTool for a route that maps to more than one
// capability (PUT creates a new profile and edits an existing one): each gate
// — the principal's tools whitelist, then the session's template — passes when
// it allows ANY of the capabilities.
func restRefuseToolAny(w http.ResponseWriter, r *http.Request, sess *session.Session, capIDs ...string) bool {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && p != nil && len(p.Tools) > 0 {
		allowed := false
		for _, id := range capIDs {
			if slices.Contains(p.Tools, id) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error_code": "forbidden",
				"error":      "this token's scope does not include any of " + strings.Join(capIDs, ", "),
			})
			return true
		}
	}
	if sess != nil {
		allowed := false
		for _, id := range capIDs {
			if sess.Restriction().Allows(id) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error_code": "forbidden",
				"error":      session.ForbiddenTool(capability.ToolForID(capIDs[0])),
			})
			return true
		}
	}
	return false
}

// restRefuseSessionIDs answers a comma-separated session id list when ANY entry
// is restricted, and reports whether it did. The list is resolved the way the
// MCP guard resolves one — a raw id, or a termcp:// locator — so a caller cannot
// walk around the refusal by spelling the id differently. Ids that resolve to
// nothing are left to the handler's own not-found handling.
func (h *Handler) restRefuseSessionIDs(w http.ResponseWriter, r *http.Request, raw, capID string) bool {
	for _, id := range strings.Split(raw, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		sess := h.Sessions.Get(id)
		if sess == nil {
			if p, err := locator.Parse(id); err == nil && p.Kind != locator.KindEntry {
				sess = h.Sessions.Get(p.SessionID)
			}
		}
		if restRefuseTool(w, r, sess, capID) {
			return true
		}
	}
	return false
}

// sessionOwning resolves the session that owns a resource, for the routes that
// name the resource rather than the session: DELETE /api/forwards/{id},
// DELETE /api/notifications/{id}, DELETE /api/shells/{id} and the close-shell
// compatibility route. Without it those would be the only paths to a capability
// that skip the whitelist, since none carries a session id to check.
//
// Both ids are consulted because a resource records the session that created it
// and, for a rule, the shell it targets. The session is the container the
// whitelist belongs to; the shell is the fallback for an entry that predates the
// session field or was cleaned up. GetSessionByShellID is the lookup, not
// GetByShellID: the former also finds a retained DEAD/restored snapshot, so a
// closed session's resource is still resolved to its owner instead of reading as
// unowned and passing the gate. A nil Sessions manager yields nil, which
// restRefuseTool reads as unrestricted — the same thing an unfindable id means.
func (h *Handler) sessionOwning(sessionID, shellID string) *session.Session {
	if h.Sessions == nil {
		return nil
	}
	if sess := h.Sessions.Get(sessionID); sess != nil {
		return sess
	}
	if sess := h.Sessions.GetSessionByShellID(shellID); sess != nil {
		return sess
	}
	return nil
}

// restRefusePaths answers the request when any path named in paths is outside
// sess's whitelist, and reports whether it did. Each entry is {argumentName,
// path}; an empty path is skipped, so a caller may pass a tool's whole argument
// set and let the empty ones fall out. As on the MCP side, reading is as gated as
// writing: the whitelist is about reachability, not direction.
func restRefusePaths(w http.ResponseWriter, sess *session.Session, paths ...[2]string) bool {
	if sess == nil {
		return false
	}
	r := sess.Restriction()
	for _, p := range paths {
		if p[1] == "" {
			continue
		}
		if !r.AllowsPath(p[1]) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error_code": "forbidden",
				"error":      session.ForbiddenPath(p[0], p[1]),
			})
			return true
		}
	}
	return false
}
