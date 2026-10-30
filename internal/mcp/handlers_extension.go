package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// extensionActions is the action enum the extension tool dispatches on. It is
// restated here rather than derived from the reconciler because the tool's
// surface is a decision about what an agent may do, not about what the
// reconciler can do.
//
// restart is retry's other name, and the one a caller looks for: the action rebuilds
// a session, which is what "restart" means and what "retry" only implies. Both are
// accepted so neither entrance has to know which word the other one chose.
var extensionActions = []string{"list", "get", "use", "release", "create", "update", "set", "delete", "retry", "restart"}

// handleExtensionOps is the whole extension surface in one tool, after the
// ssh_config pattern: an agent lists, reads one template's detail (including
// the notes that are its operating instructions), authors and edits templates,
// and nudges a stuck one back onto its feet. The sessions themselves are
// ordinary sessions — this tool never opens one, it only hands out the
// reconciler's handles.
func (s *Server) handleExtensionOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.extensions == nil {
		return toolError(CodeNotConfigured, "%s", "extensions are not configured on this termcp"), nil
	}
	args := request.GetArguments()
	switch getString(args, "action", "") {
	case "list":
		return s.handleExtensionList(ctx, request)
	case "get":
		return s.handleExtensionGet(ctx, request)
	case "use":
		return s.handleExtensionUse(ctx, request)
	case "release":
		return s.handleExtensionRelease(ctx, request)
	case "create":
		return s.handleExtensionCreate(ctx, request)
	case "update":
		return s.handleExtensionUpdate(ctx, request)
	case "set":
		return s.handleExtensionSet(ctx, request)
	case "delete":
		return s.handleExtensionDelete(ctx, request)
	case "retry", "restart":
		return s.handleExtensionRetry(ctx, request)
	default:
		return toolError(CodeInvalidArgument, "action must be one of %s", strings.Join(extensionActions, ", ")), nil
	}
}

// handleExtensionList is the discovery call: every extension, its status, and
// the handles an available one is driven through. Notes are deliberately absent
// — they can be long, and an agent that wants them reads them with get.
func (s *Server) handleExtensionList(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	names, err := s.extensions.Store().List()
	if err != nil {
		return toolError(CodeOperationFailed, "listing extensions: %s", err), nil
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if p, restricted, err := auth.PolicyFromContext(ctx); restricted && (err != nil || p.IsV2() && !s.agentExtensionAllowed(p, name)) {
			continue
		}
		ext, err := s.extensions.Store().Load(name)
		st, _ := s.extensions.Status(name)
		entry := map[string]any{"name": name}
		if err == nil {
			entry["name"] = ext.Name
			entry["target"] = ext.Target
			if targets := ext.Targets; len(targets) > 0 {
				entry["targets"] = targets
			}
			entry["description"] = ext.Description
			entry["enabled"] = ext.IsEnabled()
		} else if missing, mErr := s.extensions.Store().Missing(name); mErr == nil && len(missing) > 0 {
			// AN INSTALLED TEMPLATE WITH UNFILLED HOLES IS NOT A BROKEN ONE. It is
			// waiting for this installation's values, which a caller can act on — and the
			// difference between "needs targets" and "cannot parse" is the difference
			// between filling something in and repairing a file.
			entry["needs"] = missing
		}
		// A file that no longer parses is still listed, with the failure the
		// reconciler recorded on it: the status explains the rest.
		s.extensionRuntimeFields(entry, st, false)
		out = append(out, entry)
	}
	b, err := json.Marshal(map[string]any{"extensions": out})
	if err != nil {
		return toolError(CodeInternalError, "encoding extension list: %s", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// handleExtensionGet returns one template in full: the session shape, the
// capability whitelist, the declared forwards, the failure history, and the
// notes an available extension is meant to be driven by.
func (s *Server) handleExtensionGet(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	// GET SERVES THE HOLES TOO, not only the rendered extension: an agent that has to
	// fill in a value needs to see what is being asked for, and a template whose holes
	// are unfilled has nothing to render yet.
	ext, err := s.extensions.Store().Load(name)
	if err != nil {
		if missing, mErr := s.extensions.Store().Missing(name); mErr == nil && len(missing) > 0 {
			holes, _ := s.extensions.Store().Holes(name)
			b, _ := json.Marshal(map[string]any{"name": name, "needs": missing, "holes": holes})
			return mcpgo.NewToolResultText(string(b)), nil
		}
		return toolError(CodeRuleNotFound, "extension %q not found", name), nil
	}
	entry := agentExtensionConfigMap(ctx, ext)
	if holes, hErr := s.extensions.Store().Holes(name); hErr == nil && len(holes) > 0 {
		entry["holes"] = holes
	}
	st, _ := s.extensions.Status(name)
	s.extensionRuntimeFields(entry, st, true)
	b, err := json.Marshal(entry)
	if err != nil {
		return toolError(CodeInternalError, "encoding extension: %s", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// handleExtensionCreate stores a template document or authors one from its fields.
// A template's declared shape is its behavior; an unfilled package can be installed
// first and completed later with action=set.
func (s *Server) handleExtensionCreate(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	// A name whose template still loads is a conflict. One whose template no longer
	// loads is not: create overwrites it, which is the documented way out of a
	// broken file (see Store.NameState), and a name that cannot be stored at all
	// falls through to the save's own validation.
	if taken, loadable := s.extensions.Store().NameState(name); taken && loadable {
		return toolError(CodeConflict, "extension %q already exists; use action=update to change it", name), nil
	}
	// A DOCUMENT PLUS VALUES, or the field arguments — one create covers both. A
	// package is installed as `template` (the TOML a repository ships, holes and all)
	// plus the `values` this installation fills in; a one-off is created by naming
	// fields. Neither is a special case of the other, so both are accepted and the
	// document wins when it is present.
	if doc, ok := templateArg(args); ok {
		return s.createExtensionFromTemplate(name, doc, valuesArg(args))
	}
	ext := extensions.Extension{}
	ext.Name = name
	if err := extensions.ApplyArgs(&ext, args, false); err != nil {
		return extensionSaveError(err), nil
	}
	if err := extensions.SaveStruct(s.extensions.Store(), &ext); err != nil {
		return extensionSaveError(err), nil
	}
	return extensionSaved(ctx, s, &ext)
}

// handleExtensionUpdate patches a stored template. An omitted field keeps its
// stored value; a field that is present but empty clears it. That distinction
// is what makes "trim the path list" and "leave the path list alone" both
// expressible in one call.
func (s *Server) handleExtensionUpdate(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	if doc, ok := templateArg(args); ok {
		if err := s.extensions.Store().SaveTemplate(name, []byte(doc)); err != nil {
			return extensionSaveError(err), nil
		}
		if set := valuesArg(args); len(set) > 0 {
			if err := s.extensions.Store().SetValues(name, set); err != nil {
				return extensionSaveError(err), nil
			}
		}
		return s.extensionAfterWrite(name)
	}
	ext, err := s.extensions.Store().Load(name)
	if err != nil {
		if missing, mErr := s.extensions.Store().Missing(name); mErr == nil && len(missing) > 0 {
			return toolError(CodeConflict,
				"extension %q is installed but still needs %s: send action=set with values, or action=update with a template",
				name, strings.Join(missing, ", ")), nil
		}
		return toolError(CodeRuleNotFound, "extension %q not found", name), nil
	}
	if err := extensions.ApplyArgs(ext, args, true); err != nil {
		return extensionSaveError(err), nil
	}
	if err := extensions.SaveStruct(s.extensions.Store(), ext); err != nil {
		return extensionSaveError(err), nil
	}
	return extensionSaved(ctx, s, ext)
}

// handleExtensionSet fills the holes a template asks for.
//
// THIS IS THE INSTALLATION STEP. A package arrives with a recipe and no values; this
// call is what turns it into an extension on this machine. The values are keyed by
// hole name, so a caller never has to know where or how they are stored — and a value
// for a hole the template does not have is refused rather than filed away where
// nothing will read it.
func (s *Server) handleExtensionSet(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	set := valuesArg(args)
	if len(set) == 0 {
		return toolError(CodeInvalidArgument,
			"set needs values: send a `values` object keyed by hole name (action=get reports what this extension asks for)"), nil
	}
	if err := s.extensions.Store().SetValues(name, set); err != nil {
		var notApplied *extensions.NotApplied
		if !errors.As(err, &notApplied) {
			return extensionSaveError(err), nil
		}
	}
	return s.extensionAfterWrite(name)
}

// createExtensionFromTemplate installs a document: the template first, then the
// values. It does NOT fail when the values do not cover every hole — that is the
// installed-but-unset state a caller is expected to finish, and the answer says which
// holes remain.
func (s *Server) createExtensionFromTemplate(name, doc string, values map[string]any) (*mcpgo.CallToolResult, error) {
	if err := s.extensions.Store().SaveTemplate(name, []byte(doc)); err != nil {
		return extensionSaveError(err), nil
	}
	if len(values) > 0 {
		if err := s.extensions.Store().SetValues(name, values); err != nil {
			return extensionSaveError(err), nil
		}
	}
	return s.extensionAfterWrite(name)
}

// extensionAfterWrite answers with the extension as it now stands, or with what it
// still needs. A write that leaves holes unfilled has SUCCEEDED — the recipe is
// stored — so reporting a failure would be wrong, and reporting plain success would
// leave the caller thinking it can be used.
func (s *Server) extensionAfterWrite(name string) (*mcpgo.CallToolResult, error) {
	holes, _ := s.extensions.Store().Holes(name)
	missing, _ := s.extensions.Store().Missing(name)
	ext, err := s.extensions.Store().Load(name)
	if err != nil {
		entry := map[string]any{"name": name, "stored": true, "holes": holes}
		if len(missing) > 0 {
			entry["needs"] = missing
			entry["next"] = "fill them with action=set values={...}"
		} else {
			entry["error"] = err.Error()
		}
		b, _ := json.Marshal(entry)
		return mcpgo.NewToolResultText(string(b)), nil
	}
	entry := extensionConfigMap(ext)
	entry["holes"] = holes
	st, _ := s.extensions.Status(name)
	s.extensionRuntimeFields(entry, st, false)
	b, err := json.Marshal(entry)
	if err != nil {
		return toolError(CodeInternalError, "encoding extension: %s", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// templateArg reads a TOML template document from the arguments.
func templateArg(args map[string]any) (string, bool) {
	v, ok := args["template"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}

// valuesArg reads the hole values a caller is setting.
func valuesArg(args map[string]any) map[string]any {
	v, ok := args["values"]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// handleExtensionDelete removes a template and, with it, the session the
// reconciler kept warm for it.
func (s *Server) handleExtensionDelete(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	if _, err := s.extensions.Store().Load(name); err != nil {
		return toolError(CodeRuleNotFound, "extension %q not found", name), nil
	}
	if err := s.extensions.Store().Delete(name); err != nil {
		return toolError(CodeOperationFailed, "deleting extension %q: %s", name, err), nil
	}
	return successResult(), nil
}

// handleExtensionUse is the ask: bring an on-demand extension up and hand back
// the handles. It is the only action with a lifecycle side effect, which is why
// it is its own word rather than something `get` does behind the caller's back —
// a read that dials a host is a surprise, and the caller that wants it says so.
//
// The template is returned with the runtime half, so one call gives the caller
// everything `get` would plus the instance: the notes are the operating
// instructions, and the handles are what they are instructions for.
func (s *Server) handleExtensionUse(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	ext, err := s.extensions.Store().Load(name)
	if err != nil {
		return toolError(CodeRuleNotFound, "extension %q not found", name), nil
	}
	// The wait is a bound on the caller's patience, not on the dial: a host that is
	// slow to answer returns `probing` with what the caller should poll, rather
	// than a failure it would have to guess was not one.
	target := strings.TrimSpace(getString(args, "target", ""))
	st, err := s.extensions.AcquireTarget(name, target, extensionUseBudget)
	if err != nil {
		return toolError(CodeOperationFailed, "starting extension %q: %s", name, err), nil
	}
	entry := agentExtensionConfigMap(ctx, ext)
	if target != "" {
		entry["target"] = target
	}
	s.extensionRuntimeFields(entry, st, true)
	if st.Status == extensions.StatusIdle {
		entry["status"] = extensions.StatusAvailable
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return toolError(CodeInternalError, "encoding extension: %s", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// handleExtensionRelease hands an instance back: the explicit half of the idle
// window (issue #183). shell_id names which instance, and leaving it out releases
// every instance of the extension — which is what a caller that did not keep the
// handle means by "I am done with this".
//
// Releasing something that is not running is an ERROR rather than a quiet success:
// a caller that thinks it gave an instance back, and did not, is a caller that will
// not look again.
func (s *Server) handleExtensionRelease(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	target := strings.TrimSpace(getString(args, "target", ""))
	shellID := strings.TrimSpace(getString(args, "shell_id", ""))
	released, err := s.extensions.ReleaseTarget(name, target, shellID)
	if err != nil {
		return toolError(CodeRuleNotFound, "releasing extension %q: %s", name, err), nil
	}
	if released == 0 {
		return toolError(CodeConflict,
			"extension %q has no instance to release%s", name,
			" (it was already idle, or another caller's silence closed it)"), nil
	}
	b, err := json.Marshal(map[string]any{"name": name, "released": released})
	if err != nil {
		return toolError(CodeInternalError, "encoding release: %s", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// extensionUseBudget bounds how long action=use waits for an answer. Long enough
// for a cold dial to a distant host, short enough that a wedged one is reported
// as `probing` rather than held open forever.
const extensionUseBudget = 30 * time.Second

// handleExtensionRetry asks the reconciler to bring an extension up now rather
// than waiting out its backoff. Both an agent and a human can reach for it, so
// a stuck extension is one call away from being re-probed.
func (s *Server) handleExtensionRetry(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	if _, err := s.extensions.Store().Load(name); err != nil {
		return toolError(CodeRuleNotFound, "extension %q not found", name), nil
	}
	if err := s.extensions.RetryTarget(name, strings.TrimSpace(getString(args, "target", ""))); err != nil {
		return toolError(CodeOperationFailed, "retrying extension %q: %s", name, err), nil
	}
	return successResult(), nil
}

/* The authoring layer — ApplyArgs, SaveTemplate, ParseForwardDecls — lives in
   internal/extensions now, so the WebUI's create/update routes apply an author's
   arguments through the same code this tool does. A rule for what makes a
   template valid therefore has one home and two callers. */

func extensionSaveError(err error) *mcpgo.CallToolResult {
	return toolError(CodeInvalidArgument, "extension not saved: %s", err)
}

func extensionSaved(ctx context.Context, s *Server, ext *extensions.Extension) (*mcpgo.CallToolResult, error) {
	// The store's own change subscription triggers the reconciler, so the
	// session is coming up; the returned template is what was persisted.
	st, _ := s.extensions.Status(ext.Name)
	entry := agentExtensionConfigMap(ctx, ext)
	status, next := extensionStatusWord(ext.Name, st, ext.IsEnabled())
	entry["status"] = status
	if next != "" {
		entry["next"] = next
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return toolError(CodeInternalError, "encoding extension: %s", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// extensionConfigMap renders a template with the same key names the tool's
// arguments use, so what an agent reads from get is what it would send to
// update.
func extensionConfigMap(ext *extensions.Extension) map[string]any {
	entry := map[string]any{
		"name":        ext.Name,
		"target":      ext.Target,
		"description": ext.Description,
		"enabled":     ext.IsEnabled(),
		"mode":        ext.Mode,
		"command":     ext.Command,
		"allow":       ext.Allow,
		"paths":       ext.Paths,
		"notes":       ext.Notes,
	}
	if ext.URL != "" {
		entry["url"] = ext.URL
	}
	if len(ext.Headers) > 0 {
		entry["headers"] = ext.Headers
	}
	if len(ext.Args) > 0 {
		entry["args"] = ext.Args
	}
	if ext.Rows > 0 {
		entry["rows"] = ext.Rows
	}
	if ext.Cols > 0 {
		entry["cols"] = ext.Cols
	}
	if len(ext.Forwards) > 0 {
		entry["forwards"] = ext.Forwards
	}
	if len(ext.Targets) > 0 {
		entry["targets"] = ext.Targets
	}
	return entry
}

// Agent discovery exposes instructions and capability metadata, never the
// transport credentials or command arguments used to start the extension.
func agentExtensionConfigMap(ctx context.Context, ext *extensions.Extension) map[string]any {
	if _, restricted, _ := auth.PolicyFromContext(ctx); !restricted {
		return extensionConfigMap(ext)
	}
	return map[string]any{"name": ext.Name, "target": ext.Target, "description": ext.Description, "enabled": ext.IsEnabled(), "mode": ext.Mode, "allow": ext.Allow, "paths": ext.Paths, "notes": ext.Notes}
}

// extensionStatusWord is the word a reply prints for a template's state.
//
// `unavailable` IS TWO DIFFERENT PIECES OF NEWS (#159 D3). It is the reconciler's
// initial state — "nothing has proved this works yet" — and also its terminal one
// for a probe that failed. A caller that had just created an extension read the
// first as the second, concluded the create had failed and retried in a loop,
// while the walk that would answer the question was already scheduled. So the two
// are told apart by what the state actually holds: no session and no failures
// means nobody has tried yet, and that prints as the transitional word. A disabled
// template is never "starting" — nothing will walk it — and a template with a
// failure keeps `unavailable`, because that is the word that makes a reader look
// at the reason.
func extensionStatusWord(name string, st extensions.Status, enabled bool) (word, next string) {
	word = statusOr(st, extensions.StatusUnavailable)
	switch {
	case word == extensions.StatusIdle:
		// NOT A FAULT, AND THE WORD HAS TO SAY SO. An on-demand extension that
		// nothing has asked for holds nothing, which is the design rather than a
		// failure — so it gets its own word and its own instruction instead of
		// falling into `unavailable` and sending the caller looking for a reason
		// that does not exist (issue #183).
		return word, "not running; it starts when something asks for it — `open ext " + name + "` does that and returns its handles"
	case word == extensions.StatusProbing:
		// THE RECONCILER'S OWN TRANSITIONAL STATE, and it means the same thing to the
		// caller as the fallback below: not up, nothing failed, look again. It used to
		// fall into the early return and lose the hint — so the hint was present only in
		// the window before the reconciler picked the template up, and vanished the
		// moment it did (startProbe sets this state itself). That is backwards: the
		// caller that just created a template is most likely to see `probing` set by the
		// reconciler, because the create is what triggered the walk.
	case word != extensions.StatusUnavailable || len(st.Failures) > 0 || st.SessionID != "":
		return word, ""
	case !enabled:
		return extensions.StatusDisabled, ""
	default:
		word = extensions.StatusProbing
	}
	return word, "the reconciler is trying it now; `get ext " + name + "` reports how it went"
}

// extensionRuntimeFields writes the reconciler's half of an extension onto a
// result entry: the status, the handles an available extension is driven
// through, the tunnels it declared, and how long until the next automatic
// attempt. The list and get surfaces differ only in how much of the failure
// history they carry — the list keeps the latest reason, get keeps the history —
// so everything they share is written once, here.
func (s *Server) extensionRuntimeFields(entry map[string]any, st extensions.Status, withHistory bool) {
	enabled, _ := entry["enabled"].(bool)
	name, _ := entry["name"].(string)
	status, next := extensionStatusWord(name, st, enabled)
	entry["status"] = status
	if next != "" {
		entry["next"] = next
	}
	if st.SessionID != "" {
		entry["session_id"] = st.SessionID
	}
	if st.ShellID != "" {
		entry["shell_id"] = st.ShellID
		// THE INDEX IS A FACT THE SESSION MANAGER OWNS (issue #211). An extension
		// restarted by the reconciler takes the next index on the shared connection
		// (`:2`, `:3`), and leaving it off made the open envelope invent `:1` from
		// nowhere — right by accident for the first channel, and a guaranteed 404
		// once the instance had been rebuilt.
		if s.sessMgr != nil {
			if cs, bad := s.requireShell(st.ShellID); bad == nil && cs != nil {
				if idx := cs.Info().Index; idx > 0 {
					entry["index"] = float64(idx)
				}
			}
		}
	}
	if st.Lazy {
		// HOW LONG IT HAS BEEN IDLE, AND HOW LONG IT HAS LEFT. Both are facts the
		// caller cannot derive: the clock starts at the last ask, which may have been
		// another caller's.
		entry["lazy"] = true
		if st.IdleTimeoutMs > 0 {
			entry["idle_timeout_ms"] = st.IdleTimeoutMs
		}
		if st.IdleForMs > 0 {
			entry["idle_for_ms"] = st.IdleForMs
		}
	}
	if st.ClosedSessionID != "" || st.ClosedAt > 0 {
		// THE INSTANCE THAT WAS TAKEN AWAY, for the caller still holding its handle.
		entry["closed_session_id"] = st.ClosedSessionID
		if st.ClosedShellID != "" {
			entry["closed_shell_id"] = st.ClosedShellID
		}
		entry["closed_at"] = st.ClosedAt
		entry["closed_reason"] = st.ClosedReason
	}
	if len(st.Forwards) > 0 {
		entry["forwards"] = st.Forwards
	}
	if len(st.Targets) > 0 {
		entry["targets_status"] = st.Targets
	}
	if n := len(st.Failures); n > 0 {
		switch {
		case withHistory:
			entry["failures"] = st.Failures
			entry["next_retry"] = retryIn(st.NextRetry)
		case st.Status != extensions.StatusAvailable:
			// The list carries the reason an extension is down, not a repair it has
			// already made: an old exit code beside an available status reads as a
			// current fault. The history is one action=get away.
			entry["last_failure"] = st.Failures[n-1]
			entry["next_retry"] = retryIn(st.NextRetry)
		}
	}
}

func statusOr(st extensions.Status, def string) string {
	if st.Status == "" {
		return def
	}
	return st.Status
}

// retryIn renders when the reconciler will try again in the terms a reader can
// act on: a duration, "now" once the window has elapsed, and — for a zero — the
// fact that nothing is scheduled. Zero means the automatic budget is spent, not
// "immediately", and saying nothing would leave an agent polling a host that is
// not being retried, so the key names the way out.
func retryIn(nextRetryMs int64) string {
	if nextRetryMs <= 0 {
		return "none scheduled; use action=retry to try again"
	}
	if d := time.Until(time.UnixMilli(nextRetryMs)); d > 0 {
		return "in " + d.Round(time.Second).String()
	}
	return "now"
}
