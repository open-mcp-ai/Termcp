package capability

import "strings"

// AgentCall describes the authorization facts of a tool call. Surface adapters
// resolve the named resources, while this package owns operation classification.
type AgentCall struct {
	Function         string
	ProfileArgument  string
	StartsCommand    bool
	NeedsLocal       bool
	LocalTransfer    bool
	ParsedDirectives bool
}

func AgentFunction(id string) string {
	switch id {
	case SessionList, SessionInfo, ShellList:
		return "session_read"
	case SessionStart, SessionTerminate, SessionDelete, ShellClose:
		return "session_manage"
	case SessionMessage, ShellOutput, ShellDetect, ShellReaderRegister, ShellReaderUnregister:
		return "terminal_read"
	case ShellOpen, ShellInput, ShellKey, ShellResize:
		return "terminal_write"
	case FileRead, FileStat, FileURLs, FileGetwd:
		return "file_read"
	case FileWrite, FileDelete, FileRename, FileMkdir, FilePerm, FileLink, FileFs:
		return "file_write"
	case ProfileList, "profile.get":
		return "profile_read"
	case ProfileCreate, ProfileEdit, ProfileCopy, ProfileDelete:
		return "profile_write"
	case UserNotify:
		return "notify"
	}
	if strings.HasPrefix(id, "forward.") {
		return "forward"
	}
	if strings.HasPrefix(id, "ext.") {
		return "extension"
	}
	if strings.HasPrefix(id, "mcp.") || strings.HasPrefix(id, "cdp.") || strings.HasPrefix(id, "jrpc.") {
		return "peer"
	}
	return ""
}

func AgentRequirement(tool string, args map[string]any) AgentCall {
	text := func(key, fallback string) string {
		v, ok := args[key].(string)
		if !ok {
			return fallback
		}
		return v
	}
	if tool == "term" {
		return AgentCall{ParsedDirectives: true}
	}
	id, _ := IDForTool(tool)
	out := AgentCall{}
	switch tool {
	case "file_replace":
		id = FileWrite
	case "ssh_config":
		action := text("action", "list")
		id = "profile." + action
		if action == "get" {
			out.ProfileArgument = "name"
		}
	case "extension":
		id = "ext." + text("action", "list")
	case "session_start":
		out.ProfileArgument = "ssh_config"
		out.StartsCommand = text("command", "") != ""
	case "shell_detect":
		out.NeedsLocal = true
	}
	out.Function = AgentFunction(id)
	if text("local_path", "") != "" {
		out.NeedsLocal = true
		out.LocalTransfer = tool == "file_read" || tool == "file_write" || tool == "file_replace"
	}
	return out
}
