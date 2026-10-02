package storage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// markIndexStride is how many marks pass between two entries of a shell's sparse
// index. log.jsonl is a sequence of JSON lines, so a byte offset cannot be looked
// up in it without knowing where the lines around that offset begin; one entry
// every 64 marks is enough to land within a stride of any offset, while costing
// one entry per 64 marks in memory. Nothing is written to disk for it: the file
// stays the only copy of the index.
const markIndexStride = 64

// markRef is one entry of that index: the byte position a mark's line starts at
// in log.jsonl and the log offset its span begins on.
//
// The pair is what makes a seek possible — the position is where to read, the
// offset is what to compare against the requested window. A line's length is not
// recorded because it is never needed: reading forward from a position finds the
// next line's end.
type markRef struct {
	Offset int64
	Pos    int64
}

// markIndex is a shell's sparse index plus how far into log.jsonl it reaches.
//
// It is built by reading the file once, in order, and extended by reading only
// what has been appended since, so a shell that streams for hours pays one line's
// parse per mark ever written however many windows are read out of it. A file
// that shrank is read again from its start rather than mixing two generations of
// one path.
//
// mu guards both fields and is held only for the duration of a refresh, which
// reads the appended bytes and nothing else.
type markIndex struct {
	mu    sync.Mutex
	refs  []markRef
	size  int64 // bytes of log.jsonl already consumed
	count int64 // marks scanned so far; the stride's counter
}

// refresh brings the index up to the file's current length. A missing file is an
// empty index rather than an error: the mark index is advisory, and a shell that
// has written no marks yet is not a failure.
func (i *markIndex) refresh(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			i.reset()
			return nil
		}
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	// A smaller file is a different file (truncated or replaced): its positions and
	// offsets have nothing to do with the ones already indexed.
	if st.Size() < i.size {
		i.reset()
	}
	if grew := st.Size() - i.size; grew > 0 {
		chunk := make([]byte, grew)
		if _, err := f.ReadAt(chunk, i.size); err != nil && err != io.EOF {
			return err
		}
		i.consume(chunk)
	}
	return nil
}

// consume indexes the whole lines at the head of chunk, leaving a torn trailing
// line for the next call: half a mark must never be counted as one, or every
// entry after it would name the wrong line.
func (i *markIndex) consume(chunk []byte) {
	last := bytes.LastIndexByte(chunk, '\n')
	if last < 0 {
		return
	}
	chunk = chunk[:last+1]
	pos := i.size
	for len(chunk) > 0 {
		nl := bytes.IndexByte(chunk, '\n')
		line := chunk[:nl]
		chunk = chunk[nl+1:]
		// A malformed line is skipped without counting it, exactly as a read skips
		// it: the index and the read must agree on which lines are marks.
		if len(line) > 0 {
			var m api.LogMark
			if json.Unmarshal(line, &m) == nil {
				if i.count%markIndexStride == 0 {
					i.refs = append(i.refs, markRef{Offset: m.Offset, Pos: pos})
				}
				i.count++
			}
		}
		pos += int64(nl + 1)
	}
	i.size = pos
}

func (i *markIndex) reset() {
	i.refs = nil
	i.size = 0
	i.count = 0
}

// ReadMarks reads a shell's status marks in file order.
//
// This is the whole index, for a caller that reasons over all of it (the MCP
// message list). A reader that only needs one screen's worth of a long log asks
// for a window through ReadMarksWindow instead.
func (s *Store) ReadMarks(sessionID, shellID string) ([]api.LogMark, error) {
	marks, _, err := s.readMarksWindow(sessionID, shellID, math.MinInt64, math.MaxInt64)
	return marks, err
}

// ReadMarksWindow reads a shell's status marks restricted to the byte window
// [start, end).
//
// A span's end is derived from the next mark, so the marks a window needs are
// more than the ones that start inside it: the returned slice is the part of the
// index that decides those bytes, in file order. It begins with the span the
// window starts inside — without it the first rows on screen have no status — and
// ends with the first mark at or after end, whose offset is returned separately as
// `closer`. The closer is not itself a span of the window: it is the boundary the
// last real span ends at, and a caller that wanted it as a span too would have to
// invent an end for it (the next mark's offset, or total_bytes) that this read
// cannot know.
//
// The point of the window is cost: the index is read through a sparse in-memory
// seek table (markIndex), so a request reads the marks the window contains plus
// at most one stride, no matter how long log.jsonl has grown. Callers that want
// everything pass ReadMarks, which is this with an unbounded window.
func (s *Store) ReadMarksWindow(sessionID, shellID string, start, end int64) (marks []api.LogMark, closer int64, err error) {
	// An id that names no shell is an empty answer, not an error: the mark index is
	// advisory (an empty or damaged one does not change what bytes exist), and a
	// session with no shells yet asks about exactly this.
	if validateID(sessionID) != nil || validateID(shellID) != nil {
		return nil, 0, nil
	}
	return s.readMarksWindow(sessionID, shellID, start, end)
}

// readMarksWindow is the windowed read without the id check, so the full read can
// share it: one scan path is what keeps a window and a full read from disagreeing
// about a span.
func (s *Store) readMarksWindow(sessionID, shellID string, start, end int64) ([]api.LogMark, int64, error) {
	refs, err := s.markCursors(sessionID, shellID)
	if err != nil {
		return nil, 0, err
	}
	if len(refs) == 0 {
		return nil, 0, nil
	}
	f, err := os.Open(s.MarkPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	// Starting at the last entry at or before the window is what bounds the scan:
	// a stride's worth of marks behind it, the marks the window contains, and the
	// one that closes them.
	at := refs[seekRef(refs, start)]
	if _, err := f.Seek(at.Pos, io.SeekStart); err != nil {
		return nil, 0, err
	}

	var out []api.LogMark
	var closer int64
	var below []api.LogMark // the marks at or before the window, newest offset group
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m api.LogMark
		if err := json.Unmarshal(line, &m); err != nil {
			// One malformed line (a torn append) must not discard the index.
			continue
		}
		if m.Offset <= start {
			// Below the window. Only the newest offset group is kept, because a later
			// mark below the window means the window starts inside that one instead.
			// The group, not just the last mark: marks may share an offset (a submitted
			// line and the review decision about it are both zero-length at the same
			// byte), and dropping the rest of the group would drop the row's status.
			if len(below) > 0 && below[len(below)-1].Offset != m.Offset {
				below = below[:0]
			}
			below = append(below, m)
			continue
		}
		if m.Offset >= end {
			// At or past the window's end: this mark is the boundary the span before it
			// ends at, and nothing after it can touch the window. It is reported as the
			// boundary rather than returned as a span, because its own span does not
			// overlap the window and its end would have to be invented.
			closer = m.Offset
			break
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if len(below) > 0 {
		out = append(below, out...)
	}
	return out, closer, nil
}

// seekRef is the index entry to start reading from for a window starting at
// start: the last entry at or before it, or the first entry when the window
// starts before every mark. The entries are in file order, so their offsets rise.
func seekRef(refs []markRef, start int64) int {
	i := sort.Search(len(refs), func(k int) bool { return refs[k].Offset > start })
	if i == 0 {
		return 0
	}
	return i - 1
}

// markCursors returns a snapshot of a shell's sparse index, extended to the
// file's current length.
//
// The returned slice is a view of entries that are only ever appended to or
// replaced wholesale: a refresh that runs while a read is scanning cannot change
// the entries that read already holds.
func (s *Store) markCursors(sessionID, shellID string) ([]markRef, error) {
	key := sessionID + "\x00" + shellID
	s.idxMu.Lock()
	idx := s.markIdx[key]
	if idx == nil {
		idx = &markIndex{}
		s.markIdx[key] = idx
	}
	s.idxMu.Unlock()

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if err := idx.refresh(s.MarkPath(sessionID, shellID)); err != nil {
		return nil, err
	}
	return idx.refs, nil
}

// dropMarkIndex forgets one shell's index. Called when the shell's files are
// removed: an index that outlived its file would answer a later shell that
// reused the id from positions in a log that no longer exists.
func (s *Store) dropMarkIndex(sessionID, shellID string) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	delete(s.markIdx, sessionID+"\x00"+shellID)
}

// dropSessionMarkIndex forgets every shell index under one session, for the same
// reason as dropMarkIndex. The separator in the key is what keeps a session id
// from matching a longer one that starts with it.
func (s *Store) dropSessionMarkIndex(sessionID string) {
	prefix := sessionID + "\x00"
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	for k := range s.markIdx {
		if strings.HasPrefix(k, prefix) {
			delete(s.markIdx, k)
		}
	}
}
