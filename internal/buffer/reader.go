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
