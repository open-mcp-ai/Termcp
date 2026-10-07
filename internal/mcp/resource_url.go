package mcp

import (
	"fmt"

	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Resource URL (termcp://...) support. Parsing lives in internal/locator, which
// the HTTP API reuses for GET /api/resolve; this file keeps the MCP package's
// historic names as aliases and adds the session/shell resolution helpers.

// ResourceURLScheme is the URL scheme prefix of every termcp resource locator.
const ResourceURLScheme = locator.Scheme

type parsedResourceURL = locator.Parsed
type resourceURLKind = locator.Kind

const (
	resourceURLEntry   = locator.KindEntry
	resourceURLSession = locator.KindSession
	resourceURLShell   = locator.KindShell
)

// parseResourceURL parses a termcp:// resource locator (see internal/locator).
func parseResourceURL(raw string) (*parsedResourceURL, error) { return locator.Parse(raw) }

// looksLikeResourceLocator reports whether s is written in termcp resource-URL
// syntax rather than as a bare id.
func looksLikeResourceLocator(s string) bool { return locator.LooksLike(s) }

// shellFromIndex resolves a live channel by its channel index on a session (0 =
// the primary shell) — the number termcp://#<session>:N and the Web UI's shell-N
// tab label carry, assigned when the channel was created and never renumbered.
// The index is resolved through session.ShellByIndex, so MCP, the REST resolver
// and the Web UI cannot disagree about which channel N names.
//
// Read-only paths that must also serve DEAD/restart-restored sessions resolve
// through Session.SnapshotShellByIndex (see resolveOutputSource and the REST
// /api/resolve handler) — this function is the live half.
func (s *Server) shellFromIndex(sess *session.Session, index int) (*session.ChildShell, error) {
	if cs, ok := sess.ShellByIndex(index); ok {
		return cs, nil
	}
	if index <= 0 {
		// No primary shell left: callers report the plain "has no shell" error.
		return nil, nil
	}
	return nil, session.ShellIndexOutOfRangeError(sess.ID, index, sess.LiveShellCount())
}

// sessionFromParsed resolves the session named by a parsed resource URL.
// Only live sessions resolve: closed (DEAD) sessions have no transport, so
// they are not addressable by locator — callers get a hint to read output
// via shell_output instead.
//
// This is deliberately NOT the same helper as requireSession, which serves the id
// arguments of tools that must work on a DEAD session too (session_info,
// session_terminate, session_delete, shell_output). The difference is the status
// check, not the parsing: use requireSession for an argument a caller may point at
// any registered session, and this one only where the operation needs a live
// transport.
func (s *Server) sessionFromParsed(p *parsedResourceURL) (*session.Session, error) {
	if p.Kind != resourceURLSession && p.Kind != resourceURLShell {
		return nil, fmt.Errorf("resource URL does not name a session")
	}
	if sess := s.sessMgr.Get(p.SessionID); sess != nil {
		if sess.Info().Status != api.SessionRunning {
			return nil, fmt.Errorf("session %q is closed (status %s); its output is read-only via shell_output", p.SessionID, sess.Info().Status)
		}
		return sess, nil
	}
	return nil, fmt.Errorf("session %q not found", p.SessionID)
}
