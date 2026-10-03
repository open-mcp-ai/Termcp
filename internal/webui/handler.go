package webui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/notify"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// embeddedStaticServer serves index.html, inline page script, and vendor/xterm at URL / and /vendor/... .
// It reads through Assets(), so an operator-supplied --assets directory overrides the embedded copy
// file by file and a file the directory does not contain is served from the embed.
func embeddedStaticServer() http.Handler {
	return markdownContentType(http.FileServer(http.FS(Assets())))
}

// markdownContentType pins text/markdown on .md responses. Go's built-in mime
// table is platform-dependent (Windows knows .md, a bare Linux container often
// does not, where http.ServeContent then sniffs "text/plain"), and agents fetch
// these documents to read them, so the type must not vary by host.
func markdownContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".md") {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		}
		next.ServeHTTP(w, r)
	})
}

// Handler serves the browser UI and JSON/SSE APIs at / and /api/... .
type Handler struct {
	Sessions   *session.Manager
	SSH        *sshconfig.Store
	ForwardMgr *forward.ForwardManager
	NotifyMgr  *notify.Manager // active shell notification rules (read-only listing + delete)
	NoInternal bool            // when true, hide and refuse the built-in loopback profile
	Version    string          // build version (`termcp -version`), served at GET /api/version

	// Daemon marks this process as a background instance (started with
	// `termcp daemon start` or `termcp daemon stdio`): it may carry an idle
	// countdown and accepts the graceful stop. GET /api/daemon — the probe
	// every management query is built on — also serves StartedAt, DaemonLog
	// (the file the instance's output goes to; empty for a manually started
	// one) and IdleTimeout.
	Daemon      bool
	StartedAt   string
	DaemonLog   string
	IdleTimeout time.Duration // effective idle countdown; 0 = never auto-exits

	// StopDaemon asks the process to shut down gracefully; main wires it to the
	// same path a SIGTERM takes. Called from POST /api/daemon/stop once the
	// response is on the wire.
	StopDaemon func()

	// ExecuteOperation replays an approved non-terminal request (a file transfer,
	// a port forward). Wired by main to the MCP server, which owns those
	// operations; nil when no such server exists, in which case only command
	// lines can be approved. It exists so the queue can gate operations it does
	// not itself know how to perform.
	ExecuteOperation func(approval.Request) error

	sessHub   *sessionListHub
	notifyHub *uiNotifyHub
}

// Register mounts /api/... and / (HTML/CSS/JS via embed.FS + http.FileServer).
// Call after MCP routes (/sse, /message) so those paths are not shadowed.
func (h *Handler) Register(mux *http.ServeMux) {
	if h.Sessions != nil {
		h.Sessions.SetSessionListListener(h.sessionHub().broadcast)
		// Approval transitions are pushed as payloads (unlike the list hub's
		// bare signal): the browser needs the request body to render the card.
		// The notifyHub is allocated here so the sink is live before any session
		// can enable approval mode.
		_ = h.uiNotifyHub()
		h.Sessions.AddApprovalListener(h.BroadcastApproval)
		// Input an agent sends over MCP is invisible to the browser, so the tab's
		// channel status would only ever reflect local typing. Pushed for both
		// sources so one code path paints it.
		h.Sessions.AddActivityListener(func(shellID string, src session.InputSource, submit bool) {
			h.BroadcastShellActivity(shellID, src, submit)
		})
	}
	if h.ForwardMgr != nil {
		h.ForwardMgr.SetOnChange(h.sessionHub().broadcast)
	}
	if h.NotifyMgr != nil {
		h.NotifyMgr.SetOnChange(h.sessionHub().broadcast)
	}
	if h.SSH != nil {
		h.SSH.SetOnChange(h.sessionHub().broadcast)
	}
	// Build metadata
	mux.HandleFunc("GET /api/version", h.handleVersion)
	// Daemon management (`termcp daemon status|start|stop`): an instance is
	// found and stopped purely over HTTP, wherever it listens.
	mux.HandleFunc("GET /api/daemon", h.handleDaemonInfo)
	mux.HandleFunc("POST /api/daemon/stop", h.handleDaemonStop)

	// Connection profiles
	mux.HandleFunc("GET /api/connections", h.handleListConnections)
	mux.HandleFunc("GET /api/connections/batch", h.handleExportConnections)
	mux.HandleFunc("POST /api/connections/batch", h.handleImportConnections)
	mux.HandleFunc("GET /api/connections/{name}", h.handleGetConnection)
	mux.HandleFunc("PUT /api/connections/{name}", h.handlePutConnection)
	mux.HandleFunc("DELETE /api/connections/{name}", h.handleDeleteConnection)
	mux.HandleFunc("POST /api/connections/test", h.handleTestConnection)

	// Sessions
	mux.HandleFunc("GET /api/sessions", h.handleListSessions)
	// Locator resolution: termcp://<entry> | termcp://#<session>[:index] → ids.
	mux.HandleFunc("GET /api/resolve", h.handleResolve)
	mux.HandleFunc("POST /api/sessions", h.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", h.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", h.handleRenameSession)
	// DELETE purges the session (terminate + remove its on-disk directory).
	mux.HandleFunc("DELETE /api/sessions/{id}", h.handlePurgeSession)
	// Approval mode: per-session gate control, plus the approver worklist and
	// decisions. See approval.go for why the two groups are separate.
	mux.HandleFunc("GET /api/sessions/{id}/approval", h.handleSessionApproval)
	mux.HandleFunc("PATCH /api/sessions/{id}/approval", h.handleSessionApproval)
	mux.HandleFunc("GET /api/approvals", h.handleApprovalList)
	mux.HandleFunc("POST /api/approvals/{id}/approve", h.handleApprovalDecision)
	mux.HandleFunc("POST /api/approvals/{id}/reject", h.handleApprovalDecision)
	mux.HandleFunc("GET /api/ui/ws", h.handleWebUIWS)
	// output-range path id is shell_id (or session_id for primary-shell fallback).
	mux.HandleFunc("GET /api/sessions/{id}/output-range", h.handleSessionOutputRange)
	mux.HandleFunc("POST /api/sessions/{id}/shells", h.handleCreateShell)
	mux.HandleFunc("GET /api/sessions/{id}/shells", h.handleListShells)

	// Shells (globally unique IDs — virtual top-level resource)
	mux.HandleFunc("GET /api/shells/{id}/output-range", h.handleShellOutputRange)
	// The status index behind output-range: where each span starts and what made it.
	mux.HandleFunc("GET /api/shells/{id}/marks", h.handleShellMarks)
	// The row layout of that log for one terminal width, with the marks on it.
	mux.HandleFunc("GET /api/shells/{id}/rail", h.handleShellRail)
	mux.HandleFunc("DELETE /api/shells/{id}", h.handleCloseShell)
	// Terminal I/O over REST for scripts/CLI (the WebSocket stays the real-time path).
	mux.HandleFunc("POST /api/shells/{id}/input", h.handleShellInput)
	mux.HandleFunc("POST /api/shells/{id}/key", h.handleShellKey)
	mux.HandleFunc("POST /api/shells/{id}/resize", h.handleShellResize)

	// Port forwards (list all, delete by ID)
	mux.HandleFunc("GET /api/forwards", h.handleListForwards)
	mux.HandleFunc("DELETE /api/forwards/{id}", h.handleDeleteForward)
	// Session-scoped forwards
	mux.HandleFunc("GET /api/sessions/{id}/forwards", h.handleListSessionForwards)
	mux.HandleFunc("POST /api/sessions/{id}/forwards", h.handleCreateForward)

	// Shell notification rules (registered by the MCP shell_notify tool; the UI
	// lists them and can unregister).
	mux.HandleFunc("GET /api/notifications", h.handleListNotifications)
	mux.HandleFunc("DELETE /api/notifications/{id}", h.handleDeleteNotification)

	// File operations
	mux.HandleFunc("GET /api/sessions/{id}/files", h.handleListFiles)
	mux.HandleFunc("GET /api/sessions/{id}/files/download", h.handleDownloadFile)
	mux.HandleFunc("PUT /api/sessions/{id}/files", h.handleRenameFile)
	mux.HandleFunc("POST /api/sessions/{id}/files/upload", h.handleUploadFile)
	mux.HandleFunc("DELETE /api/sessions/{id}/files", h.handleDeleteFile)
	mux.HandleFunc("POST /api/sessions/{id}/files/dir", h.handleMakeDir)

	// Backward compat: old routes map to canonical handlers
	mux.HandleFunc("POST /api/sessions/start", h.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}/child-shells", h.redirectShells)
	// terminate/disconnect end the whole session (resource tree root).
	mux.HandleFunc("POST /api/sessions/{id}/terminate", h.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/disconnect", h.handleDeleteSession)
	// close-shell closes the primary shell of a session (path id is session_id).
	mux.HandleFunc("POST /api/sessions/{id}/close-shell", h.handleClosePrimaryShell)
	mux.HandleFunc("POST /api/forwards", h.handleCreateForward)
	mux.HandleFunc("DELETE /api/sessions/{id}/files/delete", h.handleDeleteFile)
	mux.HandleFunc("POST /api/sessions/{id}/files/rename", h.handleRenameFile)
	mux.HandleFunc("POST /api/sessions/{id}/files/mkdir", h.handleMakeDir)

	if uiEmbedded {
		mux.Handle("/", embeddedStaticServer())
	} else {
		// Pure-API build (go build -tags no_webui): the browser UI is neither
		// embedded nor routed. The two agent documents stay, each on its own
		// path; everything else under / is a 404.
		docs := embeddedStaticServer()
		mux.Handle("/api.md", docs)
		mux.Handle("/skills.md", docs)
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// parseRange parses an HTTP Range header value (e.g. "bytes=0-1023", "bytes=1024-",
// "bytes=-512") and returns the start offset and length for a file of the given size.
// Returns ok=false when the range is unsatisfiable.
func parseRange(s string, size int64) (start, length int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(s, prefix) {
		return 0, 0, false
	}
	s = s[len(prefix):]

	if strings.HasPrefix(s, "-") {
		// Suffix range: "bytes=-N" → last N bytes.
		n, err := strconv.ParseInt(s[1:], 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, n > 0
	}

	// "bytes=start-end" or "bytes=start-"
	parts := strings.SplitN(s, "-", 2)
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}

	if parts[1] == "" {
		// Open-ended: from start to end-of-file.
		return start, size - start, true
	}

	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start || start >= size {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end - start + 1, true
}
