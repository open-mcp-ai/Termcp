package buffer

import (
	"context"
	"fmt"
	"io"
	"time"
)

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
	// timeout <= 0 means "do not wait", whatever the context says.
	if timeout <= 0 {
		b.mu.Unlock()
		return nil, nil
	}
	// The lock is held and there is nothing to deliver, so this call is going to
	// wait; everything the wait needs is set up below. A nil context is not
	// tolerated, matching the wait this replaces, which dereferenced it too.
	wake := b.wake
	ctxDone := ctx.Done()
	b.mu.Unlock()

	// A context that is already done is checked before waiting so a cancelled read
	// does not sleep for a timeout first.
	select {
	case <-ctxDone:
		return nil, nil
	default:
	}

	return b.waitForData(readerID, timeout, maxBytes, maxLines, wake, ctxDone)
}

// waitForData blocks until there is data, the deadline passes, the context is
// cancelled, the buffer closes, or the reader is unregistered, then delivers what
// it can. It is the slow path of ReadLimited: the caller has already taken the
// lock, found nothing to deliver, and established that a wait is wanted.
//
// The wait is a select over the deadline, the context and the buffer's wake
// channel, so ending it needs no other goroutine. This replaces a sync.Cond, which
// can only wait untimed: there, a deadline had to be delivered by a timer calling
// Broadcast from a helper goroutine, so every waiting read started a second
// goroutine. Here the caller's own goroutine does the waiting and the deadline is
// one of the things it waits on - measured, 24 waiters went from 48 goroutines to
// 24. The accuracy of the deadline is unchanged (both overshoot by about the same
// amount on this machine); what changes is how much a waiter costs.
func (b *Buffer) waitForData(readerID int, timeout time.Duration, maxBytes, maxLines int, wake <-chan struct{}, ctxDone <-chan struct{}) ([]byte, error) {
	deadline := time.Now().Add(timeout)

	for {
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

		// Every reason to stop is checked before waiting, and the wait can also end
		// on any of them. A wake can arrive for a change this reader does not care
		// about (another reader was unregistered), which is why this is a loop.
		if !time.Now().Before(deadline) {
			b.mu.Unlock()
			return nil, nil
		}
		select {
		case <-ctxDone:
			b.mu.Unlock()
			return nil, nil
		default:
		}

		// Take the channel the next change will close, then release the lock. A write
		// cannot land in between: it would need the lock, which is still held while
		// wake is read, and it replaces the channel as it releases it.
		wake = b.wake
		b.mu.Unlock()

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
//
// Both the argument and the reported total are absolute: baseOffset is added when
// the total is computed and subtracted when the argument is translated, so a
// caller can treat the stream as one unbroken sequence of bytes. That is what
// makes compaction invisible to callers - dropping a prefix moves baseOffset, not
// a byte - but it also means the two numbers belong to different scales, and the
// window must be clamped against the one that indexes master. Clamping against
// the absolute total allowed start+n to run past master, which the slice
// expression then rejected (or, when n still fit the capacity, satisfied with
// zeroed bytes that were never written).
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
	if max <= 0 || start >= int64(len(b.master)) {
		return nil, total
	}
	// Clamp against the retained bytes, which is what the slice below indexes.
	n := int64(max)
	if avail := int64(len(b.master)) - start; n > avail {
		n = avail
	}
	if n <= 0 {
		return nil, total
	}
	out = make([]byte, n)
	copy(out, b.master[start:start+n])
	return out, total
}
