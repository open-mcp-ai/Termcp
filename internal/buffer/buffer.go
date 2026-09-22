package buffer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
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

// Read returns bytes available ahead of the reader's cursor, then advances the cursor.
// If maxBytes > 0, at most that many bytes are returned and the cursor advances by that amount;
// if maxBytes == 0, all bytes from the cursor to the end of the buffer are returned.
func (b *Buffer) Read(ctx context.Context, readerID int, timeout time.Duration, maxBytes int) ([]byte, error) {
	return b.ReadLimited(ctx, readerID, timeout, maxBytes, 0)
}

// ReadLimited returns output with byte and line limits applied before advancing the cursor.
// If maxLines > 0, at most that many newline-terminated lines are returned; remaining
// bytes stay unread so HasMore stays true.
func (b *Buffer) ReadLimited(ctx context.Context, readerID int, timeout time.Duration, maxBytes, maxLines int) ([]byte, error) {
	b.mu.Lock()
	rs, ok := b.readers[readerID]
	if !ok {
		b.mu.Unlock()
		return nil, ErrReader
	}

	if data := b.drainLocked(rs, maxBytes, maxLines); data != nil {
		b.mu.Unlock()
		return data, nil
	}

	if b.closed {
		b.mu.Unlock()
		return nil, io.EOF
	}

	if timeout <= 0 {
		b.mu.Unlock()
		return nil, nil
	}

	deadline := time.Now().Add(timeout)
	stop := make(chan struct{})
	ctxDone := ctx.Done()
	timer := time.NewTimer(time.Until(deadline))
	go func() {
		select {
		case <-timer.C:
			b.cond.Broadcast()
		case <-ctxDone:
			b.cond.Broadcast()
		case <-stop:
		}
	}()
	defer func() {
		close(stop)
		timer.Stop()
	}()

	for rs.readPos >= int64(len(b.master)) && !b.closed {
		if time.Until(deadline) <= 0 {
			b.mu.Unlock()
			return nil, nil
		}
		if ctxDone != nil {
			select {
			case <-ctxDone:
				b.mu.Unlock()
				return nil, nil
			default:
			}
		}
		b.cond.Wait()
		if _, ok := b.readers[readerID]; !ok {
			b.mu.Unlock()
			return nil, ErrReader
		}
	}

	if data := b.drainLocked(rs, maxBytes, maxLines); data != nil {
		b.mu.Unlock()
		return data, nil
	}

	b.mu.Unlock()
	if b.closed {
		return nil, io.EOF
	}
	return nil, nil
}

// drainLocked copies up to one slice from readPos forward and advances readPos. b.mu held.
// maxBytes/maxLines limit how far the cursor advances; unread data stays available.
func (b *Buffer) drainLocked(rs *readerState, maxBytes, maxLines int) []byte {
	end := int64(len(b.master))
	if rs.readPos >= end {
		return nil
	}
	endPos := end
	if maxBytes > 0 {
		if lim := rs.readPos + int64(maxBytes); lim < endPos {
			endPos = lim
		}
	}
	if maxLines > 0 {
		lines := 0
		for i := rs.readPos; i < endPos; i++ {
			if b.master[i] == '\n' {
				lines++
				if lines >= maxLines {
					endPos = i + 1
					break
				}
			}
		}
	}
	s := b.master[rs.readPos:endPos]
	out := make([]byte, len(s))
	copy(out, s)
	rs.readPos = endPos
	return out
}

func (b *Buffer) HasMore(readerID int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs, ok := b.readers[readerID]
	if !ok {
		return false
	}
	return rs.readPos < int64(len(b.master))
}

// Cursor returns the reader's current raw stream position, or -1 if the reader
// is unknown. Lets callers report uniform start/end offsets for streaming reads.
func (b *Buffer) Cursor(readerID int) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rs, ok := b.readers[readerID]; ok {
		return b.baseOffset + rs.readPos
	}
	return -1
}

// BaseOffset returns the absolute offset of the earliest retained byte. Bytes
// before it were dropped by compaction and can no longer be read.
func (b *Buffer) BaseOffset() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.baseOffset
}

// WriteAt records that the bytes being written start at absolute offset offset.
//
// The buffer does not use this value to place the data (it appends); it only
// checks that its own numbering agrees with the caller's. That check is the point:
// the buffer and the log must count the same bytes from the same origin, and a
// mismatch means a byte was recorded on one side and not the other. Reporting it
// here turns a silent offset drift into an immediate, located error.
func (b *Buffer) WriteAt(data []byte, offset int64) error {
	b.mu.Lock()
	want := b.baseOffset + int64(len(b.master))
	b.mu.Unlock()
	if want != offset {
		return fmt.Errorf("buffer offset mismatch: have %d, caller says %d (%d bytes off)",
			want, offset, offset-want)
	}
	return b.Write(data)
}

// Len returns the current retained master size in bytes.
func (b *Buffer) Len() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.baseOffset + int64(len(b.master))
}

// ByteRange returns the retained bytes at absolute offset start and the absolute
// stream length. A start below the earliest retained byte clamps forward, so the
// caller can compare its request against BaseOffset to detect a shortened read.
// No reader cursors are advanced.
func (b *Buffer) ByteRange(start int64, max int) (out []byte, total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total = b.baseOffset + int64(len(b.master))
	if start < 0 {
		start = 0
	}
	// Translate the caller's absolute offset into a position in master.
	start -= b.baseOffset
	if start < 0 {
		start = 0
	}
	if start >= total || max <= 0 {
		return nil, total
	}
	n := int64(max)
	if start+n > total {
		n = total - start
	}
	if n <= 0 {
		return nil, total
	}
	out = make([]byte, n)
	copy(out, b.master[start:start+n])
	return out, total
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
