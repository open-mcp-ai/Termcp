package mcp

import (
	"context"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// Batch close takes the IDs printed by ls, not names: a bare name might
// resolve to a channel's parent. Validate the shape of the entire list before
// dispatch, but report a missing object as an outcome for just that entry.
func (s *Server) closeBatch(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if len(stmt.Flags) != 0 {
		return []envelope.Slot{badRequest(stmt, "batch close takes no flags; use close <session-id>,<session-id>:<index>")}
	}
	targets := strings.Split(stmt.Target, ",")
	for _, target := range targets {
		id, index, channel := strings.Cut(target, ":")
		if id == "" || strings.ContainsAny(id, " \t\r\n/#@") {
			return []envelope.Slot{badRequest(stmt, "each batch target must be a session ID or <session-id>:<positive index>")}
		}
		if channel {
			n, err := strconv.Atoi(index)
			if err != nil || n <= 0 || strconv.Itoa(n) != index {
				return []envelope.Slot{badRequest(stmt, "invalid channel index in "+target+"; use <session-id>:<positive index>")}
			}
		}
	}
	slots := make([]envelope.Slot, 0, len(targets))
	for _, target := range targets {
		entry := stmt
		entry.Target, entry.Path = target, "close "+target
		id, _, _ := strings.Cut(target, ":")
		if s.sessMgr.Get(id) == nil {
			slots = append(slots, s.slotFromFailure(ctx, entry, &toolFailure{
				code: CodeSessionNotFound, msg: "no session with ID "+id+"; use IDs from ls",
			}))
			continue
		}
		slots = append(slots, s.runDirective(ctx, entry, win)...)
	}
	return slots
}
