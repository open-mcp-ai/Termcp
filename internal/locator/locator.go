// Package locator parses termcp:// resource locators.
//
// A locator names a termcp object without a lookup round-trip, so users can
// copy one from the Web UI and paste it into a chat or a script. Both surfaces
// resolve them: MCP tools accept locators wherever an id is expected, and the
// HTTP API exposes GET /api/resolve.
//
// Accepted forms (all via the Scheme constant):
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
//
// Locators name live sessions. A closed (DEAD) session is deliberately NOT
// resolvable: it has no transport left, so its channels are read-only and are
// addressed by the session-level output-range endpoint instead.
package locator

import (
	"fmt"
	"strconv"
	"strings"
)

// Scheme is the URL scheme prefix of every termcp resource locator.
const Scheme = "termcp://"

// Kind enumerates what a parsed resource URL names.
type Kind int

const (
	KindEntry Kind = iota
	KindSession
	KindShell
)

// Parsed is the result of parsing a termcp:// locator.
type Parsed struct {
	Kind      Kind
	Entry     string // non-empty only for KindEntry
	SessionID string // session id (no "session-" prefix)
	Index     int    // shell index, 1-based; 0 = unset (session-kind)
}

// Parse parses a possibly-scheme-qualified termcp resource locator. Bare ids
// ("abc123"), "session-abc123", and "#abc123" are accepted as the short session
// form; a trailing ":N" selects a shell channel.
func Parse(raw string) (*Parsed, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("empty resource URL")
	}
	if i := strings.Index(s, Scheme); i >= 0 {
		s = s[i+len(Scheme):]
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
			return &Parsed{Kind: KindShell, SessionID: sid, Index: index}, nil
		}
		return &Parsed{Kind: KindSession, SessionID: sid}, nil
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
			return &Parsed{Kind: KindShell, SessionID: sid, Index: index}, nil
		}
		return &Parsed{Kind: KindSession, SessionID: sid}, nil
	}

	// Pure entry name: termcp://mac (no '#' anywhere).
	if strings.ContainsAny(s, ":") {
		return nil, fmt.Errorf("malformed resource URL %q", raw)
	}
	return &Parsed{Kind: KindEntry, Entry: s}, nil
}

// LooksLike reports whether s is written in termcp resource-URL syntax (scheme
// or "#" short form, or a "session-" prefixed id) rather than as a bare id.
// Bare ids skip the parser so an unknown id still gets the plain "not found"
// message instead of parser diagnostics.
func LooksLike(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, Scheme) ||
		strings.HasPrefix(s, "#") ||
		strings.HasPrefix(s, "session-")
}
