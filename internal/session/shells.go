package session

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Lock ordering: shellStateMu -> mu -> stdinMu (and shellStateMu -> cs.mu).
// Never acquire in reverse order. s.mu and cs.mu are leaves; shellStateMu is
// only taken by session-level transitions (close/DEAD/terminate, new shells).
// The manager-assigned callbacks and the per-shell closed flag are atomics and
// need no lock (see field docs below).

// resolveShellCommand applies the connection's default_shell when the caller
// sends no command of its own. It is the second step of the shell priority
// chain (caller → profile → server-side detection); an explicit command of any
// kind, including args-only, wins outright.
func (s *Session) resolveShellCommand(command string, args []string) (string, []string) {
	if strings.TrimSpace(command) == "" && len(args) == 0 && s.defaultShell != "" {
		if f := strings.Fields(s.defaultShell); len(f) > 0 {
			return f[0], f[1:]
		}
	}
	return command, args
}

// notifyChildChange invokes the on-child-change UI callback. The callback is
// assigned by Manager.Create after New returns, possibly while root-shell exit
// watchers are already running; it is an atomic pointer, so reads never race
// the assignment (a watcher firing in the assignment window just sees nil).
func (s *Session) notifyChildChange() {
	if fn := s.onChildChange.Load(); fn != nil {
		(*fn)()
	}
}

// removeChildShell deletes a shell from the parent's map and triggers UI notification.
func (s *Session) removeChildShell(id string) {
	if fn := s.onShellClose.Load(); fn != nil {
		(*fn)(id)
	}
	s.shells.Delete(id)
	s.notifyChildChange()
}

// CreateChildShell opens a new SSH session channel on the parent's existing SSH connection.
func (s *Session) CreateChildShell(command string, args []string, pty bool, rows, cols int, name string) (*ChildShell, error) {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()

	s.mu.RLock()
	running := s.Status == api.SessionRunning
	s.mu.RUnlock()
	if s.closing || !running {
		return nil, fmt.Errorf("session has exited")
	}

	sshClient := s.SSHClient()
	if sshClient == nil {
		return nil, fmt.Errorf("sub-shell multiplexing requires an SSH connection; internal loopback sessions do not support multiple channels")
	}
	id := newResourceID()

	// Shell resolution: the caller's command, else the profile's default_shell.
	// When both are empty the transport decides — a pty shell asks the server for
	// its login shell (see sshclient.startSession); a pipe shell has no such
	// request and is refused there rather than guessed from the client's PATH.
	command, args = s.resolveShellCommand(command, args)

	execSession, err := sshclient.StartWithClient(sshClient, command, args, pty, rows, cols)
	if err != nil {
		return nil, fmt.Errorf("create child shell: %w", err)
	}

	// Assign the channel number only after the SSH channel exists. Failed opens
	// do not create a shell and therefore do not consume a locator number; every
	// successful channel still gets a unique, monotonically increasing number
	// under shellStateMu.
	s.nextShellIndex++
	index := s.nextShellIndex
	if name == "" {
		// Keep the public default name backward-compatible. The stable channel
		// number used by locators is carried separately in Index; it is not
		// derived from or encoded into the user-visible name.
		name = fmt.Sprintf("shell-%s", id)
	}

	buf := buffer.New(1024 * 1024)
	buf.NewReader() // default reader 0 for MCP read_output

	mode := api.ModePipe
	if pty {
		mode = api.ModePTY
	}
	cs := &ChildShell{
		ID:          id,
		Index:       index,
		Name:        name,
		parent:      s,
		execSession: execSession,
		buf:         buf,
		done:        make(chan struct{}),
		Status:      api.SessionRunning,
		CreatedAt:   clock.Now(),
		Rows:        rows,
		Cols:        cols,
		enterCRLF:   s.enterCRLF,
		mode:        mode,
	}

	s.shells.Store(id, cs)
	s.shellHistory.Store(id, cs.Info())
	cs.startReaders()
	s.notifyChildChange()

	slog.Debug("child shell created", "parent_id", s.ID, "child_shell_id", id)
	return cs, nil
}

// CloseChildShell terminates and removes a child shell from the parent.
// Manual close is a DELETE, not a DEAD transition: the shell is dropped from
// the live map, the per-shell history snapshot, and any persisted restore, so
// it never reappears as a dead/"end" tab. Closing a shell — even the last one —
// leaves the container running: the session owns the SSH transport, so forwards,
// SFTP and new shells keep working with zero live shells.
func (s *Session) CloseChildShell(id string) error {
	v, ok := s.shells.Load(id)
	if !ok {
		return fmt.Errorf("child shell %q not found", id)
	}
	cs := v.(*ChildShell)

	// Mark closed first: TerminateShell and the exit watcher never retain a
	// closed shell, and the watcher's store-then-recheck (startReaders) deletes
	// any entry that raced the purge below. No lock needed between this store,
	// the purge, and the watcher: closed is an atomic bool, and the purge is
	// ordered after it (program order + atomic release/acquire).
	cs.closed.Store(true)
	cs.TerminateShell()
	cs.cleanupOnce.Do(func() {
		s.shellHistory.Delete(id)
		s.removeChildShell(id)
	})
	slog.Debug("child shell closed", "parent_id", s.ID, "child_shell_id", id)
	return nil
}

// GetChildShell returns a child shell by ID, or nil if not found.
func (s *Session) GetChildShell(id string) *ChildShell {
	v, _ := s.shells.Load(id)
	if v == nil {
		return nil
	}
	return v.(*ChildShell)
}

// ListChildShells returns public metadata for all shells of this session.
func (s *Session) ListChildShells() []api.Session {
	type entry struct {
		info      api.Session
		createdAt int64
	}
	var entries []entry
	s.shells.Range(func(_, v any) bool {
		cs := v.(*ChildShell)
		entries = append(entries, entry{info: cs.Info(), createdAt: cs.CreatedAt})
		return true
	})
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		// A live shell created through the session API always carries an Index, so
		// order by it: two channels created in the same millisecond would otherwise
		// fall back to map iteration order and make this list disagree with what a
		// locator resolves. Timestamps stay as the fallback for older shells.
		if a.info.Index > 0 && b.info.Index > 0 && a.info.Index != b.info.Index {
			return a.info.Index < b.info.Index
		}
		if a.createdAt == b.createdAt {
			return a.info.ID < b.info.ID
		}
		return a.createdAt < b.createdAt
	})
	out := make([]api.Session, len(entries))
	for i, e := range entries {
		out[i] = e.info
	}
	return out
}

// ShellByIndex resolves a shell channel by its channel index — the N of
// termcp://#<session>:N and the number the Web UI shows in the shell-N tab
// label. index <= 0 means the primary (first) shell. ok is false when the
// session has no such shell (including a session whose primary shell has
// already exited).
//
// The index is the one assigned at creation (ChildShell.Index), not a position
// in the current shell list: a position shifts when an earlier channel is
// closed, so a locator the user copied as :2 would silently start naming what
// used to be :3. Indexes are never reused either, so a locator for a closed
// channel fails cleanly instead of addressing its successor.
//
// Only a session where NO shell carries an index — shells built directly in
// tests, rather than through New/CreateChildShell — falls back to creation
// order, and only when the requested index lies within that list. That fallback
// is all-or-nothing on purpose: applying it per lookup once any shell is
// numbered is exactly the positional bug this method exists to avoid.
func (s *Session) ShellByIndex(index int) (*ChildShell, bool) {
	if index <= 0 {
		cs := s.PrimaryShell()
		return cs, cs != nil
	}
	var (
		found   *ChildShell
		indexed bool
		all     []*ChildShell
	)
	s.shells.Range(func(_, v any) bool {
		cs := v.(*ChildShell)
		all = append(all, cs)
		if cs.Index > 0 {
			indexed = true
		}
		if cs.Index == index {
			found = cs
			return false
		}
		return true
	})
	if found != nil {
		return found, true
	}
	if indexed {
		// The numbering is authoritative on this session, so an absent index means
		// the channel is gone — never the Nth remaining one.
		return nil, false
	}
	sortChildShellsByCreation(all)
	if idx := index - 1; idx >= 0 && idx < len(all) {
		return all[idx], true
	}
	return nil, false
}

// SnapshotShellByIndex resolves a retained (DEAD / restart-restored) shell by its
// channel index, using the index stored in the snapshot. It is the read-only
// counterpart of ShellByIndex: a dead session has no live shell objects, so its
// locator numbering comes from the persisted snapshot. SnapshotShells also
// backfills manifests that predate Index, so this method cannot accidentally
// make legacy sessions' locators disappear.
func (s *Session) SnapshotShellByIndex(index int) (api.Session, bool) {
	if index <= 0 {
		index = 1
	}
	for _, sh := range s.SnapshotShells() {
		if sh.Index == index {
			return sh, true
		}
	}
	return api.Session{}, false
}

// ShellIndexOutOfRangeError is the one wording for "that channel number names no
// channel". Every resolver returns it — MCP's live and DEAD paths and the REST
// /api/resolve handler — so the same locator cannot get three different
// explanations depending on which surface asked.
//
// count is how many channels the caller could see: live shells for a live
// session, retained ones for a DEAD session. The caller knows which set it
// searched, so it passes the number rather than this function guessing.
func ShellIndexOutOfRangeError(sessionID string, index, count int) error {
	return fmt.Errorf("shell index %d out of range (session %q has %d shell(s))", index, sessionID, count)
}

// PrimaryShellIndex returns the channel number the session's first shell is
// addressed by — the N of termcp://#<session>:N, which is 1 by construction.
//
// It is a method rather than a literal 1 at each call site because the number a
// response advertises must be the number the resolver will honour: a client
// labels its first tab from this value, so if the two ever disagreed the copy
// button would emit a locator that resolves to a different channel (issue #73).
// A shell that predates the Index field reports 1, which is the numbering the
// positional fallback in ShellByIndex would give it.
func (s *Session) PrimaryShellIndex() int {
	if cs := s.PrimaryShell(); cs != nil {
		if info := cs.Info(); info.Index > 0 {
			return info.Index
		}
	}
	return 1
}

// IsInternalPrimaryShell reports whether shellID names the primary channel of a
// session on the built-in loopback endpoint.
//
// Closing that channel is deliberately a no-op — the loopback process outlives
// the browser tab that displayed it — and that policy has to be identical on
// every surface that can close a shell (MCP shell_close and the Web UI's
// DELETE /api/shells/{id}). The rule lives here, once, so the two cannot drift
// into one surface deleting what the other refuses to.
func (s *Session) IsInternalPrimaryShell(shellID string) bool {
	return s.SSHEndpoint == "internal" && s.PrimaryShellID() == shellID
}

// HasLiveShells reports whether the session holds any live shell object — a
// running channel or an exited one still retained for draining its output. It is
// what tells a caller whether a locator should be resolved through the live map
// or through the DEAD/restored snapshot: a session whose primary shell was
// closed still has live channels, and resolving it through the snapshot would
// answer from the wrong set.
func (s *Session) HasLiveShells() bool {
	found := false
	s.shells.Range(func(_, _ any) bool {
		found = true
		return false
	})
	return found
}

// LiveShellCount returns how many live shell objects the session holds. It backs
// the "has N shell(s)" phrasing of locator errors.
func (s *Session) LiveShellCount() int {
	count := 0
	s.shells.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// sortShellsByCreation orders shells by creation time, then ID so the order is
// total and stable. The comparator is api.LessShellCreationOrder, shared with the
// storage layer that wrote the snapshot: both must agree on which shell a legacy
// manifest's termcp://#<sid>:N names, and a tie broken differently on each side
// would make the same locator resolve to different channels.
func sortShellsByCreation(shells []api.Session) {
	sort.Slice(shells, func(i, j int) bool {
		return api.LessShellCreationOrder(shells[i], shells[j])
	})
}

// sortChildShellsByCreation is the live-shell counterpart of
// sortShellsByCreation, used by the positional fallback in ShellByIndex.
func sortChildShellsByCreation(shells []*ChildShell) {
	sort.Slice(shells, func(i, j int) bool {
		return api.LessShellCreationOrder(shells[i].Info(), shells[j].Info())
	})
}

// SnapshotShells returns the last-known per-shell metadata (still populated for
// shells dropped from the live map on exit), sorted by creation time. This is
// what gets persisted and what a DEAD session renders into its tabs.
//
// Channel indexes are backfilled on the way out when they are missing, so every
// consumer of the snapshot — the persisted manifest, the locator resolver, the
// Web UI tabs — numbers the shells the same way the live session did.
func (s *Session) SnapshotShells() []api.Session {
	var out []api.Session
	s.shellHistory.Range(func(_, v any) bool {
		out = append(out, v.(api.Session))
		return true
	})
	sortShellsByCreation(out)
	// A snapshot from a manifest written before Index existed carries none, and a	// manifest that carries none for every shell is indistinguishable from an
	// unnumbered one. Fill them in creation order, which is what the numbering
	// counted before it was stored, so the index a locator resolves to cannot
	// depend on whether the session has been through a restart.
	backfillShellIndexes(out)
	return out
}

// ShellsForView returns shell metadata for session rendering. Running sessions
// report only their live channel children: naturally exited shells stay in the
// live map for output draining, and deliberately closed shells are deleted, so
// there is never a snapshot fallback here — a closed shell can never reappear
// as a dead/"end" tab in a live session. DEAD/restored sessions fall back to
// the retained shell snapshot (natural exits / transport aborts only) so their
// tabs survive transport teardown or a restart.
func (s *Session) ShellsForView() []api.Session {
	s.mu.RLock()
	status := s.Status
	s.mu.RUnlock()
	if status == api.SessionRunning {
		return s.ListChildShells()
	}
	return s.SnapshotShells()
}
