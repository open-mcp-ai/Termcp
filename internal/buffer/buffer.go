package buffer

import (
	"errors"
	"sync"
	"sync/atomic"
)

// DefaultMaxBytes is kept for API compatibility with buffer.New(maxBytes); sizing is no longer enforced.
const DefaultMaxBytes = 1024 * 1024

var (
	ErrClosed = errors.New("buffer: closed")
	ErrReader = errors.New("buffer: invalid reader ID")
)

// reader is one independent cursor into the stream. Its position is ABSOLUTE and
// belongs to that reader alone, which is what makes reading lock-free: a read copies
// bytes out and advances one atomic, and touches nothing another goroutine owns.
//
// Absolute rather than relative-to-retained matters because compaction then moves
// nothing: dropping a prefix raises the snapshot's base, and every cursor still means
// the byte it always meant.
type reader struct{ pos atomic.Int64 }

// logState is the whole log as one immutable value. Published by pointer store; the
// bytes below len(data) are frozen once a reader can see them.
//
// APPENDING WRITES PAST A PUBLISHED LENGTH, NEVER INTO IT. append() into spare
// capacity mutates the same backing array a reader may be copying out of, and that is
// safe precisely because the two regions do not overlap: the reader stops at the length
// it loaded, the writer starts there. When capacity runs out append allocates and
// copies, and a reader holding the old snapshot keeps reading the old array — a
// complete, unchanging prefix of the same stream.
type logState struct {
	data   []byte // retained bytes; data[0] is absolute offset base
	base   int64  // absolute offset of data[0]
	closed bool
}

// Buffer is a multi-reader append-only log of process output.
//
// WHAT NEEDS A LOCK AND WHAT DOES NOT. Writes are ordered by nature — the bytes of one
// stream arrive in sequence — so writers take turns on a 1-slot token; that token is
// also what lets compaction and reader creation be mutually exclusive with a write.
// READS TAKE NOTHING: they load the snapshot, copy, and advance their own cursor. The
// memcpy of terminal output, which used to happen while every writer waited, is now
// outside any shared ownership.
type Buffer struct {
	log      atomic.Pointer[logState]
	writeTok chan struct{} // 1-slot: one appending writer at a time
	readers  sync.Map      // int -> *reader
	nextID   atomic.Int64

	// wake is closed, and replaced, whenever something a waiter could be waiting
	// for changes: a write, a close, or a reader being unregistered or seeked. A
	// waiter reads the current channel and only then re-checks its condition, so a
	// change cannot slip between the check and the wait.
	wake atomic.Pointer[chan struct{}]

	compactThreshold  int // compact when the retained log exceeds this
	compactMinAdvance int // and at least this much is reclaimable
}

// New creates a Buffer. maxBytes is ignored (historical ring capacity); output grows without a fixed cap.
func New(maxBytes int) *Buffer {
	_ = maxBytes // API compatibility; no hard limit on retained history
	b := &Buffer{
		writeTok:          make(chan struct{}, 1),
		compactThreshold:  8 << 20, // 8 MiB before attempting prefix trim
		compactMinAdvance: 1 << 20, // require ≥1 MiB reclaimable prefix
	}
	b.log.Store(&logState{})
	ch := make(chan struct{})
	b.wake.Store(&ch)
	b.writeTok <- struct{}{}
	return b
}

// state is the current log. One atomic load; the value is never mutated afterwards.
func (b *Buffer) state() *logState {
	if st := b.log.Load(); st != nil {
		return st
	}
	// Zero-value Buffer: publish an empty log once, and lose the race harmlessly.
	st := &logState{}
	if b.log.CompareAndSwap(nil, st) {
		return st
	}
	return b.log.Load()
}

// retained exposes the bytes still held, for tests in this package.
func (b *Buffer) retained() []byte { return b.state().data }

// beginWrite takes the writer token: the 1-slot channel arrives holding one value, so
// acquiring it is a receive and releasing is the send back.
func (b *Buffer) beginWrite() { <-b.writeTok }

// endWrite releases it.
func (b *Buffer) endWrite() { b.writeTok <- struct{}{} }

// wakeGen returns the channel that the next change will close, creating one if this
// is a zero-value Buffer. A waiter takes it as its last act before waiting.
func (b *Buffer) wakeGen() <-chan struct{} {
	if ch := b.wake.Load(); ch != nil {
		return *ch
	}
	fresh := make(chan struct{})
	if b.wake.CompareAndSwap(nil, &fresh) {
		return fresh
	}
	return *b.wake.Load()
}

// wakeAll tells every waiter the buffer changed: swap in a fresh channel, close the one
// waiters are on. Closing wakes all of them, which is sync.Cond.Broadcast's semantics,
// and the replacement means the next waiter has something to select on that nobody has
// closed yet. A waiter may be woken for a change it did not want (another reader was
// unregistered), which re-enters the wait having re-checked its condition.
func (b *Buffer) wakeAll() {
	for {
		cur := b.wake.Load()
		if cur == nil {
			// Publish a channel nobody waits on yet, then retry.
			fresh := make(chan struct{})
			if b.wake.CompareAndSwap(nil, &fresh) {
				return
			}
			continue
		}
		next := make(chan struct{})
		if b.wake.CompareAndSwap(cur, &next) {
			close(*cur)
			return
		}
	}
}

// NewReader registers a new independent reader that only observes writes after registration.
func (b *Buffer) NewReader() (int, error) {
	b.beginWrite()
	defer b.endWrite()
	st := b.state()
	if st.closed {
		return 0, ErrClosed
	}
	return b.addReader(st.base + int64(len(st.data))), nil
}

// NewReaderFromStart registers a reader at the beginning of the retained buffer
// (full in-memory scrollback for UI reconnect). Caller must drain this reader or it pins old bytes from compaction.
func (b *Buffer) NewReaderFromStart() (int, error) {
	b.beginWrite()
	defer b.endWrite()
	st := b.state()
	if st.closed {
		return 0, ErrClosed
	}
	return b.addReader(st.base), nil
}

// NewReaderSeededFrom registers a new reader whose cursor starts at srcReaderID's cursor
// (same logical position — no duplicate copy of backlog).
func (b *Buffer) NewReaderSeededFrom(srcReaderID int) (int, error) {
	b.beginWrite()
	defer b.endWrite()
	if b.state().closed {
		return 0, ErrClosed
	}
	src, ok := b.readers.Load(srcReaderID)
	if !ok {
		return 0, ErrReader
	}
	return b.addReader(src.(*reader).pos.Load()), nil
}

// addReader stores a cursor. Caller holds writeTok, which is what keeps a compaction
// from dropping bytes this reader is being started on.
func (b *Buffer) addReader(pos int64) int {
	// IDs start at 0: the session layer's default reader is "reader 0" by name.
	id := int(b.nextID.Add(1)) - 1
	r := &reader{}
	r.pos.Store(pos)
	b.readers.Store(id, r)
	return id
}

// lookup returns one reader by id.
func (b *Buffer) lookup(id int) (*reader, bool) {
	v, ok := b.readers.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*reader), true
}

func (b *Buffer) Unregister(id int) {
	b.readers.Delete(id)
	// A reader blocked on this id must learn it is gone, so this wakes too.
	b.wakeAll()
}

// Write appends data for all readers and wakes waiters.
func (b *Buffer) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	b.beginWrite()
	defer b.endWrite()
	st := b.state()
	if st.closed {
		return ErrClosed
	}
	next := &logState{data: append(st.data, data...), base: st.base}
	b.compact(next)
	b.log.Store(next)
	b.wakeAll()
	return nil
}

// compact drops a prefix of the log that every reader has already passed. It edits the
// private snapshot the writer is about to publish, so no reader ever sees it.
//
// THE POSITIONS ARE READ WITHOUT A LOCK, and that is deliberate: a reader may seek
// backwards between this scan and the publish, in which case those bytes are gone and
// the read clamps to the new base — the same answer a seek to a dropped offset gets.
func (b *Buffer) compact(next *logState) {
	if len(next.data) <= b.compactThreshold {
		return
	}
	minPos := int64(len(next.data))
	b.readers.Range(func(_, v any) bool {
		if p := v.(*reader).pos.Load() - next.base; p < minPos {
			minPos = p
		}
		return true
	})
	if minPos < int64(b.compactMinAdvance) {
		return
	}
	next.data = next.data[minPos:]
	// Absorb the drop into base so the absolute numbering is unchanged, and the
	// cursors — already absolute — need no adjustment at all.
	next.base += minPos
}

func (b *Buffer) Close() {
	b.beginWrite()
	defer b.endWrite()
	st := b.state()
	if st.closed {
		return
	}
	b.log.Store(&logState{data: st.data, base: st.base, closed: true})
	b.wakeAll()
}

func (b *Buffer) IsClosed() bool {
	return b.state().closed
}
