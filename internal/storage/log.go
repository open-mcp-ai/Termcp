package storage

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// AppendLog appends raw bytes to a shell's log.bin and returns the offset the
// data was written at.
//
// The returned offset is the file position, which is what makes an offset
// meaningful without any bookkeeping: it is a fact about the file, not a sum of
// lengths that can drift out of agreement with the bytes.
//
// The append happens through a cached handle so a burst of small writes does not
// pay an open() per write. Callers must AppendMark *after* this returns so a
// crash can only ever leave a trailing span with no mark (attributed to the
// previous status), never a mark pointing past the end of the data.
func (s *Store) AppendLog(sessionID, shellID string, data []byte) (int64, error) {
	if err := validateID(sessionID); err != nil {
		return 0, err
	}
	if err := validateID(shellID); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return s.LogSize(sessionID, shellID)
	}
	return s.logSink.append(sessionID, shellID, data)
}

// AppendMark appends one status transition to a shell's log.jsonl.
//
// Append this after the corresponding AppendLog: the mark then always points at
// a byte that exists. The reverse order would produce marks pointing past EOF,
// which cannot be repaired.
func (s *Store) AppendMark(sessionID, shellID string, mark api.LogMark) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	if err := validateID(shellID); err != nil {
		return err
	}
	dir := s.shellDir(sessionID, shellID)
	if err := s.initLogDir(sessionID, shellID); err != nil {
		return err
	}
	line, err := json.Marshal(mark)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	// Marks are a handful of bytes per status change, so open/append/close keeps
	// their durability story simple and never holds a second handle open.
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// LogSize returns the current size of a shell's log.bin, i.e. the offset just
// past the last byte ever written.
func (s *Store) LogSize(sessionID, shellID string) (int64, error) {
	st, err := os.Stat(s.LogPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return st.Size(), nil
}

// ReadLog reads at most max bytes of a shell's log.bin starting at offset.
//
// This is a positional read: it never consults log.jsonl and never accumulates
// lengths, so a window is exactly the file's bytes at that position.
func (s *Store) ReadLog(sessionID, shellID string, offset int64, max int) ([]byte, error) {
	if offset < 0 {
		offset = 0
	}
	if max <= 0 {
		return nil, nil
	}
	f, err := os.Open(s.LogPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, max)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}
