package mcp

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	pkgsftp "github.com/pkg/sftp"

	"github.com/open-mcp-ai/termcp/internal/clock"
	filesftp "github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/pkg/api"
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
		return fileErr(err), nil
	}
	return jsonResult(toMap(result)), nil
}

func (s *Server) handleFileWrite(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	// A POINTER ALL THE WAY DOWN, because absence and 0 are different requests:
	// "no offset" means replace the file, "offset 0" means overwrite from the first
	// byte. They shared the value 0 once, and the destructive one was the default.
	offset := int64PtrFromArgs(args, "offset")
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
		return fileErr(err), nil
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
	// Resolve for the echoed id and URLs; the SFTP client above already proved the
	// session exists and is running, so a failure here is only about the spelling.
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	result, err := sftpCli.StatFile(remotePath)
	if err != nil {
		return fileErr(err), nil
	}
	m := toMap(result)
	// Build the URLs and the echoed id from the RESOLVED session, never the
	// argument: a locator here would produce
	// /api/sessions/termcp://#<sid>/files/download, a URL containing '#' and '://'
	// that addresses nothing.
	sessID := sess.ID
	m["download_url"] = s.baseURL + "/api/sessions/" + sessID + "/files/download?path=" + url.QueryEscape(remotePath)
	m["upload_url"] = s.baseURL + "/api/sessions/" + sessID + "/files/upload"
	m["session_id"] = sessID
	return jsonResult(m), nil
}

func (s *Server) handleFileDelete(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
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
		return fileErr(err), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileRename(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	fromPath := getString(args, "from_path", "")
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
		return fileErr(err), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileMakeDir(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
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
		return fileErr(err), nil
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
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	// sess.ID, not the argument: these URLs are meant to be opened, and a locator
	// would put '#' and '://' into the path.
	//
	// Requested response headers ride inside the download URL (see
	// api.AppendResponseHeaders): the caller's list is folded into the query, so
	// whoever opens it later — curl, a browser — gets the headers without
	// anything else passing them along.
	q := url.Values{"path": {remotePath}}
	headers, bad := responseHeadersArg(args)
	if bad != nil {
		return bad, nil
	}
	if err := api.AppendResponseHeaders(q, headers); err != nil {
		return toolError(CodeInvalidArgument, "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{
		"download_url": s.baseURL + "/api/sessions/" + sess.ID + "/files/download?" + q.Encode(),
		"upload_url":   s.baseURL + "/api/sessions/" + sess.ID + "/files/upload",
		"session_id":   sess.ID,
		"remote_path":  remotePath,
	}), nil
}

// responseHeadersArg reads the response_headers argument: a map of header name
// to value, the shape an MCP client sends for an object-typed property. A value
// that is not a string is refused rather than dropped: the caller would get a
// URL that looks like it carries the header and does not.
func responseHeadersArg(args map[string]any) (map[string]string, *mcpgo.CallToolResult) {
	v, ok := args["response_headers"]
	if !ok || v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, toolError(CodeInvalidArgument, "%s", "response_headers must be an object of header name to string value")
	}
	headers := make(map[string]string, len(raw))
	for name, value := range raw {
		s, ok := value.(string)
		if !ok {
			return nil, toolError(CodeInvalidArgument, "response header %q must be a string, got %T", name, value)
		}
		headers[name] = s
	}
	return headers, nil
}

// handleFilePerm dispatches chmod, chown, chtimes.
func (s *Server) handleFilePerm(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	action := getString(args, "action", "")

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
			return fileErr(err), nil
		}
	case "chown":
		remotePath := getString(args, "remote_path", "")
		uid := intPtrFromArgs(args, "uid")
		gid := intPtrFromArgs(args, "gid")
		// POINTERS, NOT -1: uid 0 IS root, so a sentinel drawn from inside the valid
		// range cannot say "not given". The old check collapsed three outcomes into one
		// sentence — a missing uid, a missing gid, and a negative one all answered
		// "chown requires remote_path, uid, and gid" — so a caller who passed
		// `--uid 0` and forgot gid could not tell which field was actually wrong.
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "chown requires remote_path"), nil
		}
		if uid == nil || gid == nil {
			return toolError(CodeInvalidArgument, "%s", "chown requires uid and gid"), nil
		}
		if *uid < 0 || *gid < 0 {
			return toolError(CodeInvalidArgument, "%s", "uid and gid must be >= 0"), nil
		}
		if err := sftpCli.ChownFile(remotePath, *uid, *gid); err != nil {
			return fileErr(err), nil
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
			return fileErr(err), nil
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
			return fileErr(err), nil
		}
		return jsonResult(map[string]any{"target": target}), nil
	case "symlink":
		target := getString(args, "target", "")
		linkPath := getString(args, "link_path", "")
		if target == "" || linkPath == "" {
			return toolError(CodeInvalidArgument, "%s", "symlink requires target and link_path"), nil
		}
		if err := sftpCli.SymlinkFile(target, linkPath); err != nil {
			return fileErr(err), nil
		}
	case "link":
		existingPath := getString(args, "existing_path", "")
		newPath := getString(args, "new_path", "")
		if existingPath == "" || newPath == "" {
			return toolError(CodeInvalidArgument, "%s", "link requires existing_path and new_path"), nil
		}
		if err := sftpCli.LinkFile(existingPath, newPath); err != nil {
			return fileErr(err), nil
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
		// SIZE IS REQUIRED, AND ZERO IS A REAL SIZE. The old check demanded
		// remote_path and then used whatever size happened to be there — defaulting
		// to 0 — so `--action fs --op truncate --remote-path f` with no --size
		// EMPTIED THE FILE and answered success. A missing size and a size of 0 are
		// different requests, and the destructive one must not be the default.
		sizeVal, hasSize := args["size"]
		if remotePath == "" || !hasSize {
			return toolError(CodeInvalidArgument, "%s", "truncate requires remote_path and size (use --size 0 to empty a file)"), nil
		}
		size := int64(getFloat64(map[string]any{"size": sizeVal}, "size", 0))
		if err := sftpCli.TruncateFile(remotePath, size); err != nil {
			return fileErr(err), nil
		}
	case "realpath":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "realpath requires remote_path"), nil
		}
		canonical, err := sftpCli.RealPath(remotePath)
		if err != nil {
			return fileErr(err), nil
		}
		// TWO SERVERS, TWO MEANINGS OF "REAL PATH".
		//
		// SSH_FXP_REALPATH is only specified to make a path absolute and clean it.
		// OpenSSH goes further and resolves symlinks; pkg/sftp's server does not —
		// it calls filepath.Abs and stops. So `realpath symlink` answered the link's
		// own path on the internal profile and the target's path on a real host: the
		// same directive, two answers, and the caller cannot tell which one it got.
		//
		// The internal profile's filesystem IS this process's filesystem, so the
		// missing step can be taken here. A remote host keeps whatever its server
		// does: resolving its symlinks would mean walking them over SFTP, and
		// inventing a second answer for a host that already answered is worse than
		// reporting the answer it gave.
		// Resolved here rather than up front: only this branch needs the session
		// record, and sftpClient already proved the session exists and is running.
		if sess, serr := s.requireRunningSession(sessionID); serr == nil && canonical != "" &&
			sess.Info().SSHEndpoint == "internal" {
			// pkg/sftp's server normalises a Windows path to "/C:/dir/file", because
			// the SFTP wire form is POSIX-shaped. EvalSymlinks cannot read that: the
			// leading slash makes it a rooted path on the current volume, so the call
			// fails and the link is reported unresolved -- the bug this block exists
			// to fix, surviving on one platform. Strip the slash before asking the OS,
			// and put the wire shape back afterwards so the reply keeps one format.
			lookup := canonical
			prefixed := false
			if isWindowsDrivePrefixedPath(lookup) {
				lookup = lookup[1:]
				prefixed = true
			}
			if resolved, rerr := filepath.EvalSymlinks(lookup); rerr == nil {
				if prefixed {
					resolved = "/" + filepath.ToSlash(resolved)
				}
				canonical = resolved
			}
		}
		return jsonResult(map[string]any{"canonical_path": canonical}), nil
	case "statvfs":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "statvfs requires remote_path"), nil
		}
		result, err := sftpCli.StatVFS(remotePath)
		if err != nil {
			return fileErr(err), nil
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
		return fileErr(err), nil
	}
	return jsonResult(map[string]any{"directory": dir}), nil
}

// fileErr turns a filesystem failure into the tool error that describes it.
//
// EVERY FILE FAILURE USED TO BE CodeOperationFailed, which renders as 500. That
// flattened three different situations into one: a path that does not exist (the
// caller's typo, or a file that was moved), a permission refusal (policy), and a
// genuine transport fault. A caller cannot retry intelligently or fix a path it is
// told only "operation failed".
//
// The not-found case is also the one a caller hits most: stat/read/delete on a
// missing path is an ordinary answer, not a server fault.
func fileErr(err error) *mcpgo.CallToolResult {
	switch {
	case err == nil:
		return toolError(CodeOperationFailed, "%s", "unknown file error")
	case errors.Is(err, os.ErrNotExist), errors.Is(err, pkgsftp.ErrSSHFxNoSuchFile):
		return toolError(CodeFileNotFound, "%s", err.Error())
	case errors.Is(err, os.ErrPermission), errors.Is(err, pkgsftp.ErrSSHFxPermissionDenied):
		return toolError(CodeForbidden, "%s", err.Error())
	case errors.Is(err, filesftp.ErrIsDirectory):
		// A directory is a caller mistake, not a fault: 400 rather than 500.
		return toolError(CodeInvalidArgument, "%s", err.Error())
	}
	return toolError(CodeOperationFailed, "%s", err.Error())
}

// isWindowsDrivePrefixedPath reports whether p looks like "/C:/..." or "/C:\...",
// the POSIX-shaped wire form pkg/sftp emits for a Windows absolute path.
func isWindowsDrivePrefixedPath(p string) bool {
	if len(p) < 4 || p[0] != '/' {
		return false
	}
	c := p[1]
	if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return false
	}
	if p[2] != ':' {
		return false
	}
	return p[3] == '/' || p[3] == '\\'
}
