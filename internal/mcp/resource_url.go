package mcp

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// Resource URL (termcp://...) parsing and resolution.
//
// A resource URL names a termcp object without requiring a separate lookup
// round-trip. Accepted forms (all via the ResourceURLScheme constant):
//
//	termcp://<entry>              – ssh_config profile name ("internal" = loopback)
//	termcp://#[session]           – a session (short form, always preferred)
//	termcp://#[session]:[index]   – a shell channel of that session:
//	                                :1 = first shell (the primary), :N = Nth by creation order
//	termcp://<entry>#[session]    – accepted for back-compat; entry is IGNORED
//	                                (session ids are unique, entry prefixes are not reliable)
//
// Shell channel index is 1-based creation order (matches the Web UI's
// shell-1/shell-2 tab labels), NOT the raw id. Only the short form (no entry)
// is emitted by the UI copy buttons.

// resourceURLKind enumerates what a parsed resource URL names.
type resourceURLKind int

const (
	resourceURLEntry resourceURLKind = iota
	resourceURLSession
	resourceURLShell
)

// parsedResourceURL is the result of parsing a termcp:// locator.
type parsedResourceURL struct {
	kind  resourceURLKind
	entry string // non-empty only for resourceURLEntry
	sid   string // session id (no "session-" prefix)
	index int    // shell index, 1-based; 0 = unset (session-kind)
}

// parseResourceURL parses a possibly-scheme-qualified termcp resource locator.
// Accepted forms (all via the ResourceURLScheme constant):
//
//	termcp://<entry>              – ssh_config profile name ("internal" = loopback)
//	termcp://#[session]           – a session (short form, always preferred)
//	termcp://#[session]:[index]   – a shell channel (1-based creation order)
//	termcp://<entry>#[session]    – back-compat; entry is IGNORED
//
// Bare ids ("abc123"), "session-abc123", and "#abc123" are accepted as the
// short session form; a trailing ":N" selects a shell channel.
func parseResourceURL(raw string) (*parsedResourceURL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("empty resource URL")
	}
	if i := strings.Index(s, ResourceURLScheme); i >= 0 {
		s = s[i+len(ResourceURLScheme):]
	}
	s = strings.TrimPrefix(s, "//")
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return nil, fmt.Errorf("malformed resource URL %q", raw)
	}

	if strings.HasPrefix(s, "shells/") {
		// Notification broadcast URI (termcp://shells/<shell_id>) is NOT a
		// locator for user operations; reject it with a clear error.
		return nil, fmt.Errorf("termcp://shells/<id> is a notification broadcast URI, not a resource URL")
	}

	// Split an optional trailing :<index> (shell channel) off the session
	// part. Only the part after the LAST '#' can carry an index.
	index := 0
	if hashIdx := strings.Index(s, "#"); hashIdx >= 0 {
		tail := s[hashIdx+1:]
		if i := strings.LastIndex(tail, ":"); i >= 0 {
			n, err := strconv.Atoi(tail[i+1:])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid shell index in resource URL %q", raw)
			}
			index = n
			s = s[:hashIdx+1+i]
		}
	}

	// Short form: termcp://#[session][:N] (or a bare "#session").
	if strings.HasPrefix(s, "#") {
		sid := strings.TrimPrefix(s, "#")
		sid = strings.TrimPrefix(sid, "/")
		sid = strings.TrimPrefix(sid, "session-")
		if sid == "" {
			return nil, fmt.Errorf("missing session id in resource URL %q", raw)
		}
		if index > 0 {
			return &parsedResourceURL{kind: resourceURLShell, sid: sid, index: index}, nil
		}
		return &parsedResourceURL{kind: resourceURLSession, sid: sid}, nil
	}

	// Back-compat entry form: termcp://<entry>#[session][:N].
	// The entry prefix was found unreliable (names are user-assigned), so we
	// accept the form but IGNORE the entry; the session id is authoritative.
	if hashIdx := strings.Index(s, "#"); hashIdx >= 0 {
		sid := strings.TrimPrefix(s[hashIdx+1:], "session-")
		if sid == "" {
			return nil, fmt.Errorf("missing session id in resource URL %q", raw)
		}
		if index > 0 {
			return &parsedResourceURL{kind: resourceURLShell, sid: sid, index: index}, nil
		}
		return &parsedResourceURL{kind: resourceURLSession, sid: sid}, nil
	}

	// Pure entry name: termcp://mac (no '#' anywhere).
	if strings.ContainsAny(s, ":") {
		return nil, fmt.Errorf("malformed resource URL %q", raw)
	}
	return &parsedResourceURL{kind: resourceURLEntry, entry: s}, nil
}

// shellFromIndex resolves a 1-based creation order index on a session. If no
// index was given (0), the primary (first) shell is returned. Uses the API
// view piped through ListChildShells so ordering always matches the Web UI.
func (s *Server) shellFromIndex(sess *session.Session, index int) (*session.ChildShell, error) {
	if index <= 0 {
		return sess.PrimaryShell(), nil
	}
	all := sess.ListChildShells()
	if idx := index - 1; idx < len(all) {
		return sess.GetChildShell(all[idx].ID), nil
	}
	return nil, fmt.Errorf("shell index %d out of range (session %q has %d shell(s))", index, sess.ID, len(all))
}

// sessionFromParsed resolves the session named by a parsed resource URL.
// Live sessions take precedence; restored DEAD / archived sessions fall back
// to the history manager so read-only locators keep working.
func (s *Server) sessionFromParsed(p *parsedResourceURL) (*session.Session, error) {
	if p.kind != resourceURLSession && p.kind != resourceURLShell {
		return nil, fmt.Errorf("resource URL does not name a session")
	}
	if sess := s.sessMgr.Get(p.sid); sess != nil {
		return sess, nil
	}
	if s.historyMgr != nil {
		if _, ok := s.historyMgr.Get(p.sid); ok {
			return nil, fmt.Errorf("session %q is archived; read it via shell_output(offset/tail)", p.sid)
		}
	}
	return nil, fmt.Errorf("session %q not found", p.sid)
}

// looksLikeResourceLocator reports whether s is written in termcp resource-URL
// syntax (scheme or "#" short form, or a "session-" prefixed id) rather than as
// a bare id. Bare ids skip the parser so an unknown id still gets the plain
// "shell not found" message instead of parser diagnostics.
func looksLikeResourceLocator(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, ResourceURLScheme) ||
		strings.HasPrefix(s, "#") ||
		strings.HasPrefix(s, "session-")
}
