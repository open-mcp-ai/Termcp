package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// extensionSummary is one row of the right panel. It carries what a browser
// needs to render the card without a second fetch: the template's shape, the
// reconciler's verdict on it, and the handles an available extension is driven
// through.
type extensionSummary struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Status      string `json:"status"`
	// URL is set for a template that names an HTTP MCP endpoint instead of a
	// program: the card shows what it talks to, which is the one field that says
	// which of the two shapes this is.
	URL string `json:"url,omitempty"`
	// SessionID/ShellID are the handles a click opens. Empty when the extension
	// is not available, which is exactly the case the card has to explain.
	SessionID string `json:"session_id,omitempty"`
	ShellID   string `json:"shell_id,omitempty"`
	// Forwards are the tunnels the extension declared, with the address each one
	// is listening on once it is up.
	Forwards []extensions.ForwardStatus `json:"forwards,omitempty"`
	// Failure is the most recent reason an extension is not up. One line, not
	// the whole history: the panel shows the verdict, the API serves the rest.
	Failure *extensions.Failure `json:"failure,omitempty"`
	// NextRetryMs is when the reconciler tries again (Unix ms), 0 = no retry
	// scheduled. The panel renders it as a countdown for a human waiting on it.
	NextRetryMs int64 `json:"next_retry_ms,omitempty"`
	// Targets breaks a multi-host extension down per host. Empty for the common
	// single-target template, whose fields are mirrored at the top level.
	Targets      []extensions.TargetStatus `json:"targets,omitempty"`
	HostCount    int                       `json:"host_count"`
	ForwardCount int                       `json:"forward_count"`
}

// summarizeExtension folds a template and its reconciler status into one panel
// row. A template whose state the reconciler has not reached yet reports
// unavailable — an extension starts down and has to be proved up.
func summarizeExtension(ext *extensions.Extension, st extensions.Status) extensionSummary {
	out := extensionSummary{
		Name:        ext.Name,
		Kind:        ext.Kind,
		Target:      ext.Target,
		Description: ext.Description,
		Enabled:     ext.IsEnabled(),
		Status:      st.Status,
		URL:         ext.URL,
		SessionID:   st.SessionID,
		ShellID:     st.ShellID,
		Forwards:    st.Forwards,
		NextRetryMs: st.NextRetry,
		Targets:     st.Targets,
	}
	for _, target := range ext.EffectiveTargets() {
		out.HostCount++
		out.ForwardCount += len(target.Forwards)
	}
	// The failure is reported only while the extension is not up: it is the reason
	// the reader is looking at a down card, and one that outlived its repair puts
	// an old exit code under a running extension's name. The history stays in the
	// detail read, which is where the extension's status card gets it.
	if n := len(st.Failures); n > 0 && out.Status != extensions.StatusAvailable {
		out.Failure = &st.Failures[n-1]
	}
	if out.Status == "" {
		out.Status = extensions.StatusUnavailable
	}
	return out
}

// summarizeExtensions folds every stored template and its reconciler status into
// panel rows. A template whose state the reconciler has not reached yet reports
// unavailable — an extension starts down and has to be proved up — and one whose
// file no longer parses is listed anyway, with whatever failure the reconciler
// recorded on it rather than as a gap in the panel.
//
// It is the single builder for both surfaces that show the list: the REST read
// and the WebSocket frame. The socket marshals once for every open tab while the
// REST call answers one request, which is the only thing the two do differently.
func summarizeExtensions(r *extensions.Reconciler) []extensionSummary {
	names, err := r.Store().List()
	if err != nil {
		return nil
	}
	rows := make([]extensionSummary, 0, len(names))
	for _, name := range names {
		st, _ := r.Status(name)
		ext, err := r.Store().Load(name)
		if err != nil {
			// A file that no longer parses has no template to read, so only the name
			// and the reconciler's verdict are known. Enabled is reported as false
			// rather than true: the panel's toggle reads it, and a card that claims a
			// broken template is enabled invites a click that cannot do anything.
			rows = append(rows, extensionSummary{
				Name:    name,
				Enabled: false,
				Status:  firstNonEmpty(st.Status, extensions.StatusUnavailable),
			})
			continue
		}
		rows = append(rows, summarizeExtension(ext, st))
	}
	return rows
}

func (h *Handler) handleListExtensions(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"extensions": []extensionSummary{}})
		return
	}
	rows := summarizeExtensions(h.Extensions)
	if rows == nil {
		rows = []extensionSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"extensions": rows})
}

func (h *Handler) handleGetExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	name := r.PathValue("name")
	// The editor has two views of one template: the form (the JSON below, keyed the
	// way the extension tool's own arguments are) and the file itself. The TOML view
	// asks for the file, so what the author edits is byte for byte what the store
	// holds — and what an export hands them.
	if r.URL.Query().Get("format") == "toml" {
		raw, err := h.Extensions.Store().ReadRaw(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/toml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	ext, err := h.Extensions.Store().Load(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st, _ := h.Extensions.Status(name)
	// The detail view serves the whole template, notes included: the panel's
	// editor and the failure history both live behind this one call.
	writeJSON(w, http.StatusOK, map[string]any{
		"extension":     summarizeExtension(ext, st),
		"kind":          ext.Kind,
		"target":        ext.Target,
		"targets":       ext.Targets,
		"mode":          ext.Mode,
		"command":       ext.Command,
		"args":          ext.Args,
		"allow":         ext.Allow,
		"paths":         ext.Paths,
		"notes":         ext.Notes,
		"failures":      st.Failures,
		"forward_decls": ext.Forwards,
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

/* The three authoring routes. They are the browser's half of the same authoring
   layer the MCP `extension` tool uses — both decode the author's fields into a
   map, hand it to extensions.ApplyArgs, and persist through
   extensions.SaveTemplate. A field an author omits keeps its stored value on
   update and the preset's value on create; a field present but empty clears it.
   That is the whole semantic contract, and because it lives in
   internal/extensions the two callers cannot drift apart on it. */

// readExtensionBody is the shared decode for create and update. A body over the
// limit is refused rather than truncated: a truncated template would validate
// against fields the author did not send.
func readExtensionBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, extensions.MaxConfigSize+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if len(body) > extensions.MaxConfigSize {
		http.Error(w, "extension template too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, true
	}
	var args map[string]any
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, true
}

func extensionName(r *http.Request) string {
	return strings.TrimSpace(r.PathValue("name"))
}

// bodyName is the create route's name: the file's directory is not in the path
// on a POST, so the author names it in the body. A non-string is refused rather
// than stringified — a name an author sent as a number is not a name.
func bodyName(args map[string]any) (string, bool) {
	if v, ok := args["name"]; ok {
		s, ok := v.(string)
		if !ok {
			return "", false
		}
		return strings.TrimSpace(s), true
	}
	return "", true
}

// bodyKind is the preset to start a create from; an absent or empty kind means a
// custom template with no preset values of its own.
func bodyKind(args map[string]any) string {
	if v, ok := args["kind"]; ok {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return extensions.KindCustom
}

// isTOMLRequest reports whether a write carries the file view of a template
// rather than the form's fields. The editor's two views are two content types on
// one route, so the same URL serves both and neither needs its own endpoint.
func isTOMLRequest(r *http.Request) bool {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "application/toml") || strings.HasPrefix(ct, "text/plain")
}

// readTemplateTOML reads the file view: the TOML a config.toml holds, named by the
// caller. Nothing is defaulted — a hand-written document is taken literally, which
// is what makes the file view honest: the panel's form fills preset values because
// it starts from one, and a file you wrote does not silently gain fields.
func readTemplateTOML(w http.ResponseWriter, r *http.Request, name string) (*extensions.Extension, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, extensions.MaxConfigSize+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if len(body) > extensions.MaxConfigSize {
		http.Error(w, "extension template too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	ext, err := extensions.ParseTemplate(body, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return ext, true
}

func (h *Handler) handleCreateExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	if isTOMLRequest(r) {
		// A document has no place to put the storage name (the file is named by its
		// directory), so a create carries it in the query — the editor's name field
		// is what fills it.
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		ext, ok := readTemplateTOML(w, r, name)
		if !ok {
			return
		}
		if taken, loadable := h.Extensions.Store().NameState(name); taken && loadable {
			http.Error(w, "extension "+name+" already exists; use PUT to change it", http.StatusConflict)
			return
		}
		h.saveExtensionTemplate(w, r, ext, http.StatusCreated)
		return
	}
	args, ok := readExtensionBody(w, r)
	if !ok {
		return
	}
	name, isString := bodyName(args)
	if !isString {
		http.Error(w, "name must be a string", http.StatusBadRequest)
		return
	}
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if taken, loadable := h.Extensions.Store().NameState(name); taken && loadable {
		http.Error(w, "extension "+name+" already exists; use PUT to change it", http.StatusConflict)
		return
	}
	ext := extensions.Preset(bodyKind(args))
	ext.Name = name
	if err := extensions.ApplyArgs(&ext, args, false); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.saveExtensionTemplate(w, r, &ext, http.StatusCreated)
}

// saveExtensionTemplate persists one template and answers with the same summary a
// read would produce. Both views of the editor land here, so a template saved from
// the form and one saved from the file go through identical validation and land as
// the same bytes on disk.
func (h *Handler) saveExtensionTemplate(w http.ResponseWriter, r *http.Request, ext *extensions.Extension, status int) {
	if err := extensions.SaveTemplate(h.Extensions.Store(), ext); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// SAVING FROM THE PANEL IS AN ASK (issue #183). An extension that declares no
	// forward is not dialled until something asks for it, and a person who has just
	// filled in the form and pressed save is asking: leaving them with a row that
	// says `idle` and no button that means "start it" would be a panel that cannot
	// do the one thing the form is for. A LIST is still passive — the panel polling
	// the extension table must not dial anything.
	//
	// The wait is zero because this is a request handler: the ask is what matters,
	// and the panel's next poll shows the outcome. A resident template ignores the
	// ask's timing entirely — the walk brings it up.
	_, _ = h.Extensions.Acquire(ext.Name, 0)
	st, _ := h.Extensions.Status(ext.Name)
	writeJSON(w, status, map[string]any{"extension": summarizeExtension(ext, st)})
}

func (h *Handler) handleUpdateExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	name := extensionName(r)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	ext, err := h.Extensions.Store().Load(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if isTOMLRequest(r) {
		edited, ok := readTemplateTOML(w, r, name)
		if !ok {
			return
		}
		h.saveExtensionTemplate(w, r, edited, http.StatusOK)
		return
	}
	args, ok := readExtensionBody(w, r)
	if !ok {
		return
	}
	if err := extensions.ApplyArgs(ext, args, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.saveExtensionTemplate(w, r, ext, http.StatusOK)
}

func (h *Handler) handleDeleteExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	name := extensionName(r)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	// Comma-separated batch (a,b,c), the shape the session and profile routes use:
	// every name is deleted independently, the answer is 200 with per-name results
	// instead of 204, and one bad entry does not stop the rest. A single name is
	// unchanged. Names cannot contain a comma (the store validates them), which is
	// what makes the split safe.
	if strings.Contains(name, ",") {
		results := make([]map[string]any, 0)
		for _, part := range strings.Split(name, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			code, err := deleteOneExtension(h.Extensions.Store(), part)
			if err != nil {
				results = append(results, map[string]any{"id": part, "ok": false, "code": code, "error": err.Error()})
				continue
			}
			results = append(results, map[string]any{"id": part, "ok": true})
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}
	if err := h.Extensions.Store().Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteOneExtension removes one template for the batch path, classifying the
// outcome so a name that is already gone counts as cleared (another tab got there
// first) rather than as a failure of this one.
func deleteOneExtension(store *extensions.Store, name string) (string, error) {
	if _, err := store.Load(name); err != nil {
		if errors.Is(err, extensions.ErrNotFound) {
			return "extension_not_found", err
		}
		return "operation_failed", err
	}
	if err := store.Delete(name); err != nil {
		return "operation_failed", err
	}
	return "", nil
}

// handleExportExtensions serves the panel's export: a TOML document holding the
// named templates, or every stored one when no names are given. A name that no
// longer resolves is skipped rather than failing the file, so a selection captured
// in a stale tab still downloads.
func (h *Handler) handleExportExtensions(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	var only []string
	if raw := r.URL.Query().Get("names"); raw != "" {
		for _, n := range strings.Split(raw, ",") {
			if n = strings.TrimSpace(n); n != "" {
				only = append(only, n)
			}
		}
	}
	data, err := h.Extensions.Store().ExportDocument(only)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="termcp-extensions.toml"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleImportExtensions stores a whole document at once. Nothing is overwritten:
// a name that is taken comes back in `renamed` under the name it was given
// instead, and the panel tells the author which ones moved.
func (h *Handler) handleImportExtensions(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	// The limit is the document cap itself: a body that reaches it is refused by
	// the store rather than truncated here, so an author never imports a file that
	// was cut short mid-document.
	body, err := io.ReadAll(io.LimitReader(r.Body, extensions.MaxDocumentSize+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := h.Extensions.Store().ImportDocument(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// handleRetryExtension is the human's nudge. It is the same call the agent's
// extension(action=retry) makes, so the two cannot drift apart in what they are
// allowed to do: the panel button and the tool are one reconciler entry point
// with two callers.
func (h *Handler) handleRetryExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "extensions are not configured"})
		return
	}
	name := r.PathValue("name")
	if err := h.Extensions.Retry(name); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
