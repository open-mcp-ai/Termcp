package webui

import (
	"net/http"
	"os"
)

// Daemon management lives entirely on these two routes: a front-end discovers
// an instance by asking GET /api/daemon — never through process tables, so an
// instance is found wherever it listens, regardless of which data directory or
// platform started it — and stops a daemon-started one through the graceful
// POST /api/daemon/stop. Both sit behind the regular auth middleware.

// handleDaemonInfo answers the probe behind `termcp daemon status` and
// friends: what this process is, whether it is a background daemon, and where
// its output goes.
func (h *Handler) handleDaemonInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"daemon":          h.Daemon,
		"pid":             os.Getpid(),
		"version":         h.Version,
		"started_at":      h.StartedAt,
		"log":             h.DaemonLog,
		"idle_timeout_ms": h.IdleTimeout.Milliseconds(),
		/* UP, BUT STILL READING YESTERDAY (issue #353). The socket is bound before the
		   session history is restored, so a probe that only asked "is anything
		   listening" cannot tell a half-loaded instance from a loaded one. This is
		   that difference, for a client that wants to wait for the full list. */
		"restoring": h.restoring(),
	})
}

// restoring reports the instance's own state, defaulting to false when no probe
// was wired: an instance with no restore to report is not restoring.
func (h *Handler) restoring() bool {
	if h.Restoring == nil {
		return false
	}
	return h.Restoring()
}

// handleDaemonStop shuts the process down gracefully — the same path a SIGTERM
// takes. Only instances started as daemons have that lifecycle; a manually
// started instance answers 409 and is left alone.
func (h *Handler) handleDaemonStop(w http.ResponseWriter, r *http.Request) {
	if !h.Daemon {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not a daemon instance"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	if h.StopDaemon != nil {
		// Put the response on the wire before the shutdown starts, so the asking
		// process always gets its answer. Flush is explicit: the shutdown may
		// cancel the request's context, and the response must not depend on when
		// the runtime happens to write it out.
		_ = http.NewResponseController(w).Flush()
		go h.StopDaemon()
	}
}
