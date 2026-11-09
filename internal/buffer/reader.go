package buffer

import (
	"context"
	"fmt"
	"io"
	"time"
)

// TrimTail says how many trailing bytes of a window cannot complete a character, so
// the read hands back whole ones and leaves the rest for the next read.
//
// THE BUFFER OWNS BYTES AND KNOWS NOTHING ABOUT TEXT. Where a character ends depends
// on the charset, the charset is a property of the READ REQUEST, and the caller is the
// only one holding it — so the rule travels with the call rather than being guessed
// here. nil means "cut exactly where I said": a caller reading raw bytes (the browser
// repainting a terminal, a frame) has no characters to protect.
type TrimTail func(window []byte) int

// Read returns bytes available ahead of the reader's cursor, then advances the cursor.
// If maxBytes > 0, at most that many bytes are returned and the cursor advances by that amount;
// if maxBytes == 0, all bytes from the cursor to the end of the buffer are returned.
func (b *Buffer) Read(ctx context.Context, readerID int, timeout time.Duration, maxBytes int) ([]byte, error) {
	return b.ReadLimited(ctx, readerID, timeout, maxBytes, 0, nil)
}

// ReadLimited returns output with byte and line limits applied before advancing the cursor.
// If maxLines > 0, at most that many lines are returned; remaining
// bytes stay unread so HasMore stays true.
//
// trim, when non-nil, withholds a trailing sequence the caller's charset cannot
// complete. Those bytes stay UNREAD, so the next read delivers them whole: the cursor
// advances by what was actually returned, which is the same rule every other window
// here follows.
//
// NO LOCK IS TAKEN ON THIS PATH. The snapshot and the caller's own cursor are both
// atomic loads, and the copy out of the log runs while writers keep appending past
// it — see logState on why the two regions cannot overlap.
func (b *Buffer) ReadLimited(ctx context.Context, readerID int, timeout time.Duration, maxBytes, maxLines int, trim TrimTail) ([]byte, error) {
	rs, ok := b.lookup(readerID)
	if !ok {
		return nil, ErrReader
	}
	if data := b.drain(rs, maxBytes, maxLines, trim); data != nil {
		return data, nil
	}
	if b.state().closed {
		return nil, io.EOF
	}
	// timeout <= 0 means "do not wait", whatever the context says.
	if timeout <= 0 {
		return nil, nil
	}
	// A context that is already done is checked before waiting so a cancelled read
	// does not sleep for a timeout first.
	ctxDone := ctx.Done()
	select {
	case <-ctxDone:
		return nil, nil
	default:
	}
	return b.waitForData(rs, readerID, timeout, maxBytes, maxLines, trim, ctxDone)
}

// waitForData blocks until there is data, the deadline passes, the context is
// cancelled, the buffer closes, or the reader is unregistered, then delivers what
// it can. It is the slow path of ReadLimited: the caller has already found nothing
// to deliver and established that a wait is wanted.
//
// The wait is a select over the deadline, the context and the buffer's wake channel,
// so ending it needs no other goroutine. This replaces a sync.Cond, which can only
// wait untimed: there, a deadline had to be delivered by a timer calling Broadcast
// from a helper goroutine, so every waiting read started a second goroutine. Here the
// caller's own goroutine does the waiting and the deadline is one of the things it
// waits on - measured, 24 waiters went from 48 goroutines to 24.
func (b *Buffer) waitForData(rs *reader, readerID int, timeout time.Duration, maxBytes, maxLines int, trim TrimTail, ctxDone <-chan struct{}) ([]byte, error) {
	deadline := time.Now().Add(timeout)

	for {
		// TAKE THE WAKE CHANNEL FIRST, THEN LOOK FOR DATA. The order is the whole
		// correctness argument: a write after this capture closes the channel captured
		// here, and a write before it is already in the bytes the drain below sees. Look
		// first and capture second and the two can meet in the middle — the write lands,
		// closes the old channel, installs a new one, and this waiter then sits on the
		// new one until its deadline.
		wake := b.wakeGen()
		// Re-look the reader up: it may have been unregistered while waiting, and the
		// next iteration can be entered long after this call started.
		if _, ok := b.lookup(readerID); !ok {
			return nil, ErrReader
		}
		if data := b.drain(rs, maxBytes, maxLines, trim); data != nil {
			return data, nil
		}
		if b.state().closed {
			return nil, io.EOF
		}

		// Every reason to stop is checked before waiting, and the wait can also end
		// on any of them. A wake can arrive for a change this reader does not care
		// about (another reader was unregistered), which is why this is a loop.
		if !time.Now().Before(deadline) {
			return nil, nil
		}
		select {
		case <-ctxDone:
			return nil, nil
		default:
		}

		// time.After is an allocation per call, but this path only runs when a wait is
		// actually needed. A read that finds data never reaches here.
		select {
		case <-wake:
		case <-time.After(time.Until(deadline)):
			return nil, nil
		case <-ctxDone:
			return nil, nil
		}
	}
}

// drain copies up to one window from the reader's cursor forward and advances the
// cursor. Unread data stays available: maxBytes/maxLines limit how far the cursor moves.
//
// The cursor is the reader's own atomic; the bytes come from one snapshot. A
// concurrent append writes past that snapshot's length and a concurrent compaction
// only moves the snapshot's base, so the window copied here stays valid either way.
func (b *Buffer) drain(rs *reader, maxBytes, maxLines int, trim TrimTail) []byte {
	st := b.state()
	pos := rs.pos.Load()
	// A cursor that fell behind a compaction has lost those bytes and continues from
	// the earliest one still held, which is the same answer Seek gives it.
	if pos < st.base {
		pos = st.base
		rs.pos.Store(pos)
	}
	end := st.base + int64(len(st.data))
	if pos >= end {
		return nil
	}
	start := int(pos - st.base)
	endPos := int(end - st.base)
	if maxBytes > 0 {
		if lim := start + maxBytes; lim < endPos {
			endPos = lim
		}
	}
	if maxLines > 0 {
		lines := 0
		for i := start; i < endPos; i++ {
			if st.data[i] == '\n' {
				lines++
				if lines >= maxLines {
					endPos = i + 1
					break
				}
			}
		}
	}
	s := st.data[start:endPos]
	// THE CUT IS SOMEBODY ELSE'S DECISION. Trimming a trailing incomplete character
	// needs the charset, which the buffer does not have and must not learn: it is a
	// byte log, and the charset belongs to the read request. So the caller passes the
	// rule (see TrimTail), and this is the one place it is applied — after every limit
	// has been applied, before the cursor moves, because the withheld bytes have to
	// stay unread for the next read to deliver them whole.
	if trim != nil {
		if drop := trim(s); drop > 0 && drop < len(s) {
			s = s[:len(s)-drop]
		}
	}
	out := make([]byte, len(s))
	copy(out, s)
	rs.pos.Store(pos + int64(len(s)))
	return out
}

func (b *Buffer) HasMore(readerID int) bool {
	rs, ok := b.lookup(readerID)
	if !ok {
		return false
	}
	st := b.state()
	return rs.pos.Load() < st.base+int64(len(st.data))
}

// Cursor returns the reader's current raw stream position, or -1 if the reader
// is unknown. Lets callers report uniform start/end offsets for streaming reads.
func (b *Buffer) Cursor(readerID int) int64 {
	rs, ok := b.lookup(readerID)
	if !ok {
		return -1
	}
	return rs.pos.Load()
}

// Seek moves one reader's cursor to an absolute stream offset.
//
// It exists because a POSITIONAL read (offset, from_line, tail_lines) does not move
// any cursor, and a cursor that never moves is a cursor that lies: `read <channel>`
// promises "the new bytes since the last read", while every `run` reads positionally —
// so after a series of runs the first plain read replayed the whole channel from byte
// zero (issue #162, S2). The caller had already seen those bytes; the reader had no
// way to know.
//
// Backwards is allowed on purpose: `--offset 0` is a legal read, and a caller that
// pages back and forth should not have its cursor pinned forward by that. Callers that
// want "never rewind" ask for it (see advanceCursor in the mcp layer) rather than
// getting it silently here.
//
// An offset before the retained window is clamped to the earliest byte still held,
// because those bytes are gone: refusing would make a legal read fail for a reason the
// caller cannot act on, and the reply's Content-Range already says where it really
// started. An offset past the end is refused — that one is a caller mistake, and
// silently clamping it forward would skip bytes the caller has not seen.
func (b *Buffer) Seek(readerID int, pos int64) error {
	rs, ok := b.lookup(readerID)
	if !ok {
		return ErrReader
	}
	st := b.state()
	end := st.base + int64(len(st.data))
	if pos > end {
		return fmt.Errorf("offset %d is past the end of the stream (%d)", pos, end)
	}
	if pos < st.base {
		pos = st.base
	}
	rs.pos.Store(pos)
	// A reader parked at the new position may now have data (or may no longer be the
	// reason a waiter is blocked), so waiters re-check. Seeking backwards over data a
	// waiter is already blocked on is the case that needs this: without the wake it
	// would sit there while bytes it can now read are available.
	b.wakeAll()
	return nil
}

// BaseOffset returns the absolute offset of the earliest retained byte. Bytes
// before it were dropped by compaction and can no longer be read.
func (b *Buffer) BaseOffset() int64 {
	return b.state().base
}

// WriteAt records that the bytes being written start at absolute offset offset.
//
// The buffer does not use this value to place the data (it appends); it only
// checks that its own numbering agrees with the caller's. That check is the point:
// the buffer and the log must count the same bytes from the same origin, and a
// mismatch means a byte was recorded on one side and not the other. Reporting it
// here turns a silent offset drift into an immediate, located error.
func (b *Buffer) WriteAt(data []byte, offset int64) error {
	if want := b.Len(); want != offset {
		return fmt.Errorf("buffer offset mismatch: have %d, caller says %d (%d bytes off)",
			want, offset, offset-want)
	}
	return b.Write(data)
}

// Len returns the current absolute stream length: every byte written, including the
// prefix compaction has dropped.
func (b *Buffer) Len() int64 {
	st := b.state()
	return st.base + int64(len(st.data))
}

// ByteRange returns the retained bytes at absolute offset start and the absolute
// stream length. A start below the earliest retained byte clamps forward, so the
// caller can compare its request against BaseOffset to detect a shortened read.
// No reader cursors are advanced.
//
// Both the argument and the reported total are absolute: base is added when
// the total is computed and subtracted when the argument is translated, so a
// caller can treat the stream as one unbroken sequence of bytes. That is what
// makes compaction invisible to callers - dropping a prefix moves base, not
// a byte.
func (b *Buffer) ByteRange(start int64, max int) (out []byte, total int64) {
	st := b.state()
	total = st.base + int64(len(st.data))
	if start < 0 {
		start = 0
	}
	// Translate the caller's absolute offset into a position in the retained log.
	pos := start - st.base
	if pos < 0 {
		pos = 0
	}
	if max <= 0 || pos >= int64(len(st.data)) {
		return nil, total
	}
	// Clamp against the retained bytes, which is what the slice below indexes.
	n := int64(max)
	if avail := int64(len(st.data)) - pos; n > avail {
		n = avail
	}
	if n <= 0 {
		return nil, total
	}
	out = make([]byte, n)
	copy(out, st.data[pos:pos+n])
	return out, total
}
