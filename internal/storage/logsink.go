package storage

import (
	"io"
	"os"
	"strings"
)

// sinkOp selects what the owner goroutine should do with one request.
type sinkOp uint8

const (
	opAppend sinkOp = iota
	opCloseShell
	opCloseSession
	opCloseAll
)

// sinkRequest is one unit of work for the owner. It is a value so a caller can
// set reply after the fields it knows; the reply channel is how the outcome gets
// back, since the owner is the one that can compute it.
type sinkRequest struct {
	op      sinkOp
	session string
	shell   string
	data    []byte
	reply   chan sinkResult
}

type sinkResult struct {
	offset int64
	err    error
}

// logSink owns every cached append handle and performs every append.
//
// An append is two steps that must not be separated: the offset the bytes landed
// at has to be read from the file, and that offset is what the caller is told the
// stream position of its bytes is. If another write happens in between, two
// callers are handed the same offset and one of them is wrong about where its
// bytes live.
//
// A mutex can serialize that pair, and one did. It was the wrong tool here for the
// same reason the per-session lock in the message package was: the handles it
// guarded are opened on first append and closed on shell delete or shutdown, so
// the lock would have to be looked up in a map and would have a lifetime shorter
// than the files it protects. Deletion is where such a design stops working - a
// writer holding the old lock and one holding its successor can both write the
// same file - and no amount of locking fixes a lock that can be replaced.
//
// So the handles have no lock and no shared home: they are local variables of the
// goroutine below, which is the only code in the process that opens, writes,
// seeks or closes them, and every operation on them is a request to that
// goroutine. The offset is decided by the same goroutine that performed the write,
// which makes it true by construction rather than by holding something across
// both steps.
type logSink struct {
	store *Store
	reqs  chan sinkRequest
}

func newLogSink(store *Store) *logSink {
	sink := &logSink{store: store, reqs: make(chan sinkRequest)}
	go sink.run()
	return sink
}

// run owns the open handles and is the only goroutine that touches them.
func (sink *logSink) run() {
	handles := make(map[string]*os.File)

	for req := range sink.reqs {
		switch req.op {
		case opAppend:
			f, err := sink.handle(handles, req.session, req.shell)
			if err != nil {
				req.reply <- sinkResult{err: err}
				continue
			}
			// The handle is O_APPEND, so the bytes land at the end of the file
			// whatever the position; this seek is only how that position is learned.
			off, err := f.Seek(0, io.SeekEnd)
			if err != nil {
				req.reply <- sinkResult{err: err}
				continue
			}
			if _, err := f.Write(req.data); err != nil {
				// The offset is reported anyway: the caller compares it against its
				// own view of the stream and needs to know where the gap starts.
				req.reply <- sinkResult{offset: off, err: err}
				continue
			}
			req.reply <- sinkResult{offset: off}

		case opCloseShell:
			k := logKey(req.session, req.shell)
			if f, ok := handles[k]; ok {
				_ = f.Close()
				delete(handles, k)
			}
			req.reply <- sinkResult{}

		case opCloseSession:
			prefix := req.session + "\x00"
			for k, f := range handles {
				if strings.HasPrefix(k, prefix) {
					_ = f.Close()
					delete(handles, k)
				}
			}
			req.reply <- sinkResult{}

		case opCloseAll:
			var firstErr error
			for k, f := range handles {
				if err := f.Close(); err != nil && firstErr == nil {
					firstErr = err
				}
				delete(handles, k)
			}
			req.reply <- sinkResult{err: firstErr}
		}
	}
}

// handle returns the cached append handle for a shell, opening it if needed.
// Runs on run's goroutine, which is why handles needs no lock.
func (sink *logSink) handle(handles map[string]*os.File, sessionID, shellID string) (*os.File, error) {
	k := logKey(sessionID, shellID)
	if f, ok := handles[k]; ok {
		return f, nil
	}

	dir := sink.store.shellDir(sessionID, shellID)
	if err := sink.store.initDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(sink.store.LogPath(sessionID, shellID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	handles[k] = f
	return f, nil
}

// do sends one request and waits for the owner to perform it. Waiting is what
// makes the request's data safe to reuse afterwards: the bytes have been written
// by the time this returns.
func (sink *logSink) do(req sinkRequest) sinkResult {
	// Buffered so the owner can always answer, even if this caller gives up.
	req.reply = make(chan sinkResult, 1)
	sink.reqs <- req
	return <-req.reply
}

func (sink *logSink) append(sessionID, shellID string, data []byte) (int64, error) {
	res := sink.do(sinkRequest{op: opAppend, session: sessionID, shell: shellID, data: data})
	return res.offset, res.err
}

func (sink *logSink) closeShell(sessionID, shellID string) {
	sink.do(sinkRequest{op: opCloseShell, session: sessionID, shell: shellID})
}

func (sink *logSink) closeSession(sessionID string) {
	sink.do(sinkRequest{op: opCloseSession, session: sessionID})
}

func (sink *logSink) closeAll() error {
	return sink.do(sinkRequest{op: opCloseAll}).err
}

// logKey identifies a shell's log for the handle cache. The NUL separator cannot
// appear in an ID (see validateID), so no two shells can share a key.
func logKey(sessionID, shellID string) string {
	return sessionID + "\x00" + shellID
}
