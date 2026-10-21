package webui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/notify"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// embeddedStaticServer serves index.html, inline page script, and vendor/xterm at URL / and /vendor/... .
// It reads through Assets(), so an operator-supplied --assets directory overrides the embedded copy
// file by file and a file the directory does not contain is served from the embed.
func embeddedStaticServer() http.Handler {
	return noCacheForEmbeddedAssets(markdownContentType(http.FileServer(http.FS(Assets()))))
}

// noCacheForEmbeddedAssets marks the embedded UI assets no-cache. Embedded files
// carry no modification time, so http.FileServer sends no Last-Modified and no
// ETag — a response with neither a validator nor a freshness directive leaves
// the browser heuristically caching a stylesheet from a PREVIOUS binary, which
// then paints the new markup with old rules until a hard refresh. The embed
// lives in memory and is cheap to resend, so every load simply refetches.
func noCacheForEmbeddedAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cacheControl := "no-cache"
		if strings.HasPrefix(r.URL.Path, "/themes/") {
			// Theme imports must refresh even when an editor saves twice in one
			// second: Last-Modified cannot distinguish those disk versions.
			cacheControl = "no-store"
			r = r.Clone(r.Context())
			r.Header.Del("If-Modified-Since")
			r.Header.Del("If-None-Match")
		}
		w.Header().Set("Cache-Control", cacheControl)
		next.ServeHTTP(w, r)
	})
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
	MsgMgr     *message.Manager
	SSH        *sshconfig.Store
	ForwardMgr *forward.ForwardManager
	NotifyMgr  *notify.Manager // active shell notification rules (read-only listing + delete)
	// Extensions drives the right-side panel: it lists the pinned,
	// capability-trimmed sessions and reports whether each is up. nil in a build
	// that does not run the reconciler, in which case the panel is simply empty.
	Extensions *extensions.Reconciler
	NoInternal bool   // when true, hide and refuse the built-in loopback profile
	Version    string // build version (`termcp -version`), served at GET /api/version

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

	// viewers tracks the Web UI tabs currently attached (see viewers.go), so the
	// page can show who is watching. Created on first use.
	viewers    *viewerRegistry
	viewerOnce sync.Once

	// MCPConnections lists the MCP clients attached to the MCP server (see
	// internal/mcp/connections.go). Wired by main to server.Connections; nil when
	// no MCP server exists, in which case GET /api/clients returns no MCP rows.
	//
	// It returns []any rather than []mcp.Connection so this package does not
	// import internal/mcp: that package's tests load this one's embedded assets
	// (internal/mcp/docs_test.go), and an import in this direction would close a
	// cycle Go rejects. The values are still mcp.Connection — the page sees JSON
	// either way.
	MCPConnections func() []any
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
	if h.Extensions != nil {
		// An extension's status transition is what the right panel renders, so it
		// rides the same signal as a connection change: every tab refetches.
		h.Extensions.SetOnChange(h.sessionHub().broadcast)
	}
	// Build metadata
	mux.HandleFunc("GET /api/version", h.handleVersion)
	// Who is attached to this instance: MCP clients and Web UI tabs. Not the SSH
	// profiles of GET /api/connections — those are hosts to connect to.
	mux.HandleFunc("GET /api/clients", h.handleListClients)
	// Recent agent and human actions across every session, newest first. Derived
	// from the per-shell marks the terminal timeline already reads.
	mux.HandleFunc("GET /api/activity", h.handleActivity)
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

	// Extensions (the right panel): templates for pinned, capability-trimmed
	// sessions with the reconciler's live status on each. The authoring routes
	// write through the same store the MCP `extension` tool uses, so a template
	// saved from the browser and one saved by an agent are indistinguishable, and
	// the batch pair is the panel's export and import — one document holding many
	// templates, the same shape in both directions.
	mux.HandleFunc("GET /api/extensions", h.handleListExtensions)
	mux.HandleFunc("POST /api/extensions", h.handleCreateExtension)
	mux.HandleFunc("GET /api/extensions/batch", h.handleExportExtensions)
	mux.HandleFunc("POST /api/extensions/batch", h.handleImportExtensions)
	mux.HandleFunc("GET /api/extensions/{name}", h.handleGetExtension)
	mux.HandleFunc("PUT /api/extensions/{name}", h.handleUpdateExtension)
	mux.HandleFunc("DELETE /api/extensions/{name}", h.handleDeleteExtension)
	mux.HandleFunc("POST /api/extensions/{name}/retry", h.handleRetryExtension)

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
	// Line-addressed reads over the same log (REST twin of shell_output from_line).
	mux.HandleFunc("GET /api/shells/{id}/lines", h.handleShellLines)
	// The row layout of that log for one terminal width, with the marks on it.
	mux.HandleFunc("GET /api/shells/{id}/rail", h.handleShellRail)
	mux.HandleFunc("DELETE /api/shells/{id}", h.handleCloseShell)
	// Closing a channel is the ordinary act and KEEPS its record; DELETE above is the
	// erase. Two verbs because they are two decisions: a tab's X should not destroy
	// the transcript that the archive exists to show. Same capability gate as close —
	// a caller allowed to stop a channel is allowed to stop it either way, and one that
	// is not may do neither.
	mux.HandleFunc("POST /api/shells/{id}/archive", h.handleArchiveShell)
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
		// embedded nor routed. The one agent document stays on its own path;
		// everything else under / is a 404. The extension guide is deliberately NOT
		// a document any more — `help ext` carries it, and a guide the tool can
		// reach cannot go stale against the tool it describes.
		docs := embeddedStaticServer()
		mux.Handle("/api.md", docs)
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
