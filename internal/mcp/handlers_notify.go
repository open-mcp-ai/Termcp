package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/notify"
)

// handleShellNotifyOps dispatches register, unregister, and list actions on shell_notify.
func (s *Server) handleShellNotifyOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.notifyMgr == nil {
		return toolError(CodeNotConfigured, "%s", "notification manager not initialized"), nil
	}
	args := request.GetArguments()
	action := strings.TrimSpace(getString(args, "action", ""))

	switch action {
	case "register":
		shellID := strings.TrimSpace(getString(args, "shell_id", ""))
		if shellID == "" {
			return toolError(CodeInvalidArgument, "%s", "shell_id is required for register"), nil
		}
		// Validate that the shell exists in running sessions
		shell, bad := s.requireShell(shellID)
		if bad != nil {
			return bad, nil
		}

		channelStr := strings.TrimSpace(getString(args, "channel", ""))
		var channel notify.Channel
		switch channelStr {
		case "resource":
			channel = notify.ChannelResource
		case "sampling":
			channel = notify.ChannelSampling
		default:
			return toolError(CodeInvalidArgument, "%s", "channel must be resource or sampling"), nil
		}

		eventStr := strings.TrimSpace(getString(args, "event", "output"))
		var event notify.Event
		switch eventStr {
		case "exit":
			event = notify.EventExit
		case "silence":
			event = notify.EventSilence
		case "output":
			event = notify.EventOutput
		default:
			return toolError(CodeInvalidArgument, "%s", "event must be exit, silence, or output"), nil
		}

		silenceSec := int(getFloat64(args, "silence_seconds", 3))
		if silenceSec <= 0 {
			silenceSec = 3
		}

		sessionID := shell.ParentSessionID()
		// Capture the MCP client session that registered this rule so sampling
		// notifications can be delivered later from timer/exit goroutines, where
		// the dispatch context no longer carries the client session.
		target := mcpserver.ClientSessionFromContext(ctx)
		// Register under the resolved shell id: the rule's ShellID is what the
		// cascade matches on (ClearShell from the exit watcher, and the session's
		// ResourceScope) and what the broadcast URI carries
		// (termcp://shells/<shell_id>). A locator stored here would make both the
		// cleanup and the URI name something that is not a shell.
		rule, err := s.notifyMgr.Register(sessionID, shell.ID, channel, event, silenceSec, target)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		// Attach at creation: session deletion releases the rule (and stops its
		// timers) through the session's ResourceScope.
		if sess := s.sessMgr.Get(sessionID); sess != nil {
			sess.AttachCleanup(func() { s.notifyMgr.Unregister(rule.ID) })
		}
		return jsonResult(map[string]any{
			"ok":       true,
			"rule_id":  rule.ID,
			"shell_id": rule.ShellID,
			"channel":  string(rule.Channel),
			"event":    string(rule.Event),
		}), nil

	case "unregister":
		ruleID := strings.TrimSpace(getString(args, "rule_id", ""))
		if ruleID == "" {
			return toolError(CodeInvalidArgument, "%s", "rule_id is required for unregister"), nil
		}
		if !s.notifyMgr.Unregister(ruleID) {
			return toolError(CodeRuleNotFound, "%s", fmt.Sprintf("rule %q not found", ruleID)), nil
		}
		return jsonResult(map[string]any{"ok": true, "rule_id": ruleID}), nil

	case "list":
		shellID := strings.TrimSpace(getString(args, "shell_id", ""))
		// An optional filter: resolve it so a locator selects the same rules a bare
		// id would, instead of matching nothing.
		if shellID != "" {
			shell, bad := s.requireShell(shellID)
			if bad != nil {
				return bad, nil
			}
			shellID = shell.ID
		}
		rules := s.notifyMgr.List(shellID)
		return jsonResult(map[string]any{"rules": rules}), nil

	default:
		return toolError(CodeInvalidArgument, "%s", "action must be register, unregister, or list"), nil
	}
}

// maxNotifyMessageLen caps the message shown in the Web UI toast.
const maxNotifyMessageLen = 2000

// handleNotifyUser posts a user-facing notification to every open termcp Web UI
// tab (toast + browser system notification) and optionally highlights the card
// of the given session. Unlike shell_notify, which wakes the AI Agent, this
// reaches the human at the browser.
func (s *Server) handleNotifyUser(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	message := strings.TrimSpace(getString(args, "message", ""))
	if message == "" {
		return toolError(CodeInvalidArgument, "%s", "message is required"), nil
	}
	if r := []rune(message); len(r) > maxNotifyMessageLen {
		message = string(r[:maxNotifyMessageLen])
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
