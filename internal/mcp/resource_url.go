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

// shellFromIndex resolves a 1-based creation-order channel on a session (0 =
// the primary shell) — the same numbering the Web UI and termcp:// locators use.
func (s *Server) shellFromIndex(sess *session.Session, index int) (*session.ChildShell, error) {
	if cs, ok := sess.ShellByIndex(index); ok {
		return cs, nil
	}
	if index <= 0 {
		// No primary shell left: callers report the plain "has no shell" error.
		return nil, nil
	}
	return nil, fmt.Errorf("shell index %d out of range (session %q has %d shell(s))", index, sess.ID, len(sess.ListChildShells()))
}

// sessionFromParsed resolves the session named by a parsed resource URL.
// Live sessions take precedence; restored DEAD / archived sessions fall back
// to the history manager so read-only locators keep working.
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
