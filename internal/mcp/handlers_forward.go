package mcp

import (
	"context"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) handleLocalForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remoteHost := getString(args, "remote_host", "localhost")
	remotePort := int(getFloat64(args, "remote_port", 0))
	localPort := int(getFloat64(args, "local_port", 0))

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePort <= 0 || remotePort > 65535 {
		return toolError(CodeInvalidArgument, "%s", "remote_port required (1-65535)"), nil
	}

	sess, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	if s.forwardMgr == nil {
		return toolError(CodeNotConfigured, "%s", "forward manager not available"), nil
	}
	// Register under the resolved session id: the forward's SessionID is what the
	// DEAD cascade matches on (forwardMgr.CloseBySession), so storing the argument
	// verbatim would leave the listener alive when the session goes down if that
	// argument was a locator.
	fw, err := s.forwardMgr.CreateLocal(sess.ID, sess.Info().Name, remoteHost, remotePort, localPort, sshCli)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	// Attach at creation: the session's ResourceScope closes this forward when
	// the session is deleted, so no subsystem has to be remembered elsewhere.
	sess.AttachCleanup(func() { _ = s.forwardMgr.Close(fw.ForwardID) })
	return jsonResult(map[string]any{
		"local_port": fw.ListenAddr,
		"forward_id": fw.ForwardID,
	}), nil
}

func (s *Server) handleRemoteForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	localHost := getString(args, "local_host", "0.0.0.0")
	localPort := int(getFloat64(args, "local_port", 0))
	remoteHost := getString(args, "remote_host", "")
	remotePort := int(getFloat64(args, "remote_port", 0))

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if localPort <= 0 || localPort > 65535 {
		return toolError(CodeInvalidArgument, "%s", "local_port required (1-65535)"), nil
	}
	if remoteHost == "" || remotePort <= 0 {
		return toolError(CodeInvalidArgument, "%s", "remote_host and remote_port required"), nil
	}

	sess, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	if s.forwardMgr == nil {
		return toolError(CodeNotConfigured, "%s", "forward manager not available"), nil
	}
	// sess.ID, not the argument — see handleLocalForward.
	fw, err := s.forwardMgr.CreateRemote(sess.ID, sess.Info().Name, localHost, localPort, remoteHost, remotePort, sshCli)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	sess.AttachCleanup(func() { _ = s.forwardMgr.Close(fw.ForwardID) })
	return jsonResult(map[string]any{
		"remote_port": localPort,
		"forward_id":  fw.ForwardID,
	}), nil
}

func (s *Server) handleDynamicForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	localPort := int(getFloat64(args, "local_port", 0))

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}

	sess, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	if s.forwardMgr == nil {
		return toolError(CodeNotConfigured, "%s", "forward manager not available"), nil
	}
	info := sess.Info()
	// sess.ID, not the argument — see handleLocalForward.
	fw, err := s.forwardMgr.CreateDynamic(sess.ID, info.Name, localPort, sshCli, info.SSHEndpoint == "internal")
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	sess.AttachCleanup(func() { _ = s.forwardMgr.Close(fw.ForwardID) })
	return jsonResult(map[string]any{"local_port": fw.ListenAddr, "forward_id": fw.ForwardID}), nil
}

func (s *Server) handleListForwards(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.forwardMgr == nil {
		return jsonResult(map[string]any{"forwards": []any{}}), nil
	}
	fws := s.forwardMgr.List()
	arr := make([]any, len(fws))
	for i, fw := range fws {
		arr[i] = fw
	}
	return jsonResult(map[string]any{"forwards": arr}), nil
}

func (s *Server) handleCloseForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	forwardID := getString(args, "forward_id", "")
	if forwardID == "" {
		return toolError(CodeInvalidArgument, "%s", "forward_id required"), nil
	}
	if s.forwardMgr == nil {
		return toolError(CodeNotConfigured, "%s", "forward manager not available"), nil
	}
	if err := s.forwardMgr.Close(forwardID); err != nil {
		return toolError(forwardErrCode(err), "%s", err.Error()), nil
	}
	return successResult(), nil
}

// --- File operation tool handlers ---
