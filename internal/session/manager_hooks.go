package session

import (
	"github.com/open-mcp-ai/termcp/internal/approval"
)

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
	// A shell's exit is a durable state change: its status becomes exited and its
	// exit code is set, and both are read back after a restart to render the shell
	// tab. Nothing else writes them - the exit watcher only retains them in memory
	// (shellHistory) - so without this the manifest kept saying "running" and a
	// restart resurrected finished shells as live ones.
	//
	// Persist only the owning session, and resolve it from the shell rather than
	// scanning, since this runs on every shell exit. The lookup can legitimately
	// fail (a shell closed concurrently), in which case there is no owner left to
	// describe and skipping is correct.
	m.listChangeMu.RLock()
	fn := m.onExitHook
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn(shellID, exitCode)
	}
	if owner := m.GetByShellID(shellID); owner != nil {
		m.persistOne(owner.ID)
	}
	m.notifyListChange()
}

func (m *Manager) notifyClose(shellID string) {
	m.listChangeMu.RLock()
	fn := m.onCloseHook
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn(shellID)
	}
}

// AddActivityListener registers a callback invoked whenever a shell receives
// input, with the source that sent it. The bytes themselves are not carried:
// a subscriber wants to know *that* someone is typing and who it is, which is
// what a status display needs, and the keystrokes already reach the browser
// through the terminal's own echo.
func (m *Manager) AddActivityListener(fn func(shellID string, src InputSource, submit bool)) {
	if fn == nil {
		return
	}
	m.activityMu.Lock()
	m.activityListeners = append(m.activityListeners, fn)
	m.activityMu.Unlock()
}

// notifyActivity forwards one input event to every registered listener.
func (m *Manager) notifyActivity(shellID string, src InputSource, submit bool) {
	m.activityMu.Lock()
	listeners := append([]func(string, InputSource, bool){}, m.activityListeners...)
	m.activityMu.Unlock()
	for _, fn := range listeners {
		fn(shellID, src, submit)
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
