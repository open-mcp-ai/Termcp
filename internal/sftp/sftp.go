package sftp

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// FileResult holds the result of a file_read operation.
type FileResult struct {
	Data      string `json:"data,omitempty"`
	Mode      string `json:"mode"` // "text" | "hex" | "file"
	Encoding  string `json:"encoding,omitempty"`
	TotalSize int64  `json:"total_size"`
	HasMore   bool   `json:"has_more,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	Length    int64  `json:"length,omitempty"`
	BytesRead int64  `json:"bytes_read,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
}

// FileStatResult holds the result of a file_stat operation.
type FileStatResult struct {
	Name     string           `json:"name"`
	Size     int64            `json:"size"`
	IsDir    bool             `json:"is_dir"`
	ModTime  int64            `json:"mod_time,omitempty"` // Unix ms; 0 = unknown
	Children []FileStatResult `json:"children,omitempty"`
}

// sftpClient abstracts SFTP operations.
type Client struct {
	client *sftp.Client
}

// Close closes the SFTP client.
func (s *Client) Close() error {
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

	if fi.IsDir() {
		entries, err := s.client.ReadDir(remotePath)
		if err != nil {
			return result, nil // partial result
		}
		for _, e := range entries {
			result.Children = append(result.Children, FileStatResult{
				Name:    e.Name(),
				Size:    e.Size(),
				IsDir:   e.IsDir(),
				ModTime: clock.Millis(e.ModTime()),
			})
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
