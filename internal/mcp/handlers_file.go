package mcp

import (
	"context"
	"net/url"
	"os"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

func (s *Server) handleFileRead(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	offset := int64(getFloat64(args, "offset", 0))
	length := int64(getFloat64(args, "length", 0))
	mode := getString(args, "mode", "text")
	localPath := getString(args, "local_path", "")

	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	if mode != "text" && mode != "hex" && mode != "file" {
		return toolError(CodeInvalidArgument, "%s", `mode must be "text", "hex", or "file"`), nil
	}
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	result, err := sftpCli.ReadFile(remotePath, offset, length, mode, localPath)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return jsonResult(toMap(result)), nil
}

func (s *Server) handleFileWrite(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	if res, held := s.gateOperation(ctx, request, "file_write", sessionID); held {
		return res, nil
	}
	remotePath := getString(args, "remote_path", "")
	offset := int64(getFloat64(args, "offset", 0))
	data := getString(args, "data", "")
	mode := getString(args, "mode", "text")
	localPath := getString(args, "local_path", "")
	localOffset := int64(getFloat64(args, "local_offset", 0))
	length := int64(getFloat64(args, "length", 0))

	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	if localPath == "" && data == "" {
		return toolError(CodeInvalidArgument, "%s", "data or local_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	n, err := sftpCli.WriteFile(remotePath, offset, data, mode, localPath, localOffset, length)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{"ok": true, "bytes_written": n}), nil
}

func (s *Server) handleFileStat(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")

	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	result, err := sftpCli.StatFile(remotePath)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	m := toMap(result)
	m["download_url"] = s.baseURL + "/api/sessions/" + sessionID + "/files/download?path=" + url.QueryEscape(remotePath)
	m["upload_url"] = s.baseURL + "/api/sessions/" + sessionID + "/files/upload"
	m["session_id"] = sessionID
	return jsonResult(m), nil
}

func (s *Server) handleFileDelete(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if res, held := s.gateOperation(ctx, request, "file_delete", sessionID); held {
		return res, nil
	}
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.RemoveFile(remotePath); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileRename(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	fromPath := getString(args, "from_path", "")
	if res, held := s.gateOperation(ctx, request, "file_rename", sessionID); held {
		return res, nil
	}
	toPath := getString(args, "to_path", "")
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if fromPath == "" || toPath == "" {
		return toolError(CodeInvalidArgument, "%s", "from_path and to_path required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.RenameFile(fromPath, toPath); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileMakeDir(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if res, held := s.gateOperation(ctx, request, "file_mkdir", sessionID); held {
		return res, nil
	}
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.MakeDir(remotePath); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleGetFileURLs(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	if _, bad := s.requireRunningSession(sessionID); bad != nil {
		return bad, nil
	}
	return jsonResult(map[string]any{
		"download_url": s.baseURL + "/api/sessions/" + sessionID + "/files/download?path=" + url.QueryEscape(remotePath),
		"upload_url":   s.baseURL + "/api/sessions/" + sessionID + "/files/upload",
		"session_id":   sessionID,
		"remote_path":  remotePath,
	}), nil
}

// handleFilePerm dispatches chmod, chown, chtimes.
func (s *Server) handleFilePerm(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	action := getString(args, "action", "")
	if res, held := s.gateOperation(ctx, request, "file_perm", sessionID); held {
		return res, nil
	}

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()

	switch action {
	case "chmod":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "chmod requires remote_path and mode (decimal, e.g. 493 = 0755)"), nil
		}
		if _, ok := args["mode"]; !ok {
			return toolError(CodeInvalidArgument, "%s", "chmod requires remote_path and mode (decimal, e.g. 493 = 0755)"), nil
		}
		mode := os.FileMode(getFloat64(args, "mode", 0))
		if err := sftpCli.ChmodFile(remotePath, mode); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "chown":
		remotePath := getString(args, "remote_path", "")
		uid := int(getFloat64(args, "uid", -1))
		gid := int(getFloat64(args, "gid", -1))
		if remotePath == "" || uid < 0 || gid < 0 {
			return toolError(CodeInvalidArgument, "%s", "chown requires remote_path, uid, and gid"), nil
		}
		if err := sftpCli.ChownFile(remotePath, uid, gid); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "chtimes":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "chtimes requires remote_path, atime, and mtime"), nil
		}
		if _, ok := args["atime"]; !ok {
			return toolError(CodeInvalidArgument, "%s", "chtimes requires remote_path, atime, and mtime"), nil
		}
		if _, ok := args["mtime"]; !ok {
			return toolError(CodeInvalidArgument, "%s", "chtimes requires remote_path, atime, and mtime"), nil
		}
		// Unix milliseconds, like every other timestamp the API accepts or returns.
		atime := clock.Time(int64(getFloat64(args, "atime", 0)))
		mtime := clock.Time(int64(getFloat64(args, "mtime", 0)))
		if err := sftpCli.ChtimesFile(remotePath, atime, mtime); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be chmod, chown, or chtimes"), nil
	}
	return successResult(), nil
}

// handleFileLinkOp dispatches readlink, symlink, link.
func (s *Server) handleFileLinkOp(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	action := getString(args, "action", "")
	// readlink changes nothing, so it is not gated: putting a read behind a
	// decision teaches a reviewer to click Accept without reading.
	if action != "readlink" {
		if res, held := s.gateOperation(ctx, request, "file_link", sessionID); held {
			return res, nil
		}
	}

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()

	switch action {
	case "readlink":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "readlink requires remote_path"), nil
		}
		target, err := sftpCli.ReadLink(remotePath)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"target": target}), nil
	case "symlink":
		target := getString(args, "target", "")
		linkPath := getString(args, "link_path", "")
		if target == "" || linkPath == "" {
			return toolError(CodeInvalidArgument, "%s", "symlink requires target and link_path"), nil
		}
		if err := sftpCli.SymlinkFile(target, linkPath); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "link":
		existingPath := getString(args, "existing_path", "")
		newPath := getString(args, "new_path", "")
		if existingPath == "" || newPath == "" {
			return toolError(CodeInvalidArgument, "%s", "link requires existing_path and new_path"), nil
		}
		if err := sftpCli.LinkFile(existingPath, newPath); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be readlink, symlink, or link"), nil
	}
	return successResult(), nil
}

// handleFileFsOp dispatches truncate, realpath, statvfs.
func (s *Server) handleFileFsOp(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	action := getString(args, "action", "")
	// realpath and statvfs are reads; only truncate changes the host.
	if action == "truncate" {
		if res, held := s.gateOperation(ctx, request, "file_fs", sessionID); held {
			return res, nil
		}
	}

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()

	switch action {
	case "truncate":
		remotePath := getString(args, "remote_path", "")
		size := int64(getFloat64(args, "size", 0))
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "truncate requires remote_path and size"), nil
		}
		if err := sftpCli.TruncateFile(remotePath, size); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "realpath":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "realpath requires remote_path"), nil
		}
		canonical, err := sftpCli.RealPath(remotePath)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"canonical_path": canonical}), nil
	case "statvfs":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "statvfs requires remote_path"), nil
		}
		result, err := sftpCli.StatVFS(remotePath)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(toMap(result)), nil
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be truncate, realpath, or statvfs"), nil
	}
	return successResult(), nil
}

// handleFileGetwd returns the remote working directory via SSH/SFTP.
func (s *Server) handleFileGetwd(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	dir, err := sftpCli.Getwd()
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{"directory": dir}), nil
}
