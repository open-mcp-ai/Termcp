package mcp

import (
	"context"

	mcpgo "github.com/open-mcp-ai/termcp/mcp"
)

// call is the one way a tool is invoked, whether the caller is the MCP server
// (through withLogging) or code and tests inside this package.
//
// The admission decision lives in this path, not in the handlers, so anything
// that wants to behave like a real tool call has to come through here. That is
// the point: a test that calls handleFileWrite directly is testing a function
// nobody can reach, and a review gate asserted that way proves nothing about
// what an agent experiences.
//
// The parameters take the same JSON shape the wire carries, so what a test sends
// is what a model sends.
func (s *Server) call(ctx context.Context, tool string, args map[string]any) (*mcpgo.CallToolResult, error) {
	h := s.handlerFor(tool)
	if h == nil {
		return toolError(CodeInvalidArgument, "%s", "unknown tool "+tool), nil
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	return withLogging(tool, h, s)(ctx, req)
}

// handlerFor is the operation name → handler map: the implementation layer
// behind the `term` directives and the single registry the review executor
// replays through. Having one map means an operation cannot be reachable from a
// directive and unreplayable by the executor, which is a class of bug review
// mode makes expensive — the human approves, and the operation fails with "no
// handler".
func (s *Server) handlerFor(tool string) toolHandler {
	switch tool {
	case "term":
		return s.handleTerm
	case "session_start":
		return s.handleStartSession
	case "session_list":
		return s.handleListSessions
	case "session_info":
		return s.handleGetSessionInfo
	case "session_terminate":
		return s.handleTerminateSession
	case "session_delete":
		return s.handleDeleteSession
	case "shell_open":
		return s.handleStartSubShell
	case "shell_list":
		return s.handleListSubshells
	case "shell_close":
		return s.handleCloseShell
	case "shell_archive":
		return s.handleArchiveShell
	case "shell_input":
		return s.handleSendInput
	case "shell_key":
		return s.handlePressKey
	case "shell_output":
		return s.handleReadOutput
	case "shell_resize":
		return s.handleResizePty
	case "shell_detect":
		return s.handleDetectShell
	case "shell_reader_register":
		return s.handleRegisterReader
	case "shell_reader_unregister":
		return s.handleUnregisterReader
	case "message":
		return s.handleMessageOps
	case "notify_user":
		return s.handleNotifyUser
	case "forward":
		return s.handleForwardOps
	case "file_read":
		return s.handleFileRead
	case "file_write":
		return s.handleFileWrite
	case "file_replace":
		return s.handleFileReplace
	case "file_stat":
		return s.handleFileStat
	case "file_delete":
		return s.handleFileDelete
	case "file_rename":
		return s.handleFileRename
	case "file_mkdir":
		return s.handleFileMakeDir
	case "file_urls":
		return s.handleGetFileURLs
	case "file_perm":
		return s.handleFilePerm
	case "file_link":
		return s.handleFileLinkOp
	case "file_fs":
		return s.handleFileFsOp
	case "file_getwd":
		return s.handleFileGetwd
	case "ssh_config":
		return s.handleSSHConfigOps
	case "extension":
		return s.handleExtensionOps
	default:
		return nil
	}
}
