package session

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"
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
	id := uuid.New().String()[:12]
	if name == "" {
		name = fmt.Sprintf("shell-%s", id)
	}

	// Shell resolution: the caller's command, else the profile's default_shell.
	// When both are empty the transport decides — a pty shell asks the server for
	// its login shell (see sshclient.startSession); a pipe shell has no such
	// request and is refused there rather than guessed from the client's PATH.
	command, args = s.resolveShellCommand(command, args)

	execSession, err := sshclient.StartWithClient(sshClient, command, args, pty, rows, cols)
	if err != nil {
		return nil, fmt.Errorf("create child shell: %w", err)
	}

	buf := buffer.New(1024 * 1024)
	buf.NewReader() // default reader 0 for MCP read_output

	mode := api.ModePipe
	if pty {
		mode = api.ModePTY
	}
	cs := &ChildShell{
		ID:          id,
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].createdAt < entries[j].createdAt })
	out := make([]api.Session, len(entries))
	for i, e := range entries {
		out[i] = e.info
	}
	return out
}

// ShellByIndex resolves a shell channel by its 1-based creation order — the
// numbering the Web UI shows as shell-1/shell-2 tabs and that termcp://
// locators use (see internal/locator). index <= 0 means the primary (first)
// shell. ok is false when the session has no such shell (including a session
// whose primary shell has already exited).
func (s *Session) ShellByIndex(index int) (*ChildShell, bool) {
	if index <= 0 {
		cs := s.PrimaryShell()
		return cs, cs != nil
	}
	all := s.ListChildShells()
	if idx := index - 1; idx >= 0 && idx < len(all) {
		cs := s.GetChildShell(all[idx].ID)
		return cs, cs != nil
	}
	return nil, false
}

// SnapshotShells returns the last-known per-shell metadata (still populated for
// shells dropped from the live map on exit), sorted by creation time. This is
// what gets persisted and what a DEAD session renders into its tabs.
func (s *Session) SnapshotShells() []api.Session {
	var out []api.Session
	s.shellHistory.Range(func(_, v any) bool {
		out = append(out, v.(api.Session))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
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
