package mcp

import (
	"context"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/mcp"
)

// maxNotifyMessageLen caps the message shown in the Web UI toast.
const maxNotifyMessageLen = 2000

// handleNotifyUser posts a user-facing notification to every open termcp Web UI
// tab (toast + browser system notification) and optionally highlights the card
// of the given session. It reaches the human at the browser; nothing in termcp
// wakes an agent, which is what the retired reverse-wake-up rules tried to do.
func (s *Server) handleNotifyUser(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	message := strings.TrimSpace(getString(args, "message", ""))
	if message == "" {
		return toolError(CodeInvalidArgument, "%s", "message is required"), nil
	}
	// A TRUNCATED TOAST SAYS IT WAS TRUNCATED (issue #204).
	//
	// The cap is legitimate — this is a toast, not a document, and the value is a
	// protection on the browser's side rather than a decision about the caller's work.
	// What was not legitimate is that the cut was silent: a caller who sent a long
	// message got "ok" and the human read a sentence that stopped mid-word with no
	// indication that anything was missing.
	if r := []rune(message); len(r) > maxNotifyMessageLen {
		message = string(r[:maxNotifyMessageLen]) +
			" … (" + strconv.Itoa(len(r)-maxNotifyMessageLen) + " more characters were not shown: the toast is capped at " +
			strconv.Itoa(maxNotifyMessageLen) + ")"
	}

	level := strings.TrimSpace(getString(args, "level", "info"))
	if level == "" {
		level = "info"
	}
	switch level {
	case "info", "success", "warn", "error":
	default:
		return toolError(CodeInvalidArgument, "%s", "level must be info, success, warn, or error"), nil
	}

	title := strings.TrimSpace(getString(args, "title", ""))
	if title == "" {
		title = "termcp"
	}

	durationSec := int(getFloat64(args, "duration_seconds", 10))
	if durationSec < 0 {
		durationSec = 0
	}
	if durationSec > 600 {
		durationSec = 600
	}

	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	if sessionID != "" {
		sess, bad := s.requireSession(sessionID)
		if bad != nil {
			return bad, nil
		}
		// The resolved id is what the Web UI matches when it highlights a card.
		sessionID = sess.ID
	}

	delivered := 0
	if s.uiNotify != nil {
		delivered = s.uiNotify(level, title, message, sessionID, durationSec)
	}
	out := map[string]any{"ok": true, "delivered": delivered, "title": title, "level": level}
	if sessionID != "" {
		out["session_id"] = sessionID
	}
	if delivered == 0 {
		out["hint"] = "no Web UI tab is open; the notification was not displayed"
	}
	return jsonResult(out), nil
}
