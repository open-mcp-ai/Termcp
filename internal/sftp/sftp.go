package sftp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// ErrIsDirectory reports that a read named a directory.
//
// A sentinel rather than a formatted string because the MCP layer classifies it: a
// directory is what the caller asked for and cannot use, which is a 400, while an
// SSH_FX_FAILURE from the same reader is a genuine fault and stays a 500.
var ErrIsDirectory = errors.New("is a directory")

// ErrBinaryFile reports that a text read found a NUL byte, so the content is not
// text. Classified as a 400 like ErrIsDirectory: the caller asked for a rendering
// these bytes do not have, and the message names the two modes that do.
var ErrBinaryFile = errors.New("binary file cannot be read as text")

// ReplaceInFile errors: exact matching only.
var (
	ErrPatternNotFound  = errors.New("pattern not found in file")
	ErrPatternAmbiguous = errors.New("pattern matched more than once; must match uniquely")
)

// FileResult holds the result of a file_read operation.
type FileResult struct {
	// Data is a POINTER so "the inline body is empty" and "this read has no inline
	// body" stay distinguishable.
	//
	// mode=file streams to a local path and never fills Data; mode=text/hex always
	// does, even when the window is empty. A plain string with no omitempty made the
	// first case report `data: ""` — a body that looks truncated rather than absent —
	// while keeping omitempty made the second case drop the field on a zero-length
	// read, which is the one read where seeing a zero matters.
	//
	// Length and BytesRead keep no omitempty for the same reason they always needed
	// it: 0 is a fact about the window, not a missing value.
	Data      *string `json:"data,omitempty"`
	Mode      string  `json:"mode"` // "text" | "hex" | "file"
	Encoding  string  `json:"encoding,omitempty"`
	TotalSize int64   `json:"total_size"`
	HasMore   bool    `json:"has_more,omitempty"`
	Offset    int64   `json:"offset,omitempty"`
	Length    int64   `json:"length"`
	BytesRead int64   `json:"bytes_read"`
	LocalPath string  `json:"local_path,omitempty"`
}

// FileStatResult holds the result of a file_stat operation.
type FileStatResult struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	ModTime int64  `json:"mod_time,omitempty"` // Unix ms; 0 = unknown
	// Mode is the permission bits as a caller would chmod them ("0664").
	//
	// IT IS HERE BECAUSE "IS THIS EXECUTABLE" WAS UNANSWERABLE (issue #191). A
	// script that a caller had just written failed `open --argv` with "permission
	// denied", and every surface that could have said "mode 664" instead reported
	// size and mtime: the caller went looking at mounts, users and paths while the
	// fact sat in the stat response nobody had asked for.
	Mode string `json:"mode,omitempty"`
	// Uid and Gid name the owner, which is the second half of "why can this not be
	// run/read": a 755 file owned by someone else is still not yours to write.
	Uid      string           `json:"uid,omitempty"`
	Gid      string           `json:"gid,omitempty"`
	Children []FileStatResult `json:"children,omitempty"`
}

// sftpClient abstracts SFTP operations.
type Client struct {
	client *sftp.Client
	// shared marks a Client that uses somebody else's engine, so Close is a no-op.
	// It is a field rather than a second type because every operation below is the
	// same call either way — only the lifetime differs, and lifetime is what this flag
	// records.
	shared bool
}

// Shared wraps an existing engine as a Client whose Close does nothing.
//
// WHY A VIEW RATHER THAN A REFERENCE COUNT: the engine's lifetime is its session's, and
// a caller cannot know when the last of several concurrent calls finishes. Handing out
// an owned-looking Client would invite `defer Close()`, which would leave every later
// call holding a dead engine — the bug this type exists to make impossible to write.
func Shared(engine *sftp.Client) *Client {
	return &Client{client: engine, shared: true}
}

// Close closes the SFTP client, unless it is shared.
func (s *Client) Close() error {
	if s.shared {
		// Nothing to close: the session owns the engine and closes it when the session
		// ends. Silently succeeding is the honest answer — the caller's contract is
		// "release what this call opened", and this call opened nothing.
		return nil
	}
	return s.client.Close()
}

// RemoveFile deletes a remote file.
func (s *Client) RemoveFile(remotePath string) error {
	return s.client.Remove(remotePath)
}

// RenameFile moves or renames a remote file/directory (same filesystem).
func (s *Client) RenameFile(oldPath, newPath string) error {
	return s.client.Rename(oldPath, newPath)
}

// MakeDir creates a directory (and parents) on the remote.
func (s *Client) MakeDir(remotePath string) error {
	return s.client.MkdirAll(remotePath)
}

// StatFile returns file/directory info.
func (s *Client) StatFile(remotePath string) (*FileStatResult, error) {
	fi, err := s.client.Stat(remotePath)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	result := &FileStatResult{
		Name:    filepath.Base(remotePath),
		Size:    fi.Size(),
		IsDir:   fi.IsDir(),
		ModTime: clock.Millis(fi.ModTime()),
	}
	// The permission facts ride on the platform's own struct. pkg/sftp's server
	// reports them, and the internal VFS may not: absent is "unknown", rendered as
	// an omitted field rather than a made-up 0 — a 0000 would read as "no access at
	// all", which is a different claim from "this server does not say".
	if st, ok := fi.Sys().(*sftp.FileStat); ok && st != nil {
		result.Mode = fmt.Sprintf("%04o", st.Mode&0o7777)
		result.Uid = strconv.FormatUint(uint64(st.UID), 10)
		result.Gid = strconv.FormatUint(uint64(st.GID), 10)
	}

	if fi.IsDir() {
		entries, err := s.client.ReadDir(remotePath)
		if err != nil {
			return result, nil // partial result
		}
		for _, e := range entries {
			child := FileStatResult{
				Name:    e.Name(),
				Size:    e.Size(),
				IsDir:   e.IsDir(),
				ModTime: clock.Millis(e.ModTime()),
			}
			if st, ok := e.Sys().(*sftp.FileStat); ok && st != nil {
				child.Mode = fmt.Sprintf("%04o", st.Mode&0o7777)
				child.Uid = strconv.FormatUint(uint64(st.UID), 10)
				child.Gid = strconv.FormatUint(uint64(st.GID), 10)
			}
			result.Children = append(result.Children, child)
		}
	}

	return result, nil
}

// ChmodFile changes the permissions of a remote file.
func (s *Client) ChmodFile(remotePath string, mode os.FileMode) error {
	return s.client.Chmod(remotePath, mode)
}

// ChownFile changes the owner and group of a remote file.
func (s *Client) ChownFile(remotePath string, uid, gid int) error {
	return s.client.Chown(remotePath, uid, gid)
}

// ChtimesFile changes the access and modification times of a remote file.
func (s *Client) ChtimesFile(remotePath string, atime, mtime time.Time) error {
	return s.client.Chtimes(remotePath, atime, mtime)
}

// ReadLink returns the target of a symbolic link.
func (s *Client) ReadLink(path string) (string, error) {
	return s.client.ReadLink(path)
}

// SymlinkFile creates a symbolic link on the remote.
// target is the existing path, linkPath is the new symlink to create.
func (s *Client) SymlinkFile(target, linkPath string) error {
	return s.client.Symlink(target, linkPath)
}

// LinkFile creates a hard link on the remote.
func (s *Client) LinkFile(existing, newPath string) error {
	return s.client.Link(existing, newPath)
}

// TruncateFile truncates a remote file to the given size.
func (s *Client) TruncateFile(path string, size int64) error {
	return s.client.Truncate(path, size)
}

// RealPath returns the canonical absolute path of a remote file or directory.
func (s *Client) RealPath(path string) (string, error) {
	return s.client.RealPath(path)
}

// FsStatResult holds the result of a file_statvfs operation.
type FsStatResult struct {
	TotalSpace  uint64 `json:"total_space"`
	FreeSpace   uint64 `json:"free_space"`
	AvailSpace  uint64 `json:"avail_space"`
	TotalINodes uint64 `json:"total_inodes"`
	FreeINodes  uint64 `json:"free_inodes"`
	AvailINodes uint64 `json:"avail_inodes"`
	BlockSize   uint64 `json:"block_size"`
}

// fsStatFromVFS converts a pkg/sftp StatVFS to FsStatResult.
func fsStatFromVFS(v *sftp.StatVFS) *FsStatResult {
	return &FsStatResult{
		TotalSpace:  v.Frsize * v.Blocks,
		FreeSpace:   v.Frsize * v.Bfree,
		AvailSpace:  v.Frsize * v.Bavail,
		TotalINodes: v.Files,
		FreeINodes:  v.Ffree,
		AvailINodes: v.Favail,
		BlockSize:   v.Frsize,
	}
}

// StatVFS returns filesystem statistics for the given remote path.
func (s *Client) StatVFS(path string) (*FsStatResult, error) {
	v, err := s.client.StatVFS(path)
	if err != nil {
		return nil, fmt.Errorf("statvfs: %w", err)
	}
	return fsStatFromVFS(v), nil
}

// Getwd returns the remote working directory.
func (s *Client) Getwd() (string, error) {
	return s.client.Getwd()
}

// OpenSFTPOverSSH opens an SFTP connection over an existing SSH client (for regular SSH).
func NewClient(sshClient *ssh.Client) (*Client, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("ssh client is nil")
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return &Client{client: sftpCli}, nil
}
