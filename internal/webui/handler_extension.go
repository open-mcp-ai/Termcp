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

// extensionSummary is one row of the right panel: its name, the reconciler's
// verdict, and the holes a human on this installation still has to fill.
type extensionSummary struct {
	Name        string `json:"name"`
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
	// Holes is what the template still asks this installation for. A card with
	// unfilled holes is not broken — it is installed and waiting — and the panel
	// needs the two to look different, because one is a click and the other is a
	// repair.
	Holes []extensions.Hole `json:"holes,omitempty"`
	// Needs is the names of the unfilled holes, for a compact badge.
	Needs []string `json:"needs,omitempty"`
}

// summarizeExtension folds a template and its reconciler status into one panel
// row. A template whose state the reconciler has not reached yet reports
// unavailable — an extension starts down and has to be proved up.
func summarizeExtension(ext *extensions.Extension, st extensions.Status) extensionSummary {
	out := extensionSummary{
		Name:        ext.Name,
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
			// A template that no longer parses has no extension to read, so only the
			// name and the reconciler's verdict are known. Enabled is reported as false
			// rather than true: the panel's toggle reads it, and a card that claims a
			// broken template is enabled invites a click that cannot do anything.
			row := extensionSummary{
				Name:    name,
				Enabled: false,
				Status:  firstNonEmpty(st.Status, extensions.StatusUnavailable),
			}
			// AN INSTALLED TEMPLATE WAITING FOR VALUES IS NOT A BROKEN ONE. The
			// difference is the whole installation story: one card needs a value typed
			// in, the other needs a file repaired, and a panel that shows them the same
			// way teaches neither.
			if holes, hErr := r.Store().Holes(name); hErr == nil && len(holes) > 0 {
				row.Holes = holes
				for _, h := range holes {
					if !h.Set {
						row.Needs = append(row.Needs, h.Name)
					}
				}
				if len(row.Needs) > 0 {
					row.Status = extensions.StatusIdle
				}
			}
			rows = append(rows, row)
			continue
		}
		row := summarizeExtension(ext, st)
		if holes, hErr := r.Store().Holes(name); hErr == nil && len(holes) > 0 {
			row.Holes = holes
			for _, h := range holes {
				if !h.Set {
					row.Needs = append(row.Needs, h.Name)
				}
			}
		}
		rows = append(rows, row)
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

func (h *Handler) handlePreviewExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "extension name is required", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, extensions.MaxConfigSize+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > extensions.MaxConfigSize {
		http.Error(w, "extension template too large", http.StatusRequestEntityTooLarge)
		return
	}
	values, err := h.Extensions.Store().Values(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rendered, err := extensions.Render(body, values)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// VALIDATED THE WAY Load VALIDATES, so a preview cannot call something fine that
	// the store will refuse the moment it is saved.
	parsed, err := extensions.Parse(rendered)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	parsed.Name = name
	parsed.Normalize()
	if err := parsed.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	_, _ = w.Write(rendered)
}

func (h *Handler) handleGetExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	name := r.PathValue("name")
	// `format=toml` answers the TEMPLATE — the document with its holes — because that
	// is what an author or a package writes. It is not the extension: the extension is
	// the template plus this installation's values.
	if r.URL.Query().Get("format") == "toml" {
		raw, err := h.Extensions.Store().Template(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/toml; charset=utf-8")
		_, _ = w.Write(raw)
		return
	}
	holes, holesErr := h.Extensions.Store().Holes(name)
	if holesErr != nil {
		http.NotFound(w, r)
		return
	}
	ext, err := h.Extensions.Store().Load(name)
	if err != nil {
		// A template whose holes are unfilled still has a detail view: it is the view
		// that says which holes are unfilled. Reporting 404 here would tell a reader the
		// extension does not exist when the truth is that it needs one value.
		var missing []string
		for _, hole := range holes {
			if !hole.Set {
				missing = append(missing, hole.Name)
			}
		}
		if len(missing) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"name":     name,
				"holes":    holes,
				"needs":    missing,
				"status":   extensions.StatusIdle,
				"template": "the template is stored and waiting for these values",
			})
			return
		}
		http.NotFound(w, r)
		return
	}
	st, _ := h.Extensions.Status(name)
	// The detail view serves the rendered extension AND the holes behind it: what an
	// extension does, and what this installation supplied to make it do that.
	writeJSON(w, http.StatusOK, map[string]any{
		"extension":     summarizeExtension(ext, st),
		"target":        ext.Target,
		"targets":       ext.Targets,
		"mode":          ext.Mode,
		"command":       ext.Command,
		"args":          ext.Args,
		"allow":         ext.Allow,
		"paths":         ext.Paths,
		"notes":         ext.Notes,
		"holes":         holes,
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
   map, hand it to extensions.ApplyArgs, and persist through the same Store this
   tool writes. A field an author omits keeps its stored value on update and the
   conventional default on create; a field present but empty clears it.
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
	// A TOML body is the template itself, which is what the document view sends back.
	// Two spellings of one route, because the two callers send different things: the
	// panel sends a command (`{template, values}`), a person pasting a package sends
	// the document.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/toml") {
		return map[string]any{"template": string(body)}, true
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

func (h *Handler) handleCreateExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
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
		// A pasted document carries no name — a stored template is named by its
		// directory — so the create route takes it from the query.
		name = strings.TrimSpace(r.URL.Query().Get("name"))
	}
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if taken, loadable := h.Extensions.Store().NameState(name); taken && loadable {
		http.Error(w, "extension "+name+" already exists; use PUT to change it", http.StatusConflict)
		return
	}
	if template, ok := args["template"].(string); ok && strings.TrimSpace(template) != "" {
		if err := h.Extensions.Store().SaveTemplate(name, []byte(template)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if values, ok := args["values"].(map[string]any); ok && len(values) > 0 {
			if !h.applyValues(w, name, values) {
				return
			}
		}
		h.writeExtensionAfterEdit(w, name, http.StatusCreated)
		return
	}
	ext := extensions.Extension{}
	ext.Name = name
	if err := extensions.ApplyArgs(&ext, args, false); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := extensions.SaveStruct(h.Extensions.Store(), &ext); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.writeExtensionAfterEdit(w, name, http.StatusCreated)
}

// applyValues stores a value set and answers the two failures differently.
//
// A REFUSED VALUE IS A 400 — it names a hole this template does not have, or a type
// it cannot hold — and nothing was written. A value that IS stored but does not yet
// render is not an error at all: the caller's input is the only copy of itself, so
// it is kept and the row comes back with what is still missing. Collapsing the two
// would either throw away a half-typed port or claim a typo was accepted.
func (h *Handler) applyValues(w http.ResponseWriter, name string, values map[string]any) bool {
	err := h.Extensions.Store().SetValues(name, values)
	if err == nil {
		return true
	}
	var notApplied *extensions.NotApplied
	if errors.As(err, &notApplied) {
		return true
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
	return false
}

// writeExtensionAfterEdit answers with either the running extension or the holes
// it still needs. Installing a template before filling its holes is a successful
// install, not a failed write.
func (h *Handler) writeExtensionAfterEdit(w http.ResponseWriter, name string, status int) {
	holes, _ := h.Extensions.Store().Holes(name)
	missing, _ := h.Extensions.Store().Missing(name)
	ext, err := h.Extensions.Store().Load(name)
	if err != nil {
		writeJSON(w, status, map[string]any{"name": name, "holes": holes, "needs": missing, "stored": true})
		return
	}
	_, _ = h.Extensions.Acquire(name, 0)
	st, _ := h.Extensions.Status(name)
	// THE HOLES TRAVEL WITH THE ROW either way. A caller that just typed a value needs
	// to see whether it filled the hole it was aimed at, and a row that says
	// "available" while a value sits unused would be the panel lying about what it
	// just did.
	writeJSON(w, status, map[string]any{
		"extension": summarizeExtension(ext, st),
		"holes":     holes,
		"needs":     missing,
	})
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
	args, ok := readExtensionBody(w, r)
	if !ok {
		return
	}
	if template, ok := args["template"].(string); ok && strings.TrimSpace(template) != "" {
		if err := h.Extensions.Store().SaveTemplate(name, []byte(template)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if values, ok := args["values"].(map[string]any); ok && len(values) > 0 {
			if !h.applyValues(w, name, values) {
				return
			}
		}
		h.writeExtensionAfterEdit(w, name, http.StatusOK)
		return
	}
	if values, ok := args["values"].(map[string]any); ok && len(values) > 0 {
		if !h.applyValues(w, name, values) {
			return
		}
		h.writeExtensionAfterEdit(w, name, http.StatusOK)
		return
	}
	ext, err := h.Extensions.Store().Load(name)
	if err != nil {
		if missing, mErr := h.Extensions.Store().Missing(name); mErr == nil && len(missing) > 0 {
			http.Error(w, "extension still needs values for "+strings.Join(missing, ", "), http.StatusConflict)
			return
		}
		http.NotFound(w, r)
		return
	}
	if err := extensions.ApplyArgs(ext, args, true); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := extensions.SaveStruct(h.Extensions.Store(), ext); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.writeExtensionAfterEdit(w, name, http.StatusOK)
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
