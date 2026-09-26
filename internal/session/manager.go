package session

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Manager is a thread-safe registry of sessions with persistence.
type Manager struct {
	// approvalMu guards approvalListeners, which fan approval transitions out to
	// every interested subsystem (the web UI hub, the notify wake-up).
	//
	// It is a list rather than a single slot because the subscribers are
	// independent: main installs the agent wake-up and the web UI installs its
	// push, and neither is a replacement for the other. A single slot silently
	// dropped one of them.
	approvalMu        sync.Mutex
	approvalListeners []func(sessionID string, req approval.Request)

	sessions    sync.Map // string → *Session
	internalSSH *sshserver.Server
	msgMgr      *message.Manager
	store       *storage.Store

	listChangeMu sync.RWMutex
	onListChange func()
	onOutputHook func(shellID string)
	onExitHook   func(shellID string, exitCode *int)
	onCloseHook  func(shellID string)
	onDeadHook   func(sessionID string)
}

// SetNotifyHooks registers hooks for terminal I/O and lifecycle events.
func (m *Manager) SetNotifyHooks(onOutput func(shellID string), onExit func(shellID string, exitCode *int), onClose func(shellID string)) {
	m.listChangeMu.Lock()
	m.onOutputHook = onOutput
	m.onExitHook = onExit
	m.onCloseHook = onClose
	m.listChangeMu.Unlock()
}

func (m *Manager) notifyOutput(shellID string) {
	m.listChangeMu.RLock()
	fn := m.onOutputHook
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn(shellID)
	}
}

func (m *Manager) notifyExit(shellID string, exitCode *int) {
	m.listChangeMu.RLock()
	fn := m.onExitHook
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn(shellID, exitCode)
	}
}

func (m *Manager) notifyClose(shellID string) {
	m.listChangeMu.RLock()
	fn := m.onCloseHook
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn(shellID)
	}
}

// AddApprovalListener registers a callback invoked for every approval queue
// transition (submission, approval, rejection, expiry, cancellation).
//
// Subscribers are additive: several subsystems observe the same transitions.
// A callback runs without any manager lock held, so it may call back into the
// session (the web UI broadcaster does).
func (m *Manager) AddApprovalListener(fn func(sessionID string, req approval.Request)) {
	if fn == nil {
		return
	}
	m.approvalMu.Lock()
	m.approvalListeners = append(m.approvalListeners, fn)
	m.approvalMu.Unlock()
}

// notifyApproval forwards one queue transition to every registered listener.
func (m *Manager) notifyApproval(sessionID string, req approval.Request) {
	m.approvalMu.Lock()
	listeners := append([]func(string, approval.Request){}, m.approvalListeners...)
	m.approvalMu.Unlock()
	for _, fn := range listeners {
		fn(sessionID, req)
	}
}

// SetOnDeadHook registers a callback invoked once per session, right after it
// transitions to DEAD (explicit terminate, process exit, or transport loss).
// Resources bound to a live transport — port forwards — are torn down there;
// deleting the session later only finalizes what remains.
func (m *Manager) SetOnDeadHook(fn func(sessionID string)) {
	m.listChangeMu.Lock()
	m.onDeadHook = fn
	m.listChangeMu.Unlock()
}

func (m *Manager) notifyDead(sessionID string) {
	m.listChangeMu.RLock()
	fn := m.onDeadHook
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn(sessionID)
	}
}

// NewManager creates a Manager. internalSSH must be the built-in sshserver.Server (after Start) when using internal profiles; may be nil if only remote sessions are used in tests.
func NewManager(msgMgr *message.Manager, store *storage.Store, internalSSH *sshserver.Server) *Manager {
	return &Manager{
		internalSSH: internalSSH,
		msgMgr:      msgMgr,
		store:       store,
	}
}

// SetSessionListListener registers a callback invoked without holding Manager locks whenever
// the session set or a session's lifecycle state may have changed (create, delete, exit, terminate).
func (m *Manager) SetSessionListListener(fn func()) {
	m.listChangeMu.Lock()
	m.onListChange = fn
	m.listChangeMu.Unlock()
}

func (m *Manager) notifyListChange() {
	m.listChangeMu.RLock()
	fn := m.onListChange
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// NotifyChange triggers the session list change callback (for WebSocket/SSE push).
func (m *Manager) NotifyChange() {
	m.notifyListChange()
}

// Create starts a new session and registers it. A session stays in the registry
// until explicitly deleted; disconnect/terminate/abort only turn it DEAD.
func (m *Manager) Create(cfg Config) (*Session, error) {
	s, err := New(m.internalSSH, cfg, m.msgMgr)
	if err != nil {
		// Failure is invisible downstream (MCP tool error results are debug-levelled;
		// the web UI handler only replies over HTTP), so log it for the terminal.
		// Never log credentials or command text; the error itself already carries
		// the failing host:port for remote dials.
		attrs := []any{"err", err}
		if cfg.Mode != "" {
			attrs = append(attrs, "mode", cfg.Mode) // mode of the first shell
		}
		if cfg.Name != "" {
			attrs = append(attrs, "name", cfg.Name)
		}
		if isRemote(cfg) {
			attrs = append(attrs, "remote_addr", remoteDialAddr(cfg.Remote), "dial_timeout_s", cfg.Remote.DialTimeoutSeconds)
		} else {
			attrs = append(attrs, "endpoint", "internal")
		}
		slog.Error("session create failed", attrs...)
		return nil, err
	}
	m.sessions.Store(s.ID, s)

	// A profile can make review the default for the host. This runs after the
	// session is registered and has its change sink installed (below), so the
	// gate is on before the caller can send anything and the UI is told about it
	// through the same path a manual switch uses.
	//
	// One reviewer, no timeout: a profile says whether to review, not how many
	// people must. A queued request must not expire on its own either — a session
	// whose review mode silently lapsed while a command waited would be worse
	// than one that never gated it.
	if cfg.Approval {
		if err := s.EnableApproval(1, 0); err != nil {
			slog.Error("session approval default failed", "session_id", s.ID, "err", err)
		}
	}

	sid := s.ID
	// Exit watchers started by New() may already be reading these callbacks;
	// atomic stores make the assignment race-free (a watcher firing in the
	// assignment window sees nil, same observable behavior as before).
	onDead := func() {
		// DEAD keeps the object in the registry. Nothing is removed, forgotten,
		// or purged here — only the new state is persisted and the UI notified.
		// Transport-bound resources (forwards) are released through the dead hook.
		slog.Debug("session marked DEAD", "session_id", sid)
		m.persist()
		m.notifyListChange()
		m.notifyDead(sid)
	}
	s.onDead.Store(&onDead)
	onChildChange := m.notifyListChange
	s.onChildChange.Store(&onChildChange)

	onOutput := m.notifyOutput
	s.onOutput.Store(&onOutput)
	onShellExit := m.notifyExit
	s.onShellExit.Store(&onShellExit)
	onShellClose := m.notifyClose
	s.onShellClose.Store(&onShellClose)

	// Approval is a session-scoped resource: when the session goes away, every
	// pending request is cancelled rather than left waiting for a decision that
	// can no longer be executed. Attached here so a session created before
	// approval mode is enabled is still covered.
	s.AttachCleanup(func() { s.DisableApproval() })
	// The change sink is installed per session at creation; approval mode may be
	// switched on much later, so the sink must be in place beforehand.
	s.SetApprovalChangeHandler(m.notifyApproval)

	m.persist()
	m.notifyListChange()
	return s, nil
}

// Get returns a session by ID.
func (m *Manager) Get(id string) *Session {
	v, ok := m.sessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*Session)
}

// GetChildShell searches all sessions for a child shell with the given ID.
// Returns nil if no matching child shell is found.
func (m *Manager) GetChildShell(id string) *ChildShell {
	var found *ChildShell
	m.sessions.Range(func(_, v any) bool {
		if cs := v.(*Session).GetChildShell(id); cs != nil {
			found = cs
			return false
		}
		return true
	})
	return found
}

// GetByShellID returns the parent session that owns the given shell_id.
func (m *Manager) GetByShellID(shellID string) *Session {
	var found *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(shellID) != nil {
			found = s
			return false
		}
		return true
	})
	return found
}

// GetSessionByShellID returns the session that owns a shell_id, searching both
// live in-memory shells and retained DEAD/restored shell snapshots. This keeps
// output-range resolvable for read-only DEAD views after a transport teardown
// or restart when no live ChildShell object exists for the id.
func (m *Manager) GetSessionByShellID(shellID string) *Session {
	var found *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(shellID) != nil {
			found = s
			return false
		}
		if _, ok := s.shellHistory.Load(shellID); ok {
			found = s
			return false
		}
		return true
	})
	return found
}

// CloseChildShell terminates a child shell by its ID and removes it from the owning
// parent session's map. Returns found=false if no such child shell exists.
// The parent session and its SSH connection are unaffected.
func (m *Manager) CloseChildShell(id string) (bool, error) {
	var parent *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(id) != nil {
			parent = s
			return false
		}
		return true
	})
	if parent == nil {
		return false, nil
	}
	err := parent.CloseChildShell(id)
	// Persist the updated per-shell snapshot right away so a restart cannot
	// resurrect the closed shell from the shell manifest; notify the UI so closed
	// shells disappear from tabs immediately.
	m.persist()
	m.notifyListChange()
	return true, err
}

// resolveShellID fills in the session's primary shell when a caller passes an
// empty shellID. Log files are addressed per shell, so an empty id would name a
// path that does not exist and read back nothing; callers that only hold a
// session id (DEAD/restored sessions) depend on this translation.
func (m *Manager) resolveShellID(sessionID, shellID string) string {
	if shellID != "" {
		return shellID
	}
	s := m.Get(sessionID)
	if s == nil {
		return ""
	}
	if cs := s.PrimaryShell(); cs != nil {
		return cs.ID
	}
	// DEAD/restored sessions have no live shell objects; the snapshot holds the
	// shells as they were when the session went down.
	if sh := s.SnapshotShells(); len(sh) > 0 {
		return sh[0].ID
	}
	return ""
}

// Marks returns the status index for one shell (or, with an empty shellID, the
// session's primary shell). DEAD and restart-restored sessions have no in-memory
// buffer, so this index is what says which span of the byte log holds output
// versus input.
func (m *Manager) Marks(sessionID, shellID string) ([]api.LogMark, error) {
	if m.msgMgr == nil {
		return nil, nil
	}
	return m.msgMgr.Marks(sessionID, m.resolveShellID(sessionID, shellID))
}

// OutputByteRange reads a window of a shell's persisted byte log. It exists so
// callers that only have a session id (DEAD/restored sessions) still address the
// same offset space as the live path.
func (m *Manager) OutputByteRange(sessionID, shellID string, start int64, max int) ([]byte, int64, error) {
	if m.msgMgr == nil {
		return nil, 0, nil
	}
	return m.msgMgr.OutputByteRange(sessionID, m.resolveShellID(sessionID, shellID), start, max)
}

// OutputSize returns the current length of a shell's byte log, i.e. the offset
// just past the last byte.
func (m *Manager) OutputSize(sessionID, shellID string) (int64, error) {
	if m.msgMgr == nil {
		return 0, nil
	}
	return m.msgMgr.OutputSize(sessionID, m.resolveShellID(sessionID, shellID))
}

// ListAll returns metadata for all sessions (running and DEAD).
func (m *Manager) ListAll() []api.Session {
	var result []api.Session
	m.sessions.Range(func(_, v any) bool {
		result = append(result, v.(*Session).Info())
		return true
	})
	return result
}

// Terminate closes a session's process/transport and turns it DEAD in place:
// the entry stays in the registry (and in the UI as a read-only tile) with its
// buffers and byte log retained, so shell_output/session_info keep working and
// a restart restores it as a DEAD tile. Use Delete to release resources for good.
func (m *Manager) Terminate(id string, force bool, gracePeriod time.Duration) {
	if s := m.Get(id); s != nil {
		s.Terminate(force, gracePeriod)
	}
}

// Shutdown delegates to Terminate during server shutdown; it still only DEADs
// the session (disconnect ≠ delete).
func (m *Manager) Shutdown(id string, force bool) {
	m.Terminate(id, force, 0)
}

// Delete permanently removes a session: it finalizes a running/DEAD session
// (stops remaining shells, closes buffers/transport, releases the session's
// ResourceScope), drops it from the registry, and removes its on-disk directory
// (manifests + log.bin + log.jsonl). Irreversible — use Terminate to merely close
// a session and keep it readable as a DEAD entry.
func (m *Manager) Delete(id string) error {
	if v, ok := m.sessions.Load(id); ok {
		s := v.(*Session)
		s.finalize()
		m.sessions.Delete(id)
		m.persist()
		m.notifyListChange()
	}
	if m.store != nil {
		// Closing a session is a delete, not an archive: remove the session
		// directory so nothing is left behind.
		if err := m.store.DeleteSession(id); err != nil {
			return err
		}
	}
	return nil
}

// Rename updates the display name of a live session.
func (m *Manager) Rename(id, name string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("session %q not found", id)
	}
	s.mu.Lock()
	s.Name = name
	s.UpdatedAt = clock.Now()
	s.mu.Unlock()
	m.persist()
	m.notifyListChange()
	return nil
}

// EnableApproval turns on approval mode for one session and tells the UI about it.
//
// The broadcast is why this lives on the Manager rather than the Session: a
// session cannot notify the browser list on its own, and without the frame the
// open cards keep rendering the old state until something else happens to
// refresh them (the lock would appear not to have worked).
func (m *Manager) EnableApproval(id string, need int, timeout time.Duration) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("session %q not found", id)
	}
	if err := s.EnableApproval(need, timeout); err != nil {
		return err
	}
	s.mu.Lock()
	s.UpdatedAt = clock.Now()
	s.mu.Unlock()
	m.persist()
	m.notifyListChange()
	return nil
}

// DisableApproval turns approval mode off for one session and broadcasts the
// change. Anything still pending is cancelled by the session itself.
func (m *Manager) DisableApproval(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("session %q not found", id)
	}
	s.DisableApproval()
	s.mu.Lock()
	s.UpdatedAt = clock.Now()
	s.mu.Unlock()
	m.persist()
	m.notifyListChange()
	return nil
}

// FindActiveBySSHConfig returns the first running session that matches the given
// ssh_config name (by session name or ssh_endpoint), or nil if none found.
func (m *Manager) FindActiveBySSHConfig(sshConfig string) *Session {
	var found *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		info := s.Info()
		if info.Status != api.SessionRunning {
			return true
		}
		if info.SSHEndpoint == sshConfig || info.Name == sshConfig {
			found = s
			return false
		}
		return true
	})
	return found
}

// MarkAllDead transitions every running session to DEAD (exited) at server
// shutdown, then persists and notifies. It never finalizes (does not kill
// remaining shells or purge history). Disconnect ≠ delete.
func (m *Manager) MarkAllDead() {
	var live []*Session
	m.sessions.Range(func(_, v any) bool {
		if v.(*Session).Info().Status == api.SessionRunning {
			live = append(live, v.(*Session))
		}
		return true
	})
	for _, s := range live {
		s.Terminate(true, 0)
	}
	m.persist(m.ListAll())
	m.notifyListChange()
}

// Persist shells is called by persist to reconstruct per-shell snapshots so a
// restart can restore DEAD session tabs.
func (m *Manager) persist(sessions ...[]api.Session) {
	if m.store == nil {
		return
	}
	var list []api.Session
	if len(sessions) > 0 && sessions[0] != nil {
		list = sessions[0]
	} else {
		list = m.ListAll()
	}
	// One manifest per session, and one per shell beneath it. There is no list
	// file: the session list is the directory tree, so it cannot disagree with
	// the data it describes.
	for i := range list {
		sess := list[i]
		if s := m.Get(sess.ID); s != nil {
			for _, sh := range s.SnapshotShells() {
				_ = m.store.SaveShell(sess.ID, sh)
			}
		}
		sess.Shells = nil
		_ = m.store.SaveSession(sess)
	}
}

// RestoreDead loads persisted sessions into the registry as read-only DEAD
// sessions (no SSH connection), so previously disconnected sessions reappear as
// tiles after a restart and their history is viewable. Call once at boot.
func (m *Manager) RestoreDead() error {
	if m.store == nil {
		return nil
	}
	list, err := m.store.LoadSessions()
	if err != nil {
		return err
	}
	for _, meta := range list {
		if _, ok := m.sessions.Load(meta.ID); ok {
			continue
		}
		// A restarted process holds no live SSH connection, so any session left
		// "running" is really DEAD; never advertise a fake live tile.
		if meta.Status == api.SessionRunning {
			meta.Status = api.SessionExited
		}
		s := &Session{Session: meta, scope: newResourceScope()}
		// Restored sessions never start a process, but they can still be deleted;
		// keep the message-manager cleanup on the same cascade as live sessions.
		if m.msgMgr != nil {
			sessionID := meta.ID
			s.AttachCleanup(func() { m.msgMgr.ForgetSession(sessionID) })
		}
		onDead := m.notifyListChange
		onChildChange := m.notifyListChange
		s.onDead.Store(&onDead)
		s.onChildChange.Store(&onChildChange)
		for _, sh := range meta.Shells {
			s.shellHistory.Store(sh.ID, sh)
		}
		m.sessions.Store(meta.ID, s)
		m.slogf("restored DEAD session", meta.ID)
	}
	m.persist(m.ListAll())
	m.notifyListChange()
	return nil
}

func (m *Manager) slogf(msg, id string) {
	slog.Debug(msg, "session_id", id)
}
