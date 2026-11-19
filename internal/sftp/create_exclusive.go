package sftp

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// CreateExclusive never falls back to a normal write or rename. The server's
// SSH_FXF_EXCL check, not an existence probe, decides concurrent creates.
func (s *Client) CreateExclusive(filename, data string) error {
	// Direct children only; reject symlinked parents. As with other SFTP v3
	// operations, ancestors must not be concurrently replaced by an untrusted
	// party: the wire protocol cannot open a file relative to a directory handle.
	for dir := path.Dir(filename); ; dir = path.Dir(dir) {
		check := dir
		volume := strings.TrimPrefix(dir, "/")
		driveRoot := len(volume) == 2 && volume[1] == ':' && ((volume[0] >= 'A' && volume[0] <= 'Z') || (volume[0] >= 'a' && volume[0] <= 'z'))
		if driveRoot {
			check += "/" // Windows drive root
		}
		fi, err := s.client.Lstat(check)
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q is not a plain directory", dir)
		}
		if dir == path.Dir(dir) || driveRoot {
			break
		}
	}
	f, err := s.client.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		// SFTP v3 commonly maps EEXIST to a generic SSH_FX_FAILURE.
		if _, statErr := s.client.Lstat(filename); statErr == nil {
			return fmt.Errorf("%s already exists: %w", filename, os.ErrExist)
		}
		return err
	}
	n, writeErr := io.WriteString(f, data)
	closeErr := f.Close()
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		// Do not unlink by path: it may no longer refer to the file we opened.
		return fmt.Errorf("created %s but it may be incomplete: %w", filename, writeErr)
	}
	return nil
}
