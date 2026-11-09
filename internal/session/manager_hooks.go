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
	for {
		cur := m.activityListeners.Load()
		var next []func(string, InputSource, bool)
		if cur != nil {
			next = append(next, (*cur)...)
		}
		next = append(next, fn)
		if m.activityListeners.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// notifyActivity forwards one input event to every registered listener.
func (m *Manager) notifyActivity(shellID string, src InputSource, submit bool) {
	var listeners []func(string, InputSource, bool)
	if p := m.activityListeners.Load(); p != nil {
		listeners = *p
	}
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
	for {
		cur := m.approvalListeners.Load()
		var next []func(string, approval.Request)
		if cur != nil {
			next = append(next, (*cur)...)
		}
		next = append(next, fn)
		if m.approvalListeners.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// notifyApproval forwards one queue transition to every registered listener.
func (m *Manager) notifyApproval(sessionID string, req approval.Request) {
	var listeners []func(string, approval.Request)
	if p := m.approvalListeners.Load(); p != nil {
		listeners = *p
	}
	for _, fn := range listeners {
		fn(sessionID, req)
	}
}

// AddOnDeadHook subscribes to the once-per-session DEAD transition: the callback
// runs right after a session becomes DEAD (explicit terminate, process exit, or
// transport loss). Resources bound to a live transport — port forwards — are torn
// down there; deleting the session later only finalizes what remains.
//
// The event is fanned out rather than owned, because termcp has more than one
// consumer of it: the forward manager releasing a dead session's tunnels and the
// extension reconciler marking its extension unavailable subscribe to the same
// transition, and neither may displace the other.
func (m *Manager) AddOnDeadHook(fn func(sessionID string)) {
	if fn == nil {
		return
	}
	m.listChangeMu.Lock()
	m.onDeadHooks = append(m.onDeadHooks, fn)
	m.listChangeMu.Unlock()
}

func (m *Manager) notifyDead(sessionID string) {
	m.listChangeMu.RLock()
	hooks := append([]func(string){}, m.onDeadHooks...)
	m.listChangeMu.RUnlock()
	for _, fn := range hooks {
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
