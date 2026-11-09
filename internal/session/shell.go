package session

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// ErrProcessGone reports that the channel's process has finished, so its stdin is
// gone. See ErrUnknownKey for why this is a sentinel: "the line you typed is
// wrong" and "the thing you typed it into is over" are answered by different
// statuses, and both travel through the same write.
var ErrProcessGone = errors.New("the process is gone")

// processGoneError carries ErrProcessGone without putting the sentinel's words in
// the message. The text is a sentence the caller reads ("process has exited,
// cannot send input"), and appending ": the process is gone" to it would be the
// same fact twice.
type processGoneError struct{ status string }

func (e *processGoneError) Error() string {
	return "process has " + e.status + ", cannot send input"
}
func (e *processGoneError) Is(target error) bool { return target == ErrProcessGone }

// NO LOCK HIERARCHY. s.mu is a leaf mutex for metadata; shell registration and
// teardown use atomic flags and re-checks (see shells.go), so channel opens dial
// outside any lock (AGENTS.md rule 1). Callbacks and closed flags are atomics.

// ChildShell is a lightweight shell channel sharing the parent Session's SSH connection.
//
// It is the only terminal the API surface addresses: a session is a connection
// container, and every read or write of terminal bytes goes to one of its shell
// channels. The dead TerminalShell interface used to suggest a *Session could
// stand in for one; nothing ever did that, and widening it back would reopen a
// second write path around the approval gate.
type ChildShell struct {
	ID string
	// Index is this channel's 1-based number inside the parent session (1 = the
	// first shell). It is assigned once at creation and never renumbered, so
	// termcp://#<session>:<index> keeps naming the same channel after other
	// channels are closed. 0 means "not assigned" (a shell built directly in a
	// test, or one restored from a manifest written before Index existed).
	Index       int
	Name        string
	command     string
	parent      atomic.Pointer[Session] // nil if not yet attached to a session
	execSession *sshclient.ExecSession
	buf         *buffer.Buffer
	done        chan struct{}
	closeOnce   sync.Once
	cleanupOnce sync.Once // guards explicit removal from the parent shell map
	// status is stored through atomic.Value so the zero value stays "" (a shell
	// built directly in a test), matching the old plain field. Load returns nil
	// until the constructor publishes running.
	status    atomic.Value // api.SessionStatus
	CreatedAt int64        // Unix ms
	exitCode  atomic.Pointer[int]
	rows      atomic.Int32
	cols      atomic.Int32
	// enterCRLF selects the byte sequence for pipe-mode enter.
	// It reflects the target shell family (unix vs cmd/powershell), not the
	// termcp host OS, so cross-OS SSH sessions send the right line ending.
	enterCRLF bool
	mode      api.SessionMode // pty or pipe; affects press_key("enter")
	// pipeWG tracks this shell's stdout/stderr reader goroutines so the final
	// bytes can be drained into the buffer before it is sealed. See drainPipes.
	pipeWG sync.WaitGroup
	// deliberateClose is set by TerminateShell/CloseChildShell so the exit
	// watcher does not treat an intentional channel close as SSH disconnect.
	deliberateClose atomic.Bool
	// closed marks a shell the user explicitly closed (CloseChildShell). A
	// closed shell is a DELETE, not a DEAD transition: it is removed from the
	// live map AND the per-shell history snapshot. Atomic so the exit watcher
	// and TerminateShell can check it without extra locking; see
	// CloseChildShell and the watcher's store-then-recheck in startReaders.
	closed atomic.Bool
	// archived marks a shell that was CLOSED but KEPT (ArchiveChildShell).
	//
	// Closing and erasing are two different acts and only one of them was
	// available: the tab's X called CloseChildShell, which stops the channel and
	// deletes the record, so a closed channel left nothing behind to look at —
	// no row in the archive, no entry in the session's snapshot, and "where did
	// that channel go?" had no answer anywhere in the UI. The transcript was on
	// disk the whole time; only the metadata that addresses it was gone.
	//
	// So a closed-but-kept shell sets this flag. It is out of the live map (its
	// tab must not come back) and IN the per-shell snapshot (with an exit status,
	// so a live session's view still filters it out while the archive — which
	// reads the snapshot — can show it). DELETE remains what it was: the erase.
	archived atomic.Bool
}

// statusValue reads the lifecycle status. The zero ChildShell reports "", the
// same value the old field held before a constructor ran.
func (cs *ChildShell) statusValue() api.SessionStatus {
	if v := cs.status.Load(); v != nil {
		return v.(api.SessionStatus)
	}
	return ""
}

// setStatus publishes a lifecycle transition.
func (cs *ChildShell) setStatus(st api.SessionStatus) { cs.status.Store(st) }

// exitCodeValue returns a copy of the process exit code, or nil while running.
func (cs *ChildShell) exitCodeValue() *int {
	p := cs.exitCode.Load()
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// setExitCode publishes the process exit code.
func (cs *ChildShell) setExitCode(code int) { cs.exitCode.Store(&code) }

// ParentSessionID returns the parent Session ID for this child shell.
func (cs *ChildShell) ParentSessionID() string {
	if p := cs.parent.Load(); p != nil {
		return p.ID
	}
	return ""
}

// Info returns a snapshot of the child shell's public metadata.
//
// THE STATUS IS THE WATCHER'S TO PUBLISH, and this deliberately does not correct it
// from the process. The exit watcher drains the last bytes and seals the buffer
// BEFORE storing "exited", and read paths take that ordering as a promise: a reader
// that sees `exited` may return an empty body, because the stream it was handed is
// complete. Reporting `exited` earlier — from the process channel — made a read
// return nothing while the transcript still had the tail in flight.
//
// A caller that must know whether a shell can still be written to asks
// SendTerminalBytes, which checks the process directly (see processGone).
func (cs *ChildShell) Info() api.Session {
	s := api.Session{
		ID:        cs.ID,
		Index:     cs.Index,
		Name:      cs.Name,
		Command:   cs.command,
		Mode:      cs.mode,
		Status:    cs.statusValue(),
		Rows:      int(cs.rows.Load()),
		Cols:      int(cs.cols.Load()),
		CreatedAt: cs.CreatedAt,
		UpdatedAt: clock.Now(),
	}
	s.ExitCode = cs.exitCodeValue()
	return s
}

// processGone reports whether the process has exited, whatever the published status
// says. It reads the earliest observable signal — the exec channel — and falls back
// to the finalization channel for a shell that has no exec session (a deserialized
// record, a transport-less shell).
func (cs *ChildShell) processGone() bool {
	if cs.execSession != nil {
		select {
		case <-cs.execSession.Done():
			return true
		default:
			return false
		}
	}
	select {
	case <-cs.done:
		return true
	default:
		return false
	}
}

// Done returns a channel that closes once the shell is finalized: the process
// has exited, its output is drained and sealed, and Status/ExitCode are
// published. A waiter that wakes on it may read the final transcript and exit
// code without racing the exit watcher.
func (cs *ChildShell) Done() <-chan struct{} {
	return cs.done
}

// SendTerminalBytes writes raw keystrokes to the child shell's stdin.
// pressEnter is kept for WebUI NL flag; MCP should use PressKey instead.
func (cs *ChildShell) SendTerminalBytes(data []byte, pressEnter bool) error {
	return cs.SendTerminalBytesFrom(data, pressEnter, InputFromAPI)
}

// SendTerminalBytesFrom writes raw keystrokes and records the bytes in the
// shell's log with the status of the given source.
//
// The bytes are logged whether or not the remote terminal echoes them: the log
// records what was sent, which is the only way to explain a transcript where the
// screen does not show the input (password prompts, full-screen programs).
func (cs *ChildShell) SendTerminalBytesFrom(data []byte, pressEnter bool, src InputSource) error {
	status := cs.statusValue()
	if status != api.SessionRunning {
		return &processGoneError{status: string(status)}
	}
	// THE EXIT IS PUBLISHED LATE, SO ASK THE PROCESS TOO.
	//
	// statusValue is written by the exit watcher AFTER it drains the last bytes and
	// publishes the exit code (that ordering is deliberate — a reader that sees
	// "exited" must find every byte). The cost is that there is a window in which the
	// process is gone and the status still says running, and a write in that window
	// reached the OS and came back as a bare `EOF`. Checking Done() costs nothing and
	// covers the whole window, because Done() closes as the final step of the same
	// watcher.
	//
	// It is a SELECT rather than a boolean so the closed-channel read is the only
	// thing that can succeed here: a Done() that is open must not block.
	//
	// TWO CHANNELS, because they close at different moments. execSession.Done() fires
	// the instant the process is gone; cs.done fires after the watcher has drained and
	// sealed the transcript. The gap between them is exactly the window this check
	// exists for, and only the earlier one covers it — querying cs.done alone left the
	// bare `EOF` in place, because the write happened before the watcher finished.
	if cs.execSession != nil {
		select {
		case <-cs.execSession.Done():
			return &processGoneError{status: "exited"}
		default:
		}
	}
	select {
	case <-cs.done:
		return &processGoneError{status: "exited"}
	default:
	}
	var toWrite []byte
	if pressEnter {
		toWrite = appendEnter(data, cs.enterCRLF)
	} else {
		toWrite = data
	}
	// Whether this input submits the line. Reported rather than inferred from the
	// output: the terminal's byte stream does not delimit lines (a redraw emits a
	// line break and a cursor-move sequence without ending anything), so a
	// consumer watching newlines sees a line end when the terminal only repainted.
	// The input side knows the truth — the user pressed enter.
	submit := submitsLine(toWrite)
	// Record the input *before* the bytes go out, and only when it submits the
	// line: the status display is live feedback that must not wait on the write,
	// and a keystroke that leaves the line open is not a span of the transcript.
	// See logInput for why a partial line writes nothing.
	cs.logInput(src, submit)
	// NO LOCK HERE: ExecSession owns the only write serialisation (a one-token
	// channel that a caller can give up on), and wrapping it in a second, unbounded
	// mutex was what turned one stuck network write into "every input on this channel
	// hangs" (issue #219).
	_, err := cs.execSession.WriteStdin(toWrite)
	if err != nil {
		return err
	}
	return nil
}

// PressKey writes a named key sequence (enter, ctrl+c, arrows, …) repeat times.
func (cs *ChildShell) PressKey(key string, repeat int) error {
	return cs.PressKeyFrom(key, repeat, InputFromAPI)
}

// PressKeyFrom writes a named key sequence and records it with the status of the
// given source.
func (cs *ChildShell) PressKeyFrom(key string, repeat int, src InputSource) error {
	if repeat < 1 {
		repeat = 1
	}
	if repeat > 20 {
		return fmt.Errorf("repeat must be between 1 and 20, got %d", repeat)
	}
	seq, err := KeyBytes(key, cs.mode == api.ModePTY, cs.enterCRLF)
	if err != nil {
		return err
	}
	status := cs.statusValue()
	if status != api.SessionRunning {
		return &processGoneError{status: string(status)}
	}
	payload := bytes.Repeat(seq, repeat)
	// A named key submits a line too (enter, and the ctrl sequences a line editor
	// treats as one), so the signal comes from the bytes actually sent rather than
	// from the key's name: the name is a label, the sequence is what the terminal
	// receives.
	submit := submitsLine(payload)
	cs.logInput(src, submit)
	_, err = cs.execSession.WriteStdin(payload)
	if err != nil {
		return err
	}
	return nil
}

// ResizePty adjusts the child shell's terminal dimensions. Only a pty shell has
// a terminal: a pipe shell has no TTY, so this reports an error instead of
// forwarding a meaningless window-change to the transport.
func (cs *ChildShell) ResizePty(rows, cols int) error {
	if cs.mode != api.ModePTY {
		return fmt.Errorf("PTY resize only available in pty mode (shell mode %q)", cs.mode)
	}
	if cs.statusValue() != api.SessionRunning {
		return fmt.Errorf("process not running")
	}
	if err := cs.execSession.ResizePty(rows, cols); err != nil {
		return err
	}
	cs.rows.Store(int32(rows))
	cs.cols.Store(int32(cols))
	return nil
}

// TerminateShell closes this shell's exec session channel without closing the
// parent Session's SSH client. The parent owns that connection, and the other
// child shells on it must keep running.
func (cs *ChildShell) TerminateShell() {
	if cs.statusValue() != api.SessionRunning {
		return // already terminated
	}
	cs.deliberateClose.Store(true)

	// ASK FIRST, THEN CLOSE (#293).
	//
	// A channel close is an EOF on the session's streams and nothing more. On POSIX
	// the remote sshd HUPs the process group, so most programs stop; on Windows there
	// is no SIGHUP at all, so `close <channel>` answered 204 and the program kept
	// running — holding a port the next task then failed to bind. The signal request
	// (RFC 4254 §6.9) is the protocol's own way to say "terminate this", and a termcp
	// server answers TERM/KILL by ending the whole process TREE, which is the only
	// thing that reaches a child its shell spawned and then waited on.
	//
	// Best-effort: a peer that does not implement signal requests (or a channel that
	// is already gone) leaves the close below to do what it always did.
	_ = cs.execSession.Signal(ssh.SIGTERM)

	cs.execSession.CloseSessionOnly()
	select {
	case <-cs.execSession.Done():
	case <-time.After(2 * time.Second):
	}
	// Flush output already handed to the process before sealing the buffer,
	// otherwise the tail of the transcript is dropped. The exit state is
	// published only after the bytes are final (same rule as the watcher).
	cs.drainPipes()
	cs.buf.Close()
	cs.setExitCode(-1)
	cs.setStatus(api.SessionExited)
	if parent := cs.parent.Load(); parent != nil {
		cs.retainHistory(parent)
	}
	// Ensure Done() channel is closed for any waiters (closeOnce prevents races with the exit watcher goroutine).
	cs.closeOnce.Do(func() { close(cs.done) })
}
