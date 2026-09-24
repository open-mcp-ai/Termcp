package mcp

import (
	"context"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// handleMessageOps is the low-frequency transcript entry point: it lists the
// spans of a shell's byte log. The bytes are read through shell_output, so
// there is no payload-fetching action.
func (s *Server) handleMessageOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	switch getString(request.GetArguments(), "action", "") {
	case "list":
		return s.handleListMessages(ctx, request)
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be list"), nil
	}
}

// handleForwardOps is the low-frequency port-forward entry point.
func (s *Server) handleForwardOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	action := getString(args, "action", "")
	// list is a read. Opening a tunnel exposes a port and closing one tears down
	// something someone may be relying on; both are changes to the host, so both
	// are gated. The action is checked before dispatch so the reviewer's summary
	// can say which one it is.
	if action != "list" {
		sessionID := strings.TrimSpace(getString(args, "session_id", ""))
		if res, held := s.gateOperation(ctx, request, "forward", sessionID); held {
			return res, nil
		}
	}
	switch action {
	case "local":
		return s.handleLocalForward(ctx, request)
	case "remote":
		return s.handleRemoteForward(ctx, request)
	case "dynamic":
		return s.handleDynamicForward(ctx, request)
	case "list":
		return s.handleListForwards(ctx, request)
	case "close":
		return s.handleCloseForward(ctx, request)
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be local, remote, dynamic, list, or close"), nil
	}
}

// handleSSHConfigOps dispatches profile listing and, when enabled, profile
// management. The write-capable schema is installed by
// RegisterSSHConfigWriteTools; the default schema exposes list only.
func (s *Server) handleSSHConfigOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	action := getString(request.GetArguments(), "action", "")
	switch action {
	case "list":
		return s.handleListSSHConfigs(ctx, request)
	case "create", "edit", "copy", "delete":
		if !s.sshConfigWrites {
			return toolError(CodeOperationFailed, "%s", "SSH config write actions are disabled; start termcp with --mcp-manage-ssh-configs"), nil
		}
		switch action {
		case "create":
			return s.handleCreateSSHConfig(ctx, request)
		case "edit":
			return s.handleEditSSHConfig(ctx, request)
		case "copy":
			return s.handleCopySSHConfig(ctx, request)
		default: // delete
			return s.handleDeleteSSHConfig(ctx, request)
		}
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be list, create, edit, copy, or delete"), nil
	}
}
