package webui

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

type connectionSummary struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Host        string `json:"host,omitempty"`
	User        string `json:"user,omitempty"`
	Port        int    `json:"port,omitempty"`
	// DefaultApproval is the profile's review default for new sessions. It is
	// served on the list so the card can show the flag without a second fetch.
	DefaultApproval bool `json:"default_approval"`
}

func (h *Handler) handleListConnections(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		writeJSON(w, http.StatusOK, map[string]any{"connections": []connectionSummary{}})
		return
	}
	names, err := h.SSH.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var list []connectionSummary
	for _, n := range names {
		ent, err := h.SSH.Load(n)
		if err != nil {
			continue
		}
		if h.NoInternal && ent.Kind == sshconfig.KindInternal {
			continue
		}
		list = append(list, summarizeConnection(n, ent))
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": list})
}

func summarizeConnection(name string, ent *sshconfig.Entry) connectionSummary {
	cs := connectionSummary{
		Name:            name,
		Kind:            ent.Kind,
		Description:     ent.Description,
		DefaultApproval: ent.DefaultApproval,
	}
	if ent.Kind == sshconfig.KindRemote {
		cs.Host = strings.TrimSpace(ent.Host)
		cs.User = strings.TrimSpace(ent.User)
		cs.Port = ent.Port
		if cs.Port == 0 {
			cs.Port = 22
		}
	}
	return cs
}

func (h *Handler) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	data, err := h.SSH.ReadRaw(name)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) handlePutConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if strings.EqualFold(strings.TrimSpace(name), "internal") {
		// The internal profile IS editable, but only as an override: the store
		// layers it over the built-in defaults, so setting one field does not
		// mean restating kind/description. Renaming it is still refused — the
		// name is how the built-in connection is addressed.
		if from := strings.TrimSpace(r.URL.Query().Get("from")); from != "" && !strings.EqualFold(from, name) {
			http.Error(w, "the built-in internal profile cannot be renamed", http.StatusForbidden)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := sshconfig.ParseAndValidate(body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	if from != "" && from != name {
		if err := h.SSH.Rename(from, name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := h.SSH.Save(name, body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if err := h.SSH.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestConnection probes an SSH profile without opening a session.
// POST /api/connections/test — body is the same TOML text as PUT /api/connections/{name},
// so the Web UI can test unsaved editor content. The "internal" profile is a
// loopback connection and always reports ok without dialing.
func (h *Handler) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ent, err := sshconfig.ParseAndValidate(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ent.Kind != sshconfig.KindRemote {
		writeJSON(w, http.StatusOK, &session.TestResult{OK: true})
		return
	}
	remote, err := sshconfig.RemoteFromEntry(ent, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, session.TestConnection(remote))
}

func (h *Handler) resolveSSH(name string) (cfgName string, ent *sshconfig.Entry, remote *session.RemoteSSH, err error) {
	if h.SSH == nil {
		return "", nil, nil, fmt.Errorf("ssh config store not configured")
	}
	name = strings.TrimSpace(name)
	// An entry locator is accepted here for the same reason MCP accepts one in
	// session_start(ssh_config=...): the user copies "termcp://rock64" from a card
	// and pastes it as the connection. It is a JSON body field, so the '#' and ':'
	// that keep locators out of URL paths are not a problem.
	if locator.LooksLike(name) {
		p, perr := locator.Parse(name)
		if perr != nil {
			return "", nil, nil, fmt.Errorf("invalid ssh_config locator: %w", perr)
		}
		if p.Kind != locator.KindEntry {
			return "", nil, nil, fmt.Errorf("ssh_config %q is a session/shell locator; pass an entry name or termcp://<entry>", name)
		}
		name = p.Entry
	}
	if name == "" {
		if h.NoInternal {
			return "", nil, nil, fmt.Errorf("ssh_config is required when internal profile is disabled")
		}
		name = "internal"
	}
	ent, err = h.SSH.Load(name)
	if err != nil {
		return "", nil, nil, err
	}
	if ent.Kind == sshconfig.KindInternal {
		if h.NoInternal {
			return "", nil, nil, fmt.Errorf("internal profile is disabled")
		}
		return name, ent, nil, nil
	}
	r, err := sshconfig.RemoteFromEntry(ent, h.SSH.ConfigDir(name))
	if err != nil {
		return "", nil, nil, err
	}
	return name, ent, r, nil
}
