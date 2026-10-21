// Package capability is the one table every surface asks admission questions
// of: may this caller reach the session, may this operation run against it, and
// does a human have to decide first.
//
// Before this package the same three questions were re-derived in five places —
// internal/mcp's toolGuard, its gatedTools list, internal/webui's
// restRefuseTool call sites, the WebSocket refusal, and two
// `if shell.RequiresApproval()` lines inside shell handlers. Each held its own
// spelling of the same operation, so adding a reviewable operation meant editing
// a table AND remembering to guard the handler, and "the human's own keyboard is
// not reviewed" held only because twelve call sites each happened not to write
// the check.
//
// A capability row is the whole answer for one operation: its id (what a
// restriction's allow list names), its approval kind (what review records), the
// arguments that carry a path (what the paths whitelist scopes), its gate (who
// must decide), and whether repeating it is safe. Surfaces map their own
// vocabulary — an MCP tool name, an HTTP route — onto an id and then ask here.
//
// The zero-value answer for an unknown name is the closed one only where the
// caller chose a whitelist: an ordinary session carries an unrestricted
// Restriction, so an operation nobody registered still cannot be reached by a
// session that listed nothing.
package capability

import "github.com/open-mcp-ai/termcp/internal/approval"

// Gate says who has to decide before an operation runs.
type Gate int

const (
	// GateOperator is the operator's own surface: the browser terminal, the
	// Web UI's file browser, the REST API with a deployment token. It is never
	// reviewed. That is not an exemption bolted on — review exists to catch an
	// agent doing something no human looked at, and asking the human to approve
	// their own keystroke locks them out of the session they are watching.
	GateOperator Gate = iota

	// GateReview holds the operation for N-person approval. Used for anything
	// that changes the host in a way nobody may be watching: file writes,
	// port forwards, and — the case this gate exists for — installing an
	// extension or a profile, because that changes the capability list itself.
	GateReview

	// GateDevice is "the device was assigned, so its use is authorised". Running
	// a command in an assigned session — including `curl | sh` — is not
	// something a rule can judge: what that command installs could be anything,
	// so it is authorised by the assignment ("this session may act as this
	// host's operator"), not by the command. Review still covers the acts that
	// grow the capability list (see GateReview), which is the boundary a human
	// can actually see and decide.
	GateDevice
)

// Capability is one operation, described once.
type Capability struct {
	// ID is the capability's name in a Restriction's allow list, e.g.
	// "shell.input" or "file.write".
	ID string
	// Kind is what review mode records on a queued request. The zero value
	// means the operation is not reviewable at all.
	Kind approval.Kind
	// PathArgs names the arguments that carry a filesystem path, so a session's
	// paths whitelist can scope them. Each capability declares its own rather
	// than sharing one global list: a read-only skill's paths matter for
	// file.read and file.stat, and nowhere else.
	PathArgs []string
	// Gate says who must decide before the operation runs.
	Gate Gate
	// Idempotent reports whether repeating the operation is safe. It decides the
	// envelope's Idempotent header, which only ever appears on a failure — it
	// answers "may I retry" and nothing else.
	Idempotent bool
	// Staged marks a reviewable operation whose unit of review is completed by a
	// LATER call, so the gate has to live in the operation's own write path
	// instead of queueing this call as a whole: terminal input is a line, and a
	// line is text (shell.input) plus the key that ends it (shell.key). Queueing
	// either half alone would ask for two decisions on one command and let them
	// disagree.
	//
	// It is a field and not a name check because that is exactly the pattern this
	// table exists to remove: "which operations are special" is one fact, written
	// once, next to the row it describes.
	Staged bool
}

// Reviewable reports whether review mode can hold this capability. Only a
// capability that has an approval Kind is queued; the rest are admitted or
// refused and then run.
func (c Capability) Reviewable() bool { return c.Kind != "" }

// table is the capability list. One row per operation; a row is the only place
// any of these five facts is written down.
//
// The Gate column reads as the boundary the design is built on: use of an
// assigned device is GateDevice (run a command, open a channel, send a key),
// growing the capability list is GateReview (install an extension, create a
// profile, write a file, open a tunnel), and the operator's own browser REST and
// WebSocket surfaces are GateOperator because they are the human acting.
var table = []Capability{
	// --- Sessions and connections ---
	{ID: "session.start", Gate: GateDevice},
	{ID: "session.list", Gate: GateOperator, Idempotent: true},
	{ID: "session.info", Gate: GateOperator, Idempotent: true},
	{ID: "session.terminate", Gate: GateDevice},
	{ID: "session.delete", Gate: GateDevice},

	// --- Terminal channels ---
	{ID: "shell.open", Gate: GateDevice},
	{ID: "shell.list", Gate: GateOperator, Idempotent: true},
	{ID: "shell.close", Gate: GateDevice},
	{ID: "shell.input", Kind: approval.KindShellInput, Gate: GateReview, Staged: true},
	{ID: "shell.key", Kind: approval.KindShellInput, Gate: GateReview, Staged: true},
	{ID: "shell.output", Gate: GateOperator, Idempotent: true},
	{ID: "shell.resize", Gate: GateDevice},
	{ID: "shell.detect", Gate: GateOperator, Idempotent: true},
	{ID: "shell.reader.register", Gate: GateOperator, Idempotent: true},
	{ID: "shell.reader.unregister", Gate: GateOperator, Idempotent: true},
	{ID: "shell.notify", Gate: GateOperator, Idempotent: true},
	{ID: "session.message", Gate: GateOperator, Idempotent: true},

	// --- Human-facing ---
	{ID: "user.notify", Gate: GateOperator, Idempotent: true},

	// --- Port forwards ---
	{ID: "forward.open", Kind: approval.KindForwardOpen, Gate: GateReview},
	{ID: "forward.close", Kind: approval.KindForwardClose, Gate: GateReview},
	{ID: "forward.list", Gate: GateOperator, Idempotent: true},

	// --- Files ---
	{ID: "file.read", PathArgs: []string{"remote_path", "local_path"}, Gate: GateOperator, Idempotent: true},
	{ID: "file.write", PathArgs: []string{"remote_path", "local_path"}, Kind: approval.KindFileWrite, Gate: GateReview},
	{ID: "file.stat", PathArgs: []string{"remote_path"}, Gate: GateOperator, Idempotent: true},
	{ID: "file.delete", PathArgs: []string{"remote_path"}, Kind: approval.KindFileDelete, Gate: GateReview},
	{ID: "file.rename", PathArgs: []string{"from_path", "to_path"}, Kind: approval.KindFileRename, Gate: GateReview},
	{ID: "file.mkdir", PathArgs: []string{"remote_path"}, Kind: approval.KindFileMkdir, Gate: GateReview},
	{ID: "file.urls", PathArgs: []string{"remote_path"}, Gate: GateOperator, Idempotent: true},
	{ID: "file.perm", PathArgs: []string{"remote_path"}, Kind: approval.KindFilePerm, Gate: GateReview},
	{ID: "file.link",
		PathArgs: []string{"remote_path", "target", "link_path", "existing_path", "new_path"},
		Kind:     approval.KindFileLink, Gate: GateReview},
	{ID: "file.fs", PathArgs: []string{"remote_path"}, Kind: approval.KindFileTruncate, Gate: GateReview},
	{ID: "file.getwd", Gate: GateOperator, Idempotent: true},

	// --- Profiles and extensions: these change WHAT the agent can reach, which
	// is the one thing a human must decide. Their content is visible in the
	// queue (command or url, allow, paths, forwards), so whoever approves sees
	// the whole template rather than a name.
	{ID: "profile.list", Gate: GateOperator, Idempotent: true},
	{ID: "profile.create", Kind: approval.KindProfileCreate, Gate: GateReview},
	{ID: "profile.edit", Kind: approval.KindProfileEdit, Gate: GateReview},
	{ID: "profile.copy", Kind: approval.KindProfileCreate, Gate: GateReview},
	{ID: "profile.delete", Gate: GateDevice},
	{ID: "ext.list", Gate: GateOperator, Idempotent: true},
	{ID: "ext.get", Gate: GateOperator, Idempotent: true},
	{ID: "ext.create", Kind: approval.KindExtensionCreate, Gate: GateReview},
	{ID: "ext.update", Kind: approval.KindExtensionUpdate, Gate: GateReview},
	{ID: "ext.delete", Gate: GateDevice},
	{ID: "ext.retry", Gate: GateDevice},

	// --- Frame channels: reaching a peer this process did not start ---
	//
	// GateReview, not GateDevice, and for the reason ext.create carries it: what
	// the agent gains is not a command in a host it was already assigned, it is a
	// NEW peer — an MCP server, a browser, an arbitrary socket — with whatever
	// that peer can do. That is a change to the capability list, which is the one
	// thing the review gate exists for.
	//
	// This row is the half of the fix that does not depend on issue #128: it makes
	// the door nameable. Whether a given call is HELD is the hook layer's decision
	// (the gate is read there), so nothing here queues anything yet.
	{ID: "jrpc.open", Kind: approval.KindExtensionCreate, Gate: GateReview},
	{ID: "mcp.open", Kind: approval.KindExtensionCreate, Gate: GateReview},
	{ID: "cdp.open", Kind: approval.KindExtensionCreate, Gate: GateReview},
}

// byID indexes table. Built once at init so a lookup on the hot path — every
// guarded call, every REST refusal — is a map read.
var byID = func() map[string]Capability {
	m := make(map[string]Capability, len(table))
	for _, c := range table {
		if _, dup := m[c.ID]; dup {
			panic("capability: duplicate id " + c.ID)
		}
		m[c.ID] = c
	}
	return m
}()

// Lookup returns the capability with this id. The second result is false for a
// name this build does not know, which is how a whitelist stays closed: an
// entry nobody registered cannot authorise anything.
func Lookup(id string) (Capability, bool) {
	c, ok := byID[id]
	return c, ok
}

// All returns every capability, in table order. Callers must not mutate it.
func All() []Capability { return table }

// IDForTool maps a tool name onto its capability id.
//
// The tool surface keeps its own vocabulary on purpose — a tool name is what a
// model calls, and `file_write` says more to a model than `file.write` — so the
// translation lives here, in one switch, rather than as a parallel table three
// packages each hold a copy of.
func IDForTool(tool string) (string, bool) {
	id, ok := toolIDs[tool]
	return id, ok
}

// ForTool is Lookup(IDForTool(tool)).
func ForTool(tool string) (Capability, bool) {
	id, ok := toolIDs[tool]
	if !ok {
		return Capability{}, false
	}
	c, ok := byID[id]
	return c, ok
}

// ToolForID reverses IDForTool: the MCP tool name a capability id belongs to.
// It exists so a whitelist can compare on either spelling. Two tools can share
// one capability (the human-notify route and the tool), in which case the first
// one registered wins — the mapping is only used to match a legacy template's
// entry, and any tool name in the same capability names the same operation.
func ToolForID(id string) string {
	if t, ok := toolForID[id]; ok {
		return t
	}
	return id
}

// IDForRoute maps an HTTP route shape onto its capability id. The Web UI's REST
// surface is the operator's own: it enforces a template's whitelist (an agent
// handed an extension handle can reach the REST API with the same deployment
// token) but is never reviewed.
func IDForRoute(route string) (string, bool) {
	id, ok := routeIDs[route]
	return id, ok
}

// ID constants for the capability ids the surfaces name. A surface holds one of
// these instead of a bare string, so `internal/webui` and `internal/mcp` never
// spell an operation's name twice: the table above is the only place the
// spelling exists, and a rename is one edit that the compiler checks.
const (
	SessionStart     = "session.start"
	SessionList      = "session.list"
	SessionInfo      = "session.info"
	SessionTerminate = "session.terminate"
	SessionDelete    = "session.delete"
	SessionMessage   = "session.message"

	ShellOpen             = "shell.open"
	ShellList             = "shell.list"
	ShellClose            = "shell.close"
	ShellInput            = "shell.input"
	ShellKey              = "shell.key"
	ShellOutput           = "shell.output"
	ShellResize           = "shell.resize"
	ShellDetect           = "shell.detect"
	ShellReaderRegister   = "shell.reader.register"
	ShellReaderUnregister = "shell.reader.unregister"
	ShellNotify           = "shell.notify"

	UserNotify = "user.notify"

	ForwardOpen  = "forward.open"
	ForwardClose = "forward.close"
	ForwardList  = "forward.list"

	FileRead   = "file.read"
	FileWrite  = "file.write"
	FileStat   = "file.stat"
	FileDelete = "file.delete"
	FileRename = "file.rename"
	FileMkdir  = "file.mkdir"
	FileURLs   = "file.urls"
	FilePerm   = "file.perm"
	FileLink   = "file.link"
	FileFs     = "file.fs"
	FileGetwd  = "file.getwd"

	ProfileList   = "profile.list"
	ProfileCreate = "profile.create"
	ProfileEdit   = "profile.edit"
	ProfileCopy   = "profile.copy"
	ProfileDelete = "profile.delete"

	ExtList   = "ext.list"
	ExtGet    = "ext.get"
	ExtCreate = "ext.create"
	ExtUpdate = "ext.update"
	ExtDelete = "ext.delete"
	ExtRetry  = "ext.retry"

	// The frame-channel doors. They are one capability each rather than one per
	// subcommand: `open` is what reaches a new peer, and a caller that may open
	// has already been granted the reach the rest of the subcommands exercise.
	//
	// They are rows at all so a whitelist can NAME them. Before this, a session
	// whose template listed nothing still reached `mcp open` — the directive's
	// target is a URL, so the session-scoped guard found no session to ask and
	// admitted the call. "The operation nobody registered cannot be reached by a
	// restricted session" only holds for operations that HAVE a row.
	JrpcOpen = "jrpc.open"
	McpOpen  = "mcp.open"
	CdpOpen  = "cdp.open"
)

// toolIDs is the tool name → capability id map. One line per tool; a tool
// missing here is unreachable from a restricted session, which is the correct
// failure direction.
var toolIDs = map[string]string{
	"session_start":           "session.start",
	"session_list":            "session.list",
	"session_info":            "session.info",
	"session_terminate":       "session.terminate",
	"session_delete":          "session.delete",
	"shell_open":              "shell.open",
	"shell_list":              "shell.list",
	"shell_close":             "shell.close",
	"shell_input":             "shell.input",
	"shell_key":               "shell.key",
	"shell_output":            "shell.output",
	"shell_resize":            "shell.resize",
	"shell_detect":            "shell.detect",
	"shell_reader_register":   "shell.reader.register",
	"shell_reader_unregister": "shell.reader.unregister",
	"shell_notify":            "shell.notify",
	"message":                 "session.message",
	"notify_user":             "user.notify",
	"forward":                 "forward.open",
	"file_read":               "file.read",
	"file_write":              "file.write",
	"file_stat":               "file.stat",
	"file_delete":             "file.delete",
	"file_rename":             "file.rename",
	"file_mkdir":              "file.mkdir",
	"file_urls":               "file.urls",
	"file_perm":               "file.perm",
	"file_link":               "file.link",
	"file_fs":                 "file.fs",
	"file_getwd":              "file.getwd",
	"ssh_config":              "profile.list",
	"extension":               "ext.list",
}

// toolForID is the reverse index, so a whitelist entry written as a tool name and
// a check made on a capability id meet in the middle.
var toolForID = func() map[string]string {
	m := make(map[string]string, len(toolIDs))
	for tool, id := range toolIDs {
		if _, seen := m[id]; !seen {
			m[id] = tool
		}
	}
	return m
}()

// routeIDs is the REST/WebSocket route → capability id map, keyed by the
// resource shape rather than the full path so one entry covers every id.
var routeIDs = map[string]string{
	"session.start":  "session.start",
	"session.list":   "session.list",
	"session.byID":   "session.info",
	"session.delete": "session.delete",
	"session.stop":   "session.terminate",
	"session.input":  "shell.input",
	"session.resize": "shell.resize",
	"shell.open":     "shell.open",
	"shell.byID":     "shell.output",
	"shell.read":     "shell.output",
	"shell.input":    "shell.input",
	"shell.resize":   "shell.resize",
	"shell.close":    "shell.close",
	"forward.open":   "forward.open",
	"forward.list":   "forward.list",
	"forward.close":  "forward.close",
	"notify.list":    "shell.notify",
	"notify.delete":  "shell.notify",
	"notify.create":  "user.notify",
	"file.read":      "file.read",
	"file.write":     "file.write",
	"file.stat":      "file.stat",
	"file.delete":    "file.delete",
	"file.rename":    "file.rename",
	"file.mkdir":     "file.mkdir",
	"file.urls":      "file.urls",
	"file.marks":     "shell.output",
}
