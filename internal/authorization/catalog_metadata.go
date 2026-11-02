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
		Optional:     []string{"shell.exec"},
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
		"shell.exec":              {"connect --argv", "open --argv"},
		"shell.resize":            {"resize"},
		"shell.output":            {"read", "run"},
		"shell.input":             {"input", "run"},
		"shell.key":               {"key", "run"},
		"shell.marks":             {"span"},
		"shell.reader.register":   {"reader"},
		"shell.reader.unregister": {"reader --drop"},
		"file.read":               {"read file", "get file --op readlink", "copy source"},
		"file.stat":               {"ls file", "get file --op stat"},
		"file.realpath":           {"get file --op realpath"},
		"file.readlink":           {"get file --op readlink"},
		"file.getwd":              {"get file --op getwd"},
		"file.statvfs":            {"get file --op statvfs"},
		"file.urls":               {"get file --op urls"},
		"file.write":              {"write file", "copy destination"},
		"file.replace":            {"write file --old-text"},
		"file.delete":             {"rm file"},
		"file.mkdir":              {"mkdir"},
		"file.truncate":           {"write file --op truncate"},
		"file.chmod":              {"write file --mode"},
		"file.chown":              {"write file --owner"},
		"file.chtimes":            {"write file --time"},
		"file.symlink":            {"write file --op symlink"},
		"file.link":               {"write file --op link"},
		"forward.list":            {"ls forward"},
		"forward.open.local":      {"fwd --action local", "open forward local"},
		"forward.open.remote":     {"fwd --action remote", "open forward remote"},
		"forward.open.dynamic":    {"fwd --action dynamic", "open forward dynamic"},
		"forward.close":           {"close forward", "fwd --action close"},
		"mcp.open":                {"open mcp", "open session mcp"},
		"mcp.list":                {"ls mcp"},
		"mcp.info":                {"get mcp"},
		"mcp.close":               {"rm mcp", "close mcp"},
		"mcp.read":                {"read mcp", "recv mcp"},
		"mcp.tools.list":          {"get mcp tools", "ls mcp tools"},
		"mcp.tool.invoke":         {"call mcp <tool>"},
		"mcp.raw":                 {"call mcp --send"},
		"cdp.open":                {"open cdp"},
		"cdp.list":                {"ls cdp"},
		"cdp.info":                {"get cdp"},
		"cdp.close":               {"close cdp", "rm cdp"},
		"cdp.read":                {"read cdp", "recv cdp"},
		"cdp.protocol.read":       {"get cdp protocol"},
		"cdp.attach":              {"open cdp --target"},
		"cdp.detach":              {"close cdp <target@"},
		"cdp.method.invoke":       {"call cdp <method>"},
		"cdp.raw":                 {"call cdp --send"},
		"jrpc.open":               {"open jrpc"},
		"jrpc.list":               {"ls jrpc"},
		"jrpc.info":               {"get jrpc"},
		"jrpc.close":              {"close jrpc", "rm jrpc"},
		"jrpc.read":               {"read jrpc", "recv jrpc"},
		"jrpc.method.invoke":      {"call jrpc <method>"},
		"jrpc.raw":                {"call jrpc --send"},
		"profile.list":            {"ls host"},
		"profile.get":             {"get host"},
		"profile.create":          {"create host"},
		"profile.edit":            {"edit host"},
		"profile.copy":            {"copy host"},
		"profile.delete":          {"rm host"},
		"ext.list":                {"ls ext"},
		"ext.get":                 {"get ext"},
		"ext.use":                 {"open ext"},
		"ext.release":             {"close ext"},
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
		"session.delete": {"session_delete"}, "shell.list": {"shell_list"},
		"shell.open": {"shell_open"}, "shell.close": {"shell_close", "shell_archive"},
		"shell.exec": {"session_start", "shell_open"}, "shell.resize": {"shell_resize"},
		"shell.output": {"shell_output"}, "shell.input": {"shell_input"},
		"shell.key": {"shell_key"}, "shell.marks": {"message"},
		"shell.detect": {"shell_detect"}, "shell.reader.register": {"shell_reader_register"},
		"shell.reader.unregister": {"shell_reader_unregister"},
		"file.read":               {"file_read"}, "file.stat": {"file_stat"},
		"file.realpath": {"file_fs"}, "file.readlink": {"file_link"},
		"file.getwd": {"file_getwd"}, "file.statvfs": {"file_fs"},
		"file.urls": {"file_urls"}, "file.write": {"file_write"},
		"file.replace": {"file_replace"}, "file.delete": {"file_delete"},
		"file.mkdir": {"file_mkdir"}, "file.truncate": {"file_fs"},
		"file.chmod": {"file_perm"}, "file.chown": {"file_perm"},
		"file.chtimes": {"file_perm"}, "file.symlink": {"file_link"},
		"file.link": {"file_link"}, "file.rename": {"file_rename"},
		"forward.list": {"forward"}, "forward.open.local": {"forward"},
		"forward.open.remote": {"forward"}, "forward.open.dynamic": {"forward"},
		"forward.close": {"forward"}, "profile.list": {"ssh_config"},
		"profile.get": {"ssh_config"}, "profile.create": {"ssh_config"},
		"profile.edit": {"ssh_config"}, "profile.copy": {"ssh_config"},
		"profile.delete": {"ssh_config"}, "ext.list": {"extension"},
		"ext.get": {"extension"}, "ext.use": {"extension"},
		"ext.release": {"extension"}, "ext.retry": {"extension"},
		"ext.create": {"extension"}, "ext.update": {"extension"},
		"ext.delete": {"extension"}, "user.notify": {"notify_user"},
	}
	routes := map[string][]string{
		"session.start": {"/api/sessions POST"}, "session.list": {"/api/sessions GET"},
		"session.info": {"/api/sessions/{id} GET"}, "session.terminate": {"/api/sessions/{id} DELETE"},
		"session.delete": {"/api/sessions/{id}/purge DELETE"}, "shell.open": {"/api/sessions/{id}/shells POST"},
		"shell.input": {"/api/shells/{id}/input POST", "/api/ui/ws input"},
		"shell.key":   {"/api/shells/{id}/key POST"}, "shell.output": {"/api/shells/{id}/lines GET"},
		"shell.resize": {"/api/shells/{id}/resize POST", "/api/ui/ws resize"},
		"shell.close":  {"/api/shells/{id} DELETE"}, "forward.list": {"/api/forwards GET"},
		"forward.open.local": {"/api/forwards POST"}, "forward.open.remote": {"/api/forwards POST"},
		"forward.open.dynamic": {"/api/forwards POST"}, "forward.close": {"/api/forwards/{id} DELETE"},
		"file.read": {"/api/files GET"}, "file.write": {"/api/files PUT", "/api/files POST"},
		"file.stat": {"/api/files/stat GET"}, "file.delete": {"/api/files DELETE"},
		"file.rename": {"/api/files/patch"}, "file.mkdir": {"/api/files/mkdir POST"},
		"file.urls": {"/api/files/urls GET"}, "profile.list": {"/api/connections GET"},
		"profile.get": {"/api/connections/{name} GET"}, "profile.create": {"/api/connections/{name} PUT"},
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
		"file.replace": {
			{Kind: KindForms, Values: []string{"inline"}, Description: "Replacement is always classified as inline."},
		},
		"mcp.tool.invoke": {
			{Kind: KindToolNames, Description: "Normalized remote MCP tool name."},
		},
		"cdp.method.invoke": {
			{Kind: KindMethods, Description: "Normalized CDP method name."},
		},
		"jrpc.method.invoke": {
			{Kind: KindMethods, Description: "Normalized JSON-RPC method name."},
		},
	}
	risk := map[string]RiskLevel{
		"session.delete": RiskElevated, "shell.input": RiskHigh, "shell.key": RiskHigh,
		"shell.exec": RiskHigh, "file.write": RiskElevated, "file.replace": RiskElevated,
		"file.delete": RiskHigh, "file.truncate": RiskHigh, "file.chmod": RiskHigh,
		"file.chown": RiskHigh, "file.chtimes": RiskHigh, "file.symlink": RiskHigh,
		"file.link": RiskHigh, "file.rename": RiskHigh, "forward.open.local": RiskHigh,
		"forward.open.remote": RiskHigh, "forward.open.dynamic": RiskHigh,
		"mcp.open": RiskHigh, "mcp.tool.invoke": RiskHigh, "mcp.raw": RiskHigh,
		"cdp.open": RiskHigh, "cdp.attach": RiskHigh, "cdp.detach": RiskHigh,
		"cdp.method.invoke": RiskHigh, "cdp.raw": RiskHigh, "jrpc.open": RiskHigh,
		"jrpc.method.invoke": RiskHigh, "jrpc.raw": RiskHigh,
		"profile.create": RiskHigh, "profile.edit": RiskHigh, "profile.copy": RiskHigh,
		"profile.delete": RiskHigh, "ext.create": RiskHigh, "ext.update": RiskHigh,
		"ext.delete": RiskHigh, "ext.retry": RiskElevated, "ext.use": RiskElevated,
		"alias.write": RiskElevated, "alias.delete": RiskHigh,
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
	}
}
