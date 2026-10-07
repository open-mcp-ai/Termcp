package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/open-mcp-ai/termcp/internal/notify"
)

// SendResourceNotification broadcasts an MCP resource update event.
func (s *Server) SendResourceNotification(ctx context.Context, shellID string) error {
	uri := ResourceURLScheme + "shells/" + shellID
	s.mcpServer.SendNotificationToAllClients("notifications/resources/updated", map[string]any{
		"uri": uri,
	})
	return nil
}

// SendSamplingNotification issues an MCP sampling/createMessage request to the
// client session that registered the rule, waking the AI. target is the opaque
// handle captured by shell_notify(action=register); notifications are dispatched
// from timer/exit goroutines, so the client session cannot be recovered from the
// dispatch context and must be carried on the rule.
func (s *Server) SendSamplingNotification(ctx context.Context, shellID string, target any, event notify.Event, status string) error {
	sess, ok := target.(mcpserver.SessionWithSampling)
	if !ok || sess == nil {
		return fmt.Errorf("no sampling-capable client session for shell %q", shellID)
	}
	prompt := fmt.Sprintf("[termcp reminder] Shell '%s' emitted event '%s' (status: %s). Inspect output via shell_output if needed.", shellID, event, status)
	req := mcpgo.CreateMessageRequest{
		CreateMessageParams: mcpgo.CreateMessageParams{
			SystemPrompt: "termcp notification daemon",
			Messages: []mcpgo.SamplingMessage{
				{
					Role:    mcpgo.RoleUser,
					Content: mcpgo.NewTextContent(prompt),
				},
			},
			MaxTokens: 50,
		},
	}
	_, err := sess.RequestSampling(ctx, req)
	return err
}

// SetUINotifier wires the Web UI notification delivery end (notify_user tool).
func (s *Server) SetUINotifier(fn func(level, title, message, sessionID string, durationSec int) int) {
	s.uiNotify = fn
}

// NotifyManager exposes the shell notification rule manager so other subsystems
// (e.g. the Web UI) can list and unregister rules. May return nil in tests that
// construct a bare Server.
func (s *Server) NotifyManager() *notify.Manager {
	return s.notifyMgr
}
