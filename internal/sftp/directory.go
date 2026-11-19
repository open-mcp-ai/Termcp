package sftp

import (
	"context"
	"os"
	"sort"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

const directoryLimit = 128

type DirectoryEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // Unix milliseconds.
}

type DirectoryListing struct {
	Type      string           `json:"type"`
	Entries   []DirectoryEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
	Error     string           `json:"error,omitempty"`
}

// IsDirectory tests the named entry, not a symlink's target.
func (s *Client) IsDirectory(path string) bool {
	fi, err := s.client.Lstat(path)
	return err == nil && fi.IsDir()
}

// ListDirectory returns one level of SFTP metadata.
// The existing SFTP v1 API collects a whole level before returning; the limit
// bounds what leaves this layer, not the server's READDIR traffic.
func (s *Client) ListDirectory(ctx context.Context, path string) (*DirectoryListing, error) {
	entries, err := s.client.ReadDirContext(ctx, path)
	if err != nil && len(entries) == 0 {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	list := &DirectoryListing{Type: "directory", Entries: make([]DirectoryEntry, 0, min(len(entries), directoryLimit)), Truncated: len(entries) > directoryLimit}
	if err != nil {
		list.Error = err.Error()
		list.Truncated = true
	}
	for _, e := range entries[:min(len(entries), directoryLimit)] {
		kind := "other"
		switch {
		case e.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		case e.IsDir():
			kind = "directory"
		case e.Mode().IsRegular():
			kind = "file"
		}
		list.Entries = append(list.Entries, DirectoryEntry{Name: e.Name(), Type: kind, Size: e.Size(), Mtime: clock.Millis(e.ModTime())})
	}
	return list, nil
}
