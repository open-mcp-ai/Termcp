package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) handleListSessions(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	all := s.sessMgr.ListAll()
	return jsonResult(map[string]any{"sessions": filterRunning(all)}), nil
}

func (s *Server) handleGetSessionInfo(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	info := sess.Info()
	data, _ := json.Marshal(info)
	return mcpgo.NewToolResultText(string(data)), nil
}

func (s *Server) handleTerminateSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	force := getBool(args, "force", false)
	gracePeriod := getFloat64(args, "grace_period", 5.0)
	if gracePeriod < 0 || gracePeriod > 60 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("grace_period must be between 0 and 60, got %v", gracePeriod)), nil
	}

	// Comma-separated form: close every listed session independently so one bad
	// entry does not stop the rest; the JSON result carries per-session outcomes.
	if ids, isBatch := splitBatchIDs(sessionID); isBatch {
		grace := time.Duration(gracePeriod * float64(time.Second))
		return s.batchSessionResult(ids, func(id string) error {
			s.sessMgr.Terminate(id, force, grace)
			return nil
		}), nil
	}

	// Resolve first, then act on the resolved id: Terminate is a no-op for an id it
	// cannot find, so passing a locator straight through would report success for a
	// session that was never touched.
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	// Close only: the process/transport stops and the entry becomes DEAD, but it
	// stays in the registry (and in the Web UI as a read-only tile) so its output
	// remains readable via shell_output. session_delete removes it for good.
	s.sessMgr.Terminate(sess.ID, force, time.Duration(gracePeriod*float64(time.Second)))
	return successResult(), nil
}

func (s *Server) handleDeleteSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")

	// Comma-separated form: erase every listed session independently so one bad
	// entry does not stop the rest; the JSON result carries per-session outcomes.
	if ids, isBatch := splitBatchIDs(sessionID); isBatch {
		return s.batchSessionResult(ids, func(id string) error { return s.sessMgr.Delete(id) }), nil
	}

	// Resolve first, then act on the resolved id: Delete validates its argument as a
	// storage path component, so a locator (which contains '#' and ':') would be
	// rejected as an invalid id instead of deleting the session it names.
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	// Permanent: closes any live transport, releases the session's resources
	// (shells, forwards, notification rules, buffers) and removes its on-disk
	// directory (manifests + log.bin + log.jsonl). Irreversible.
	if err := s.sessMgr.Delete(sess.ID); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}
