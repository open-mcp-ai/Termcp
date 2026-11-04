package authorization

// RiskLevel is a display and preset-audit classification. It does not replace
// effect=allow or the operation's typed constraints.
type RiskLevel string

const (
	RiskStandard RiskLevel = "standard"
	RiskElevated RiskLevel = "elevated"
	RiskHigh     RiskLevel = "high"
)

// AccessPath names a user-facing surface that can reach an operation. The MCP
// adapter maps both DSL directives and direct tools to the same operation ID.
type AccessPath string

const (
	AccessDSL      AccessPath = "dsl"
	AccessTool     AccessPath = "tool"
	AccessWebUI    AccessPath = "webui"
	AccessInternal AccessPath = "internal"
)

// ParameterSpec documents values a condition can inspect. Values are enumerable
// only when the runtime has a closed set; dynamic identifiers remain free-form
// strings and are intentionally not presented as a complete enum.
type ParameterSpec struct {
	Kind        ConstraintKind `json:"kind"`
	Values      []string       `json:"values,omitempty"`
	Description string         `json:"description,omitempty"`
}

// Recipe describes a composite request whose execution requires every listed
// operation. Optional requirements are named separately and only appear when
// the effective request uses the corresponding form.
type Recipe struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Requirements []string `json:"requirements"`
	Optional     []string `json:"optional,omitempty"`
}

var recipes = []Recipe{
	{
		ID: "dsl.run", Name: "Run a command line",
		Description:  "Types a line, presses Enter, and reads the requested output window.",
		Requirements: []string{"shell.input", "shell.key", "shell.output"},
	},
	{
		ID: "dsl.connect", Name: "Connect to a host",
		Description:  "Starts a session and opens its first channel.",
		Requirements: []string{"session.start", "shell.open"},
	},
	{
		ID: "dsl.file-copy", Name: "Copy file data",
		Description:  "Reads one endpoint and writes the other.",
		Requirements: []string{"file.read", "file.write"},
	},
}

// Recipes returns the composite request catalog. Callers must not mutate it.
func Recipes() []Recipe { return recipes }

func applyCatalogMetadata() {
	// Metadata is declarative and intentionally separate from grant decisions.
	// The compact one-row-per-action form makes missing reachability easy to spot
	// in review and keeps the executable catalog stable.
	dslOnly := map[string]struct {
		verbs []string
		risk  RiskLevel
	}{
		"alias.list":   {[]string{"ls alias"}, RiskStandard},
		"alias.write":  {[]string{"alias create", "alias edit"}, RiskElevated},
		"alias.delete": {[]string{"alias rm"}, RiskHigh},
		"task.list":    {[]string{"ls task", "--all"}, RiskStandard},
		"task.info":    {[]string{"get task"}, RiskStandard},
		"task.cancel":  {[]string{"close task"}, RiskStandard},
	}
	dsl := map[string][]string{
		"session.list":            {"ls", "ps"},
		"session.info":            {"info", "get"},
		"session.start":           {"connect"},
		"session.terminate":       {"rm session"},
		"session.delete":          {"rm session --force"},
		"shell.list":              {"ls"},
		"shell.open":              {"open"},
		"shell.close":             {"close"},
		"shell.resize":            {"resize"},
		"shell.output":            {"read", "run"},
		"shell.input":             {"input", "run"},
		"shell.key":               {"key", "run"},
		"session.message":         {"span"},
		"shell.reader.register":   {"reader"},
		"shell.reader.unregister": {"reader --drop"},
		"file.read":               {"read file", "get file --op readlink", "copy source"},
		"file.stat":               {"ls file", "get file --op stat"},
		"file.getwd":              {"get file --op getwd"},
		"file.urls":               {"get file --op urls"},
		"file.write":              {"write file", "copy destination"},
		"file.delete":             {"rm file"},
		"file.mkdir":              {"mkdir"},
		"file.perm":               {"write file --mode", "write file --owner", "write file --time"},
		"file.link":               {"get file --op readlink", "write file --op symlink", "write file --op link"},
		"file.fs":                 {"get file --op realpath", "get file --op statvfs", "write file --op truncate"},
		"forward.list":            {"ls forward"},
		"forward.open":            {"fwd", "open forward"},
		"forward.close":           {"close forward", "fwd --action close"},
		"mcp.open":                {"open mcp", "open session mcp", "ls mcp", "get mcp", "close mcp", "read mcp", "call mcp"},
		"cdp.open":                {"open cdp", "ls cdp", "get cdp", "close cdp", "read cdp", "call cdp"},
		"jrpc.open":               {"open jrpc", "ls jrpc", "get jrpc", "close jrpc", "read jrpc", "call jrpc"},
		"profile.list":            {"ls host", "get host"},
		"profile.create":          {"create host"},
		"profile.edit":            {"edit host"},
		"profile.copy":            {"copy host"},
		"profile.delete":          {"rm host"},
		"ext.list":                {"ls ext"},
		"ext.get":                 {"get ext", "open ext", "close ext"},
		"ext.retry":               {"edit ext --restart"},
		"ext.create":              {"create ext"},
		"ext.update":              {"edit ext"},
		"ext.delete":              {"rm ext"},
		"alias.list":              {"ls alias"},
		"alias.write":             {"alias create", "alias edit"},
		"alias.delete":            {"alias rm"},
		"user.notify":             {"notify"},
		"task.list":               {"ls task"},
		"task.info":               {"get task"},
		"task.cancel":             {"close task"},
	}
	tools := map[string][]string{
		"session.list": {"session_list"}, "session.info": {"session_info"},
		"session.start": {"session_start"}, "session.terminate": {"session_terminate"},
		"session.delete": {"session_delete"}, "session.message": {"message"}, "shell.list": {"shell_list"},
		"shell.open": {"shell_open"}, "shell.close": {"shell_close", "shell_archive"},
		"shell.resize": {"shell_resize"},
		"shell.output": {"shell_output"}, "shell.input": {"shell_input"},
		"shell.key": {"shell_key"}, "shell.marks": {"message"},
		"shell.detect": {"shell_detect"}, "shell.reader.register": {"shell_reader_register"},
		"shell.reader.unregister": {"shell_reader_unregister"},
		"file.read":               {"file_read"}, "file.stat": {"file_stat"},
		"file.getwd": {"file_getwd"},
		"file.urls":  {"file_urls"}, "file.write": {"file_write"},
		"file.delete":   {"file_delete"},
		"file.mkdir":    {"file_mkdir"},
		"file.perm":     {"file_perm"},
		"file.link":     {"file_link"},
		"file.fs":       {"file_fs"},
		"file.rename":   {"file_rename"},
		"forward.list":  {"forward"},
		"forward.open":  {"forward"},
		"forward.close": {"forward"}, "profile.list": {"ssh_config"},
		"profile.create": {"ssh_config"},
		"profile.edit":   {"ssh_config"}, "profile.copy": {"ssh_config"},
		"profile.delete": {"ssh_config"}, "ext.list": {"extension"},
		"ext.get": {"extension"}, "ext.retry": {"extension"},
		"ext.create": {"extension"}, "ext.update": {"extension"},
		"ext.delete": {"extension"}, "user.notify": {"notify_user"},
		"mcp.open": {"term"}, "cdp.open": {"term"}, "jrpc.open": {"term"},
	}
	routes := map[string][]string{
		"session.start": {"/api/sessions POST"}, "session.list": {"/api/sessions GET"},
		"session.info": {"/api/sessions/{id} GET"}, "session.terminate": {"/api/sessions/{id} DELETE"},
		"session.delete": {"/api/sessions/{id}/purge DELETE"}, "shell.open": {"/api/sessions/{id}/shells POST"},
		"shell.input": {"/api/shells/{id}/input POST", "/api/ui/ws input"},
		"shell.key":   {"/api/shells/{id}/key POST"}, "shell.output": {"/api/shells/{id}/lines GET"},
		"shell.resize": {"/api/shells/{id}/resize POST", "/api/ui/ws resize"},
		"shell.close":  {"/api/shells/{id} DELETE"}, "forward.list": {"/api/forwards GET"},
		"forward.open": {"/api/forwards POST"}, "forward.close": {"/api/forwards/{id} DELETE"},
		"file.read": {"/api/files GET"}, "file.write": {"/api/files PUT", "/api/files POST"},
		"file.stat": {"/api/files/stat GET"}, "file.delete": {"/api/files DELETE"},
		"file.rename": {"/api/files/patch"}, "file.mkdir": {"/api/files/mkdir POST"},
		"file.urls": {"/api/files/urls GET"}, "file.perm": {"/api/files/patch"},
		"file.link": {"/api/files/links"}, "file.fs": {"/api/files/fs"},
		"profile.list": {"/api/connections GET", "/api/connections/{name} GET"}, "profile.create": {"/api/connections/{name} PUT"},
		"profile.edit": {"/api/connections/{name} PUT"}, "profile.copy": {"/api/connections/batch GET"},
		"profile.delete": {"/api/connections/{name} DELETE"}, "ext.list": {"/api/extensions GET"},
		"ext.get": {"/api/extensions/{name} GET"}, "ext.create": {"/api/extensions POST"},
		"ext.update": {"/api/extensions/{name} PUT"}, "ext.delete": {"/api/extensions/{name} DELETE"},
		"ext.retry": {"/api/extensions/{name}/retry POST"}, "user.notify": {"/api/notify POST"},
	}
	parameters := map[string][]ParameterSpec{
		"shell.key": {
			{Kind: KindKeys, Description: "Effective key names after aliases; values are open-ended."},
			{Kind: KindRepeatMax, Description: "Maximum requested repeat count."},
		},
		"file.read": {
			{Kind: KindForms, Values: []string{"inline", "local", "pipe"}, Description: "local requires a positive length; inline is capped at 8 MiB."},
			{Kind: KindMaxBytes, Description: "Requires a proven positive transfer length or the existing inline cap."},
		},
		"file.write": {
			{Kind: KindForms, Values: []string{"inline", "local", "pipe"}, Description: "Inline counts decoded bytes; local requires a positive length."},
			{Kind: KindMaxBytes, Description: "Requires decoded inline size or a proven transfer length."},
		},
		"mcp.open": {
			{Kind: KindToolNames, Description: "Normalized remote MCP tool name."},
		},
		"cdp.open": {
			{Kind: KindMethods, Description: "Normalized CDP method name."},
		},
		"jrpc.open": {
			{Kind: KindMethods, Description: "Normalized JSON-RPC method name."},
		},
	}
	risk := map[string]RiskLevel{
		"session.delete": RiskElevated,
		"file.write":     RiskElevated,
		"file.delete":    RiskHigh, "file.perm": RiskHigh,
		"file.link": RiskHigh, "file.fs": RiskHigh, "file.rename": RiskHigh,
		"forward.open": RiskHigh,
		"mcp.open":     RiskHigh, "cdp.open": RiskHigh, "jrpc.open": RiskHigh,
		"profile.create": RiskHigh, "profile.edit": RiskHigh, "profile.copy": RiskHigh,
		"profile.delete": RiskHigh, "ext.create": RiskHigh, "ext.update": RiskHigh,
		"ext.delete": RiskHigh, "ext.retry": RiskStandard, "ext.use": RiskElevated,
		"alias.write": RiskElevated, "alias.delete": RiskHigh,
	}
	descriptions := map[string]string{
		"shell.detect":         "Detect the local shell environment used by this Termcp process; it does not inspect the remote target.",
		"shell.exec":           "Start an explicit program or argv in a terminal channel.",
		"shell.input":          "Type text into an existing terminal. The target OS account still decides what that text can do.",
		"file.write":           "Write inline data or transfer from a local path with an explicit byte bound.",
		"file.delete":          "Delete a file or directory in the target account's namespace.",
		"file.truncate":        "Reduce or extend a remote file, which can destroy existing content.",
		"file.chmod":           "Change a remote file mode.",
		"file.chown":           "Change a remote file owner or group.",
		"file.chtimes":         "Change remote file timestamps.",
		"file.symlink":         "Create a symbolic link on the target.",
		"file.link":            "Create a hard link on the target.",
		"forward.open.local":   "Expose a remote destination through a local listener.",
		"forward.open.remote":  "Ask the remote host to listen and forward traffic.",
		"forward.open.dynamic": "Run a loopback-only SOCKS proxy through the target connection.",
		"mcp.tool.invoke":      "Invoke a specific tool on a connected remote MCP server.",
		"mcp.raw":              "Send an unclassified MCP frame; this is broader than a named tool grant.",
		"mcp.open":             "Open or control an MCP peer; tool constraints identify the tools that may run.",
		"cdp.method.invoke":    "Invoke a specific method on a connected CDP endpoint.",
		"cdp.raw":              "Send an unclassified CDP frame; this is broader than a specific method grant.",
		"cdp.open":             "Open or control a CDP peer; method constraints identify the operations that may run.",
		"jrpc.method.invoke":   "Invoke a specific method on a connected JSON-RPC endpoint.",
		"jrpc.raw":             "Send an unclassified JSON-RPC frame; this is broader than a specific method grant.",
		"jrpc.open":            "Open or control a JSON-RPC peer; method constraints identify the operations that may run.",
		"profile.create":       "Create a saved host profile and its connection material.",
		"profile.edit":         "Change a saved profile's destination or credential material.",
		"profile.delete":       "Delete a saved host profile.",
		"ext.create":           "Create an executable extension definition.",
		"ext.update":           "Change an extension executable command, endpoint, allowlist or target.",
		"ext.delete":           "Delete an extension definition and stop its managed instance.",
		"alias.write":          "Change the shared command vocabulary inherited by every Agent.",
		"alias.delete":         "Delete a shared command alias inherited by every Agent.",
		"user.notify":          "Send an operator-facing UI notification and optional session reference.",
		"session.delete":       "Delete a Termcp session record after it has ended.",
		"task.cancel":          "Cancel a transfer task started through Termcp.",
		"mcp.tools.list":       "List the tools exposed by a connected remote MCP server.",
	}
	dependencies := map[string][]string{}
	for id, verbs := range dsl {
		entry := dslOnly[id]
		entry.verbs, entry.risk = verbs, risk[id]
		dslOnly[id] = entry
	}
	for i := range catalog {
		op := &catalog[i]
		if paths := dslOnly[op.ID]; len(paths.verbs) > 0 {
			op.AccessPaths = append(op.AccessPaths, AccessDSL)
			op.DSLVerbs = append([]string(nil), paths.verbs...)
		}
		if names := tools[op.ID]; len(names) > 0 {
			op.AccessPaths = append(op.AccessPaths, AccessTool)
			op.InternalTools = append([]string(nil), names...)
		}
		if names := routes[op.ID]; len(names) > 0 {
			op.AccessPaths = append(op.AccessPaths, AccessWebUI)
			op.WebUIRoutes = append([]string(nil), names...)
		}
		if len(op.AccessPaths) == 0 {
			op.AccessPaths = append(op.AccessPaths, AccessInternal)
		}
		op.Risk = RiskStandard
		if r := risk[op.ID]; r != "" {
			op.Risk = r
		}
		op.Dependencies = append([]string(nil), dependencies[op.ID]...)
		op.Parameters = append([]ParameterSpec(nil), parameters[op.ID]...)
		if description := descriptions[op.ID]; description != "" {
			op.Description = description
		}
	}
}
