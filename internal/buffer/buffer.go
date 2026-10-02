package buffer

import (
	"errors"
	"sync"
)

// DefaultMaxBytes is kept for API compatibility with buffer.New(maxBytes); sizing is no longer enforced.
const DefaultMaxBytes = 1024 * 1024

var (
	ErrClosed = errors.New("buffer: closed")
	ErrReader = errors.New("buffer: invalid reader ID")
)

type readerState struct {
	readPos int64 // next byte offset in master to deliver
}

// Buffer is a thread-safe multi-reader append-only log of process output.
// All readers share one master byte slice; each reader has an independent read cursor.
// Fully consumed prefixes are dropped when compactThreshold is exceeded to bound memory.
type Buffer struct {
	mu      sync.Mutex
	master  []byte
	readers map[int]*readerState
	nextID  int
	closed  bool
	cond    *sync.Cond
	// baseOffset is the absolute stream offset of master[0]: the number of bytes
	// this buffer has dropped from the front through compaction. Every offset this
	// type reports is absolute (baseOffset + a position in master), so dropping a
	// prefix to reclaim memory never moves a byte.
	//
	// The same numbering is used by the on-disk log (a byte's offset is its file
	// position), so a live read and a persisted read of the same byte agree.
	baseOffset        int64
	compactThreshold  int // compact when len(master) > this
	compactMinAdvance int // and min(readPos) >= this
}

// New creates a Buffer. maxBytes is ignored (historical ring capacity); output grows without a fixed cap.
func New(maxBytes int) *Buffer {
	_ = maxBytes // API compatibility; no hard limit on retained history
	b := &Buffer{
		readers:           make(map[int]*readerState),
		compactThreshold:  8 << 20, // 8 MiB before attempting prefix trim
		compactMinAdvance: 1 << 20, // require ≥1 MiB reclaimable prefix
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// NewReader registers a new independent reader that only observes writes after registration.
func (b *Buffer) NewReader() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	id := b.nextID
	b.nextID++
	b.readers[id] = &readerState{readPos: int64(len(b.master))}
	return id, nil
}

// NewReaderFromStart registers a reader at the beginning of the retained master buffer
// (full in-memory scrollback for UI reconnect). Caller must drain this reader or it pins old bytes from compaction.
func (b *Buffer) NewReaderFromStart() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	id := b.nextID
	b.nextID++
	b.readers[id] = &readerState{readPos: 0}
	return id, nil
}

// NewReaderSeededFrom registers a new reader whose cursor starts at srcReaderID's cursor
// (same logical position — no duplicate copy of backlog).
func (b *Buffer) NewReaderSeededFrom(srcReaderID int) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	src, ok := b.readers[srcReaderID]
	if !ok {
		return 0, ErrReader
	}
	id := b.nextID
	b.nextID++
	b.readers[id] = &readerState{readPos: src.readPos}
	return id, nil
}

func (b *Buffer) Unregister(id int) {
	b.mu.Lock()
	delete(b.readers, id)
	b.mu.Unlock()
	b.cond.Broadcast()
}

// Write appends data for all readers and wakes waiters.
func (b *Buffer) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	b.master = append(b.master, data...)
	b.maybeCompactLocked()
	b.cond.Broadcast()
	return nil
}

// maybeCompactLocked drops a prefix of master that every reader has already passed.
// Caller must hold b.mu.
func (b *Buffer) maybeCompactLocked() {
	if len(b.master) <= b.compactThreshold {
		return
	}
	minPos := int64(len(b.master))
	for _, rs := range b.readers {
		if rs.readPos < minPos {
			minPos = rs.readPos
		}
	}
	if minPos < int64(b.compactMinAdvance) {
		return
	}
	b.master = b.master[minPos:]
	// Absorb the drop into baseOffset so the absolute numbering is unchanged;
	// reader positions are relative to master and so must move too.
	b.baseOffset += minPos
	for _, rs := range b.readers {
		rs.readPos -= minPos
	}
}

func (b *Buffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *Buffer) IsClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}
