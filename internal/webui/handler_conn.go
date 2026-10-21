package webui

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/configcrypt"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/vault"
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
	Temporary       bool `json:"temporary"`
}

// errSSHConfigRequired is what an omitted ssh_config resolves to, on every
// surface that starts a session. It exists as a named error (rather than an
// fmt.Errorf at the return site) so a caller — the handler writing the status
// code, a test — can recognise the condition without matching the string.
var errSSHConfigRequired = errors.New("ssh_config is required")

func (h *Handler) handleListConnections(w http.ResponseWriter, r *http.Request) {
	if restRefuseTool(w, r, nil, capability.ProfileList) {
		return
	}
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
		cs := summarizeConnection(n, ent)
		cs.Temporary = h.SSH.IsTemporary(n)
		list = append(list, cs)
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
	if restRefuseTool(w, r, nil, capability.ProfileList) {
		return
	}
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
		if errors.Is(err, vault.ErrLocked) {
			http.Error(w, "the vault is locked: unlock it to edit a vaulted profile", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) handlePutConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	// PUT both creates and edits a profile; either capability admits the call.
	if restRefuseToolAny(w, r, nil, capability.ProfileCreate, capability.ProfileEdit) {
		return
	}
	if sshconfig.IsInternalName(name) {
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
	temporary := h.SSH.IsTemporary(name)
	if from != "" && from != name {
		temporary = h.SSH.IsTemporary(from)
	}
	if values, ok := r.URL.Query()["temporary"]; ok {
		if len(values) != 1 {
			http.Error(w, "temporary must be true or false", http.StatusBadRequest)
			return
		}
		temporary, err = strconv.ParseBool(values[0])
		if err != nil {
			http.Error(w, "temporary must be true or false", http.StatusBadRequest)
			return
		}
	}
	// A session keeps the profile it was created from, so a renamed profile leaves
	// running sessions still pointing at the old name. Rewriting it here is what
	// makes the NetHub card keep answering "which host is this" instead of dropping
	// to offline the moment its label changes.
	if from != "" && from != name {
		if err := h.SSH.Rename(from, name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if h.Sessions != nil {
			h.Sessions.RenameSSHConfig(from, name)
		}
	}
	if err := h.SSH.SaveWithOptions(name, body, temporary); err != nil {
		if errors.Is(err, vault.ErrLocked) {
			http.Error(w, "the vault is locked: unlock it before writing a vaulted profile", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The browser uploads and downloads opaque TOML bytes. Parsing and validation
// belong entirely to the backend so every client uses the same format.
func (h *Handler) handleExportConnections(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	// Export is the read that carries credentials: a scoped token without
	// profile.list gets no export, sealed or not.
	if restRefuseTool(w, r, nil, capability.ProfileList) {
		return
	}
	// names=a,b exports just those profiles; absent or empty exports all. An
	// explicit selection that names the built-in profile is refused outright —
	// the Web UI blocks it first, and an API caller gets the same answer —
	// while a name that merely no longer resolves is skipped so a selection
	// captured in a stale tab still downloads the profiles that remain.
	var only []string
	if raw := r.URL.Query().Get("names"); raw != "" {
		for _, n := range strings.Split(raw, ",") {
			if n = strings.TrimSpace(n); n != "" {
				if sshconfig.IsInternalName(n) {
					http.Error(w, "the built-in internal profile cannot be exported", http.StatusBadRequest)
					return
				}
				only = append(only, n)
			}
		}
	}
	sealed := r.Header.Get(EncryptHeader) != ""
	res, err := h.SSH.ExportBatch(only, sealed)
	if err != nil {
		if errors.Is(err, vault.ErrLocked) {
			http.Error(w, "the vault is locked: unlock it before exporting vaulted profiles", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := res.Document
	if len(res.SkippedVaulted) > 0 {
		// Plaintext export withholds vaulted profiles (see ExportBatch). The
		// header names them so the UI can warn instead of producing a file the
		// operator believes is complete.
		w.Header().Set("X-Termcp-Skipped-Vaulted", strings.Join(res.SkippedVaulted, ","))
	}

	/* An export leaves the machine, so it can be sealed with a password the
	   operator types (issue #108, item 3). The password arrives in a header, not
	   the query: a URL ends up in shell history, proxy logs and browser history,
	   and a password in one of those is a password leaked. Absent header = the
	   plain export this route has always produced, so existing scripts keep
	   working and an operator who does not ask for encryption gets a file they
	   can see is unencrypted. */
	filename := "termcp-connections.toml"
	if password := r.Header.Get(EncryptHeader); password != "" {
		sealed, err := configcrypt.Encrypt(data, password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data = sealed
		// The extension says what the file is: an operator who finds it later
		// knows it needs termcp, not a TOML editor.
		filename = "termcp-connections.enc"
		w.Header().Set("Content-Type", "application/x-termcp-encrypted")
	} else {
		w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) handleImportConnections(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	// Injection is profile creation: a scoped token needs profile.create in
	// its tools whitelist — that is exactly the sidecar token's shape
	// (`termcp token create --tools profile.create`), which can inject but
	// cannot read or manage anything else.
	if restRefuseTool(w, r, nil, capability.ProfileCreate) {
		return
	}
	temporary := false
	if raw := r.URL.Query().Get("temporary"); raw != "" {
		var err error
		temporary, err = strconv.ParseBool(raw)
		if err != nil {
			http.Error(w, "temporary must be true or false", http.StatusBadRequest)
			return
		}
	}
	// ttl_seconds bounds how long an imported temporary profile lives in this
	// process (the sidecar injection window); it is meaningless for a
	// persistent import and rejected there rather than silently ignored.
	var ttl time.Duration
	if raw := r.URL.Query().Get("ttl_seconds"); raw != "" {
		if !temporary {
			http.Error(w, "ttl_seconds only applies with temporary=true", http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 24*3600 {
			http.Error(w, "ttl_seconds must be 1..86400", http.StatusBadRequest)
			return
		}
		ttl = time.Duration(n) * time.Second
	}
	const maxUpload = 16 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUpload+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	/* An encrypted export is opened here, before the TOML parser sees it. The
	   header carries the password for the same reason the export takes it there:
	   it must not be in a URL. A plain document needs no password and is passed
	   through untouched — the old format stays importable forever. */
	if configcrypt.IsEncrypted(body) {
		password := r.Header.Get(EncryptHeader)
		if password == "" {
			http.Error(w, "this configuration export is encrypted: send the password in the "+
				EncryptHeader+" header", http.StatusUnauthorized)
			return
		}
		plain, err := configcrypt.Decrypt(body, password)
		if err != nil {
			// 400 for a wrong password or a damaged file: both are "the bytes you
			// sent cannot be opened", and the message distinguishes them.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = plain
	}
	result, err := h.SSH.ImportBatch(body, temporary, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// deleteOneConnection removes one profile for the batch path, classifying the
// outcome so the UI can treat a name that is already gone as cleared and the
// built-in profile's refusal as its own code rather than a generic failure.
// The two codes come from the store's own sentinels, so neither the reserved
// name nor its message is restated here. (Profile names cannot contain a comma,
// which is what makes the batch split safe.)
func deleteOneConnection(store *sshconfig.Store, name string) (string, error) {
	if _, readErr := store.ReadRaw(name); readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return "connection_not_found", readErr
		}
		return "operation_failed", readErr
	}
	if err := store.Delete(name); err != nil {
		if errors.Is(err, sshconfig.ErrReserved) {
			return "reserved_profile", err
		}
		return "operation_failed", err
	}
	return "", nil
}

// handleDeleteConnection removes a profile. Running sessions are untouched: a
// session owns its already-open connection, so only future dials lose the
// profile.
//
// {name} may be a comma-separated batch (a,b,c) like the session lifecycle
// routes: every entry is deleted independently, the response is 200 with a
// per-name results array instead of 204, and one bad entry does not stop the
// rest. A single name is unchanged.
func (h *Handler) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if restRefuseTool(w, r, nil, capability.ProfileDelete) {
		return
	}
	if strings.Contains(name, ",") {
		results := make([]map[string]any, 0)
		for _, part := range strings.Split(name, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			code, err := deleteOneConnection(h.SSH, part)
			if err != nil {
				results = append(results, map[string]any{"id": part, "ok": false, "code": code, "error": err.Error()})
				continue
			}
			results = append(results, map[string]any{"id": part, "ok": true})
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
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
	// Testing a connection spends the stored credential against the host: it
	// is a read-side probe, so it follows the read capability.
	if restRefuseTool(w, r, nil, capability.ProfileList) {
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
	writeJSON(w, http.StatusOK, session.TestConnection(r.Context(), remote))
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
		/* Deliberately NOT a fallback to the loopback profile.

		   Resolving an empty ssh_config to "internal" handed a caller that forgot the
		   field — or a client that dropped it — a live shell on the host Termcp runs
		   on, while a misspelled profile failed loudly. An empty value is the one
		   case where the caller cannot tell it asked for nothing, and the only one
		   that lands on the machine holding every stored credential. MCP's
		   session_start has always refused it (errMissingSSHConfig); the two
		   surfaces answer the same way now. Do not reintroduce a default here.

		   NoInternal is irrelevant to this: the answer to a missing field is "say
		   which host", not "here is one". */
		return "", nil, nil, errSSHConfigRequired
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

// EncryptHeader carries the password for an encrypted configuration export or
// import.
//
// A header rather than a query parameter or a body field, for two reasons. A URL
// lands in shell history, proxy logs and browser history, so a password there is
// leaked by the same act that used it. And an import body is the document itself
// — TOML or the sealed envelope — so there is nowhere in it to put a password
// without inventing a second format for the same file.
const EncryptHeader = "X-Termcp-Password"
