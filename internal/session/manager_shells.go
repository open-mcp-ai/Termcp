package session

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
