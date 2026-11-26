package mcp

// One way to name a session (issue #320).
//
// A SESSION HAS THREE SPELLINGS A PERSON ACTUALLY USES, and they were accepted in
// different places and refused in others:
//
//	6f2a1c0d9e11        the full id — always worked
//	6f2a                the short prefix `ps` prints in a narrow column, and the one a
//	                    reader copies out of a log line
//	web                 the --name it was created with
//
// The failure mode was not "not implemented" but "works here, 404 there": `read web:1`
// resolved the name, `run web:1` answered `session "web" not found`, and `ls web` said
// there was no such session. A caller cannot tell a spelling that is refused from a
// session that is gone, so it retries or gives up on the wrong one.
//
// THE RULE, IN ONE PLACE: an exact id wins, then the display name, then a prefix that
// matches exactly one session. Anything matching several is a refusal that LISTS the
// candidates — choosing for the caller is how a `close` lands on the wrong session.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// minPrefixLen is the shortest id prefix that resolves. Four hex characters name 17 bits
// of the id: long enough that a human typing what they see in a log line means one
// session, short enough that the column `ps` prints is usable. Anything shorter is
// treated as a name, so a two-letter name is not eaten as a prefix.
const minPrefixLen = 4

// resolveSessionRef turns whatever the caller wrote into the session it names.
//
//   - sess is the session, or nil when nothing matched
//   - why explains a refusal (ambiguity), and is empty otherwise
//
// No match is (nil, nil) rather than an error: each caller has its own sentence for
// "not found" and knows what the caller was trying to do.
func (s *Server) resolveSessionRef(ref string) (*session.Session, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	if sess := s.sessMgr.Get(ref); sess != nil {
		return sess, nil
	}
	// BY NAME BEFORE BY PREFIX: a name is a word the caller chose, and `web` should not
	// lose to an id that happens to start with the same four characters.
	if byName, why := s.sessionNamedOne(ref); why != "" {
		return nil, fmt.Errorf("%s", why)
	} else if byName != nil {
		return byName, nil
	}
	if len(ref) < minPrefixLen || len(ref) >= 12 {
		return nil, nil
	}
	var matches []api.Session
	for _, info := range s.sessMgr.ListAll() {
		if strings.HasPrefix(info.ID, ref) {
			matches = append(matches, info)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return s.sessMgr.Get(matches[0].ID), nil
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.ID)
	}
	return nil, fmt.Errorf("%d sessions start with %q, so the prefix does not say which one: %s. "+
		"Use more characters, or the full id", len(matches), ref, strings.Join(ids, ", "))
}
