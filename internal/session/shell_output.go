package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Lock ordering: shellStateMu -> mu -> stdinMu (and shellStateMu -> cs.mu).
// Never acquire in reverse order. s.mu and cs.mu are leaves; shellStateMu is
// only taken by session-level transitions (close/DEAD/terminate, new shells).
// The manager-assigned callbacks and the per-shell closed flag are atomics and
// need no lock (see field docs below).

func (s *Session) readOutput(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, persist bool, maxBytes int) (string, error) {
	data, err := s.buf.ReadLimited(ctx, readerID, timeout, maxBytes, maxLines)
	if err != nil && err != io.EOF {
		return "", err
	}
	output := string(data)
	if stripAnsi {
		output = ansi.Strip(output)
		output = ansi.Compact(output)
	}
	// Output is recorded at the write source (pipeToBuffer), not here, so it is
	// recorded exactly once regardless of which reader consumes it.
	return output, nil
}

// ReadOutput reads new output using the default reader.
// maxBytes limits the returned output in bytes; 0 means no limit.
func (s *Session) ReadOutput(ctx context.Context, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	return s.readOutput(ctx, s.readerID, timeout, stripAnsi, maxLines, true, maxBytes)
}

// ReadOutputForReader reads new output for a specific reader ID.
// maxBytes limits the returned output in bytes; 0 means no limit.
func (s *Session) ReadOutputForReader(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	return s.readOutput(ctx, readerID, timeout, stripAnsi, maxLines, true, maxBytes)
}

// ReadTerminalStream reads PTY output for a reader without appending to the byte log (high-frequency UI streaming).
// If maxBytes > 0, each call returns at most that many raw bytes (for WebSocket/SSE chunking); 0 means one full drain to end of buffer.
func (s *Session) ReadTerminalStream(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	cs := s.PrimaryShell()
	if cs == nil {
		return "", fmt.Errorf("session shell has exited")
	}
	return cs.ReadTerminalStream(ctx, readerID, timeout, stripAnsi, maxLines, maxBytes)
}

// OutputByteRange returns a copy of retained raw output bytes [start, start+max) and total retained length.
func (s *Session) OutputByteRange(start int64, max int) ([]byte, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf == nil {
		return nil, 0, fmt.Errorf("output buffer unavailable")
	}
	data, total := s.buf.ByteRange(start, max)
	return data, total, nil
}

// OutputBaseOffset is the absolute offset of the earliest retained byte.
func (s *Session) OutputBaseOffset() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf == nil {
		return 0
	}
	return s.buf.BaseOffset()
}

// BufferLen returns retained raw output length in bytes (for tail slicing).
func (s *Session) BufferLen() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf == nil {
		return 0
	}
	return s.buf.Len()
}

// DefaultOutputReaderID is the first ring-buffer reader created with the session.
// MCP read_output defaults to this ID. The Web UI loads older bytes via GET /output-range and streams new output with RegisterReader().
func (s *Session) DefaultOutputReaderID() int {
	return s.readerID
}

// RegisterReaderSeededFromDefault registers a new output reader seeded from the default reader
// (atomic under buffer lock) so the web stream does not compete with MCP read_output on reader 0.
func (s *Session) RegisterReaderSeededFromDefault() (int, error) {
	return s.buf.NewReaderSeededFrom(s.readerID)
}

// RegisterReaderFromBufferStart registers a reader at the start of the retained transcript
// so Web UI / SSE clients replay full in-memory scrollback after reconnect.
func (s *Session) RegisterReaderFromBufferStart() (int, error) {
	return s.buf.NewReaderFromStart()
}

// RegisterReader creates a new independent reader and returns its ID.
func (s *Session) RegisterReader() (int, error) {
	return s.buf.NewReader()
}

// UnregisterReader removes a reader by ID.
func (s *Session) UnregisterReader(id int) {
	s.buf.Unregister(id)
}

func (s *Session) HasMoreOutput(readerID int) bool {
	return s.buf.HasMore(readerID)
}

// ReaderCursor returns the reader's current byte position in the retained buffer.
func (s *Session) ReaderCursor(readerID int) int64 {
	if s.buf == nil {
		return -1
	}
	return s.buf.Cursor(readerID)
}

func (s *Session) IsBufferClosed() bool {
	return s.buf.IsClosed()
}

// RegisterReader creates a new output reader for this child shell.
func (cs *ChildShell) RegisterReader() (int, error) {
	return cs.buf.NewReader()
}

// RegisterReaderFromBufferStart creates a reader seeded at the start of the retained transcript.
func (cs *ChildShell) RegisterReaderFromBufferStart() (int, error) {
	return cs.buf.NewReaderFromStart()
}

// UnregisterReader removes a reader by ID.
func (cs *ChildShell) UnregisterReader(id int) {
	cs.buf.Unregister(id)
}

// ReadTerminalStream reads PTY output for a reader without appending to the byte log.
func (cs *ChildShell) ReadTerminalStream(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	data, err := cs.buf.ReadLimited(ctx, readerID, timeout, maxBytes, maxLines)
	if err != nil && err != io.EOF {
		return "", err
	}
	output := string(data)
	if stripAnsi {
		output = ansi.Strip(output)
		output = ansi.Compact(output)
	}
	return output, nil
}

// HasMoreOutput returns whether the given reader has unread data.
func (cs *ChildShell) HasMoreOutput(readerID int) bool {
	return cs.buf.HasMore(readerID)
}

// ReaderCursor returns the reader's current byte position in the retained buffer.
func (cs *ChildShell) ReaderCursor(readerID int) int64 {
	if cs.buf == nil {
		return -1
	}
	return cs.buf.Cursor(readerID)
}

func (cs *ChildShell) IsBufferClosed() bool {
	return cs.buf.IsClosed()
}

// OutputByteRange returns a copy of retained raw output bytes [start, start+max) and total retained length.
func (cs *ChildShell) OutputByteRange(start int64, max int) ([]byte, int64, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.buf == nil {
		return nil, 0, fmt.Errorf("output buffer unavailable")
	}
	data, total := cs.buf.ByteRange(start, max)
	return data, total, nil
}

// OutputBaseOffset is the absolute offset of the earliest retained byte.
func (cs *ChildShell) OutputBaseOffset() int64 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.buf == nil {
		return 0
	}
	return cs.buf.BaseOffset()
}

// BufferLen returns retained raw output length in bytes.
func (cs *ChildShell) BufferLen() int64 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.buf == nil {
		return 0
	}
	return cs.buf.Len()
}

// pipeDrainGrace bounds how long a shell waits for the transport to hand over
// the bytes still buffered after the process reported exit. The normal path
// finishes in microseconds (the channel EOF follows exit-status immediately);
// the grace only covers a peer that never closes its channel.
const pipeDrainGrace = 2 * time.Second

// pipeToBuffer pipes child shell output into the buffer.
//
// The loop runs until the reader reports an error (normally io.EOF once the
// channel is closed after the process exits). It must NOT stop on cs.done: the
// SSH session reports exit-status as soon as the process is gone, while the
// trailing stdout may still be sitting unread in the channel buffer — bailing
// out on cs.done truncated the tail of fast commands.
func (cs *ChildShell) pipeToBuffer(r io.Reader) {
	p := cs.parent
	if p != nil {
		p.doneWG.Add(1)
	}
	cs.pipeWG.Add(1)
	go func() {
		if p != nil {
			defer p.doneWG.Done()
		}
		defer cs.pipeWG.Done()
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				// With a log attached, the log is written FIRST and the offset it
				// returns is the one the memory buffer is told to expect: a byte's
				// position is established by the durable log and the buffer only caches
				// it. If the two ever disagree, WriteAt reports it here instead of
				// letting every later read silently address the wrong byte.
				//
				// Order also matters for crash safety: a crash between the two writes
				// leaves a byte in the log that no reader saw, which is harmless. The
				// reverse would leave a reader positioned past the end of the durable
				// data.
				var werr error
				if p != nil && p.msgMgr != nil {
					off, lerr := p.msgMgr.AppendOutput(p.ID, cs.ID, chunk)
					if lerr != nil {
						// A failed append means the durable log did not take these bytes.
						// Keeping them only in memory would create exactly the drift this
						// design removes, so stop the transcript rather than serve a stream
						// that cannot be replayed.
						slog.Error("failed to append output to log; stopping transcript",
							"session_id", p.ID, "shell_id", cs.ID, "err", lerr)
						return
					}
					werr = cs.buf.WriteAt(chunk, off)
				} else {
					// No persistence configured: the buffer is the only record, so it
					// numbers its own bytes.
					werr = cs.buf.Write(chunk)
				}
				// A closed buffer means the shell was already sealed; nothing more can be
				// recorded, so stop rather than spin on a dead transcript.
				if werr != nil {
					slog.Error("output buffer rejected write; stopping transcript",
						"session_id", cs.ID, "err", werr)
					return
				}
				if p != nil {
					if fn := p.onOutput.Load(); fn != nil {
						(*fn)(cs.ID)
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// drainPipes waits for this shell's output pipe goroutines to consume the bytes
// buffered in the transport, so the transcript is complete before the buffer is
// sealed for readers. Callers must run it before cs.buf.Close().
//
// The wait stays bounded: a transport that never reports EOF gets its read side
// closed (which ends the stream as soon as the peer answers, and unblocks a
// reader stuck on a dead transport when the mux tears down).
func (cs *ChildShell) drainPipes() {
	drained := make(chan struct{})
	go func() {
		cs.pipeWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return
	case <-time.After(pipeDrainGrace):
	}
	slog.Debug("output pipe still open after process exit; closing readers", "child_shell_id", cs.ID)
	if cs.execSession != nil {
		cs.execSession.CloseReaders()
	}
	select {
	case <-drained:
	case <-time.After(pipeDrainGrace):
		slog.Debug("output pipe did not drain; sealing buffer anyway", "child_shell_id", cs.ID)
	}
}

// retainHistory keeps the shell's final metadata for the retained per-shell
// tabs, unless the shell was explicitly closed — closed shells are deleted,
// never retained.
func (cs *ChildShell) retainHistory(p *Session) {
	if cs.closed.Load() {
		return
	}
	p.shellHistory.Store(cs.ID, cs.Info())
}

// startChildReaders starts the stdout/stderr pipe goroutines and exit watcher for a child shell.
func (cs *ChildShell) startReaders() {
	cs.pipeToBuffer(cs.execSession.Stdout)
	cs.pipeToBuffer(cs.execSession.Stderr)

	if p := cs.parent; p != nil {
		p.watchWG.Add(1)
	}
	go func() {
		if p := cs.parent; p != nil {
			defer p.watchWG.Done()
		}
		<-cs.execSession.Done()
		cs.closeOnce.Do(func() { close(cs.done) })
		cs.mu.Lock()
		cs.Status = api.SessionExited
		code := cs.execSession.ExitCode()
		cs.ExitCode = &code
		cs.mu.Unlock()
		// Retain this shell's final metadata for the retained per-shell tabs —
		// unless the user explicitly closed it. Store-then-recheck: a manual close
		// racing this store re-deletes the entry below, so a closed shell can
		// never survive in the retained snapshot (no lock needed: CloseChildShell
		// always sets closed before its purge).
		if p := cs.parent; p != nil {
			cs.retainHistory(p)
			if cs.closed.Load() {
				p.shellHistory.Delete(cs.ID)
			}
		}
		// Drain the output still buffered in the transport (exit-status can
		// arrive ahead of the last stdout bytes) before sealing the buffer.
		cs.drainPipes()
		cs.buf.Close()
		// Keep the exited ChildShell in the parent map until explicit close/Delete.
		// This preserves its closed buffer so shell_output and WebUI can drain
		// final output after a fast pipe command has already exited.
		cs.mu.RLock()
		deliberate := cs.deliberateClose
		reparent := cs.parent
		// Snapshot the exit code under the lock: a concurrent
		// CloseChildShell/TerminateShell writes cs.ExitCode while the watcher runs.
		var exitCode *int
		if cs.ExitCode != nil {
			v := *cs.ExitCode
			exitCode = &v
		}
		cs.mu.RUnlock()
		if reparent != nil {
			if fn := reparent.onShellExit.Load(); fn != nil {
				(*fn)(cs.ID, exitCode)
			}
			reparent.notifyChildChange()
		}
		// If the shell ended due to SSH disconnect (not deliberate close and not
		// clean process exit), tear down the session. exitOnce ensures once.
		if reparent != nil && !deliberate && cs.execSession.Aborted() {
			slog.Debug("session DEAD via transport abort", "session_id", reparent.ID, "child_shell_id", cs.ID)
			reparent.markDeadWithMessage("❌ SSH connection lost — network disconnected")
		}
		// A shell ending — cleanly or not — never ends the container. The session
		// owns the SSH transport, and that transport is what carries forwards, SFTP
		// and new shell channels; a run-to-exit pipe command finishing (or every
		// shell being closed) must leave all of those working. Only an aborted
		// transport, session_terminate, or manager shutdown flips a session DEAD.
		slog.Debug("child shell exited", "child_shell_id", cs.ID, "exit_code", code)
	}()
}
