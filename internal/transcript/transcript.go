// Package transcript is the transport-neutral read side of a shell's
// transcript: resolve an id (raw shell id, session id, or termcp:// locator)
// onto one byte stream, then read windows of it - by offset, tail, line
// number, or live cursor - with the stream filter applied.
//
// It exists because MCP and the HTTP API both serve this read path and must
// never disagree about what a window is: both surfaces are thin adapters that
// parse their own arguments, call Resolve + Read here, and map the typed
// errors onto their own status vocabulary. The domain semantics live once,
// here; the reading of the underlying files happens through
// message.Manager (log.bin/log.jsonl/log.lines/log.stdio), which both
// surfaces already shared.
//
// Every error is a *Error carrying a Kind, so a caller can map it to
// invalid-argument / not-found / reader / operation without string matching.
package transcript

import (
	"fmt"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/term/locator"
)

// ErrKind classifies a transcript read failure for status mapping.
type ErrKind int

const (
	// ErrInvalid marks an argument outside the read contract.
	ErrInvalid ErrKind = iota
	// ErrShellNotFound marks an id that names no shell.
	ErrShellNotFound
	// ErrSessionNotFound marks an id that names no session.
	ErrSessionNotFound
	// ErrReader marks a live-cursor read on an unregistered reader.
	ErrReader
	// ErrOperation marks an I/O failure while serving the read.
	ErrOperation
)

// Error is the one error type the package returns.
type Error struct {
	Kind ErrKind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func errf(kind ErrKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// Source is ONE shell's raw output byte stream, regardless of the shell's
// lifecycle state: a live shell (including an exited-but-retained one) streams
// from its in-memory buffer; a DEAD / restart-restored session streams from the
// shell's persisted log.bin, read positionally - the same byte sequence the
// in-memory buffer once held. Both expose the same cursor semantics (Len +
// ByteRange), which is what makes one Read serve every state.
type Source struct {
	Live    *session.ChildShell // non-nil ⇒ live source
	SessID  string
	ShellID string           // resolved shell id; "" = merged persisted stream
	MsgMgr  *message.Manager // line/stream indexes over the persisted log
	Status  api.SessionStatus
	Created int64 // Unix ms; live sources only (for session_uptime_seconds)
}

// SourceName reports where a read of this source serves bytes from.
func (o *Source) SourceName() string {
	if o.Live != nil {
		return "live"
	}
	return "persisted"
}

// Len returns the total raw stream length in bytes.
//
// THE LOG IS THE RECORD AND THE BUFFER IS A CACHE OF IT, so a live source's length is
// the LARGER of the two. shell_output.go appends to the log first and writes the buffer
// second, and the reason for that order is crash safety — but it also means that
// between those two lines the same channel answers two different lengths depending on
// which record the caller's flag happens to read: `--from-line` reads the log,
// `--tail` and `--offset` read the buffer. With the gap widened to 1.5s on Linux,
// `read <ch> --from-line 1` returned the bytes while `read <ch> --tail 20` answered
// `204` with `Content-Range: bytes */0` — a false fact about a channel that had them,
// and the reason a GBK test failed on one CI runner and not another.
func (o *Source) Len() (int64, error) {
	n := int64(0)
	if o.Live != nil {
		n = o.Live.BufferLen()
	}
	if o.MsgMgr == nil {
		return n, nil
	}
	m, err := o.MsgMgr.OutputSize(o.SessID, o.ShellID)
	if err != nil {
		// A log that cannot be read is not a reason to refuse a buffer that works.
		if o.Live != nil {
			return n, nil
		}
		return 0, err
	}
	if m > n {
		return m, nil
	}
	return n, nil
}

// ByteRange copies raw bytes [start, start+max) of the stream. No cursors or
// reader state is touched, so positional reads are safe for concurrent
// readers and for DEAD sessions alike.
func (o *Source) ByteRange(start int64, max int) ([]byte, int64, error) {
	if o.Live != nil {
		// THE TWO WAYS A BUFFER AND ITS LOG CAN DISAGREE ARE OPPOSITE ENDS OF IT.
		//
		// The buffer can be BEHIND (the log is appended first — see Len) or AHEAD of
		// where its own record starts (compaction drops a prefix every reader has
		// passed, and with no reader at all it drops everything). Either one makes the
		// buffer's answer wrong in a way the caller cannot see, so both are answered
		// from the log, which is the record and is complete from offset 0.
		if o.MsgMgr != nil && start < o.Live.OutputBaseOffset() {
			// The head: the buffer dropped these bytes, the log still has them. Serving
			// a clamped range instead would put a start in Content-Range that the caller
			// did not ask for and cannot reconcile.
			return o.MsgMgr.OutputByteRange(o.SessID, o.ShellID, start, max)
		}
		data, total, err := o.Live.OutputByteRange(start, max)
		if o.MsgMgr == nil || (err == nil && len(data) >= max) {
			return data, total, err
		}
		// The tail: the buffer has not been given these bytes yet. Fill from the log at
		// the offset right after what the buffer DID serve, so nothing is repeated.
		rest, logTotal, lerr := o.MsgMgr.OutputByteRange(o.SessID, o.ShellID, start+int64(len(data)), max-len(data))
		if lerr != nil {
			// Nothing else can answer, so the buffer's own result stands — including its
			// error, which is what a caller with no log at all has always seen.
			return data, total, err
		}
		if logTotal > total {
			total = logTotal
		}
		return append(data, rest...), total, nil
	}
	if o.MsgMgr == nil {
		return nil, 0, nil
	}
	return o.MsgMgr.OutputByteRange(o.SessID, o.ShellID, start, max)
}

// BaseOffset is the absolute offset of the earliest byte this source can serve.
//
// ZERO WHENEVER THERE IS A LOG, because the log is the record and it starts at zero:
// the in-memory buffer may have dropped a prefix (compaction), but a caller asking for
// that prefix gets it from the log, so telling them it is gone would be a fact about
// the cache reported as a fact about the stream. Without a log the buffer IS the record,
// and then its base is the honest answer — bytes before it were dropped and cannot be
// served by anything.
func (o *Source) BaseOffset() int64 {
	if o.Live != nil && o.MsgMgr == nil {
		return o.Live.OutputBaseOffset()
	}
	return 0
}

// Resolve maps an id onto the unified output stream. The id may be:
//   - a shell_id: that shell (live, or persisted per-shell stream);
//   - a session_id: the primary shell (live), or the merged output stream
//     (DEAD / restart-restored session);
//   - a termcp:// locator: a session, optionally one of its shell channels
//     by stable channel index;
//   - the short handle `<session>:<N>` that ps/info/pty print, which names the
//     same channel as the locator's `:N` form.
//
// Resolution order: live shells → live registry sessions (incl. restored
// DEAD). Every valid session lives in the registry; there is no hidden archive.
func Resolve(sessMgr *session.Manager, msgMgr *message.Manager, id string) (*Source, error) {
	// One decomposition for every spelling a handle may take: a raw id, a
	// `termcp://` locator, or the short `sid:N` form `ps` prints. Reading has to
	// agree with writing — a `run` whose bytes reached channel N must not then be
	// read back from the primary, or answer "not found" and drop the output it
	// produced.
	//
	// The entry kind is the one handle that names no session, so it is reported
	// here rather than silently treated as an id.
	shellIdx := 0
	if locator.LooksLike(id) {
		p, perr := locator.Parse(id)
		if perr != nil {
			return nil, errf(ErrInvalid, "%s", perr.Error())
		}
		if p.Kind == locator.KindEntry {
			return nil, errf(ErrInvalid, "resource URL %q names an entry, not a session or shell", id)
		}
		id, shellIdx = p.SessionID, p.Index
	} else if h := locator.SplitHandle(id); h.Index > 0 {
		id, shellIdx = h.SessionID, h.Index
	}

	// Locator channel form (:N): the target is that specific shell.
	if shellIdx > 0 {
		sess := sessMgr.Get(id)
		if sess == nil {
			return nil, errf(ErrSessionNotFound, "session %q not found", id)
		}
		// THIS CHANNEL DECIDES THE BRANCH, NOT THE SESSION.
		//
		// The test used to be `sess.HasLiveShells()` — "does this session still hold
		// any live shell" — which is a question about the SESSION being asked about
		// ONE channel. A closed channel is REMOVED from the live map and moved to the
		// retained snapshot, so on a session whose other channels are still running
		// that test said "live", the index lookup searched only the live map, and the
		// archived channel came back as "index out of range" — its transcript intact
		// on disk and unreachable through the handle the caller had just used.
		//
		// Asking about the channel fixes it and keeps the property the old comment
		// was protecting: the live map answers whenever it holds THIS index, so a
		// session whose first channel was closed still resolves :N from the same set
		// the Web UI shows.
		if cs, ok := sess.ShellByIndex(shellIdx); ok {
			info := cs.Info()
			return &Source{Live: cs, SessID: sess.ID, ShellID: cs.ID, MsgMgr: msgMgr, Status: info.Status, Created: info.CreatedAt}, nil
		}
		// Not live: a closed-but-kept channel, or a DEAD/restored session. Both are
		// addressed by the channel index retained in the snapshot, which is the same
		// number the channel carried while it ran.
		shells := sess.SnapshotShells()
		if sh, ok := sess.SnapshotShellByIndex(shellIdx); ok {
			return &Source{SessID: sess.ID, ShellID: sh.ID, MsgMgr: msgMgr, Status: sess.Info().Status}, nil
		}
		return nil, errf(ErrShellNotFound, "%s", session.ShellIndexOutOfRangeError(sess.ID, shellIdx, len(shells)).Error())
	}

	if cs := sessMgr.GetChildShell(id); cs != nil {
		info := cs.Info()
		// The shell already knows its parent, so asking the manager would be a second
		// full scan of every session to learn what cs.ParentSessionID returns directly.
		// GetByShellID would find this same session, because the only way
		// GetChildShell(id) can return cs is from that session's own shell map.
		sessID := cs.ParentSessionID()
		if sessID == "" {
			// Not reachable for a shell found through a session's shell map (both the
			// root and every child are stored with their parent already set), but the
			// value is reported as the session id rather than empty if it ever is.
			sessID = id
		}
		return &Source{Live: cs, SessID: sessID, ShellID: cs.ID, MsgMgr: msgMgr, Status: info.Status, Created: info.CreatedAt}, nil
	}
	if sess := sessMgr.Get(id); sess != nil {
		// A bare session id means the primary CHANNEL (index 1) wherever it now
		// lives: the live map while it is there, else its retained log. Keying on
		// PrimaryShell() is therefore about which channel, not about live vs DEAD —
		// the distinction the :N path draws with HasLiveShells().
		//
		// The one case that reads oddly is a running session whose primary channel
		// was manually closed (possible on a remote endpoint; the internal primary
		// refuses to close): the id then answers from the persisted log while other
		// live channels exist. That is deliberate rather than overlooked — a bare
		// id asks for channel 1, and picking some other channel because it happens
		// to be alive would answer a different question than the one asked.
		if cs := sess.PrimaryShell(); cs != nil {
			info := cs.Info()
			return &Source{Live: cs, SessID: sess.ID, ShellID: cs.ID, MsgMgr: msgMgr, Status: info.Status, Created: info.CreatedAt}, nil
		}
		// Restored DEAD session: no live shell objects, so the persisted log of
		// this session's first shell serves as the stream. The shell id must be
		// resolved here — a log belongs to a shell, so an empty id would address a
		// shell that does not exist and read back nothing. Channel index 1 is the
		// primary channel by definition, so this is the same shell the live path
		// would have picked. The fallback covers a snapshot with no index 1 at all
		// (a partially numbered one, which backfillShellIndexes deliberately leaves
		// untouched rather than renumber and risk a duplicate): the first shell in
		// creation order is then the only sensible answer.
		shellID := ""
		if sh, ok := sess.SnapshotShellByIndex(1); ok {
			shellID = sh.ID
		} else if shells := sess.SnapshotShells(); len(shells) > 0 {
			shellID = shells[0].ID
		}
		return &Source{SessID: sess.ID, ShellID: shellID, MsgMgr: msgMgr, Status: sess.Info().Status}, nil
	}
	if sess := sessMgr.GetSessionByShellID(id); sess != nil {
		return &Source{SessID: sess.ID, ShellID: id, MsgMgr: msgMgr, Status: sess.Info().Status}, nil
	}
	return nil, errf(ErrShellNotFound, "Shell '%s' not found", id)
}

// shellFromIndex resolves a live channel by its channel index on a session
// (0 = the primary shell). Read-only paths that must also serve
// DEAD/restart-restored sessions resolve through Session.SnapshotShellByIndex —
// this function is the live half.
func shellFromIndex(sess *session.Session, index int) (*session.ChildShell, error) {
	if cs, ok := sess.ShellByIndex(index); ok {
		return cs, nil
	}
	if index <= 0 {
		// No primary shell left: callers report the plain "has no shell" error.
		return nil, nil
	}
	return nil, session.ShellIndexOutOfRangeError(sess.ID, index, sess.LiveShellCount())
}
