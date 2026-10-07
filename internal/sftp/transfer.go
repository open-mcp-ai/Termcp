package sftp

import (
	"fmt"
	"io"
	"os"

	"github.com/open-mcp-ai/termcp/internal/encoding"
)

// StreamReadTo reads from remotePath (optionally at offset for length bytes) and streams
// raw bytes into w. Uses io.CopyN for zero-buffer streaming — never loads the whole file
// into memory. For large files use offset=0, length<=0 to stream the entire file.
func (s *Client) StreamReadTo(w io.Writer, remotePath string, offset, length int64) (int64, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat remote file: %w", err)
	}

	if offset < 0 {
		offset = 0
	}
	if length <= 0 || offset+length > fi.Size() {
		length = fi.Size() - offset
	}
	if length < 0 {
		length = 0
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}

	return io.CopyN(w, f, length)
}

// StreamWriteFrom reads raw bytes from r and writes them to remotePath at the given
// offset. offset=0 truncates the file; offset>0 writes at that position without
// truncation. Uses io.Copy to stream without buffering in memory.
func (s *Client) StreamWriteFrom(r io.Reader, remotePath string, offset int64) (int64, error) {
	flag := os.O_RDWR | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	f, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}

	return io.Copy(f, r)
}

// maxInlineRead caps how many bytes a single text/hex read pulls into memory.
// The file is still fully reachable: callers page with offset + has_more.
// mode=file streams to a local file and is intentionally uncapped.
const maxInlineRead = 8 << 20 // 8 MiB

// normalizeReadRange clamps a requested byte window to [0, totalSize] and to an
// optional in-memory limit. It is overflow-safe by construction: it never
// computes offset+length, so an enormous length (or a huge/negative offset)
// can never yield a negative or out-of-range allocation in make([]byte, length).
// length <= 0 means "the rest of the file".
func normalizeReadRange(totalSize, offset, length, limit int64) (int64, int64) {
	if totalSize < 0 { // some SFTP servers report -1 for special files
		totalSize = 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > totalSize {
		offset = totalSize
	}
	remaining := totalSize - offset // >= 0
	if length <= 0 || length > remaining {
		length = remaining
	}
	if limit > 0 && length > limit {
		length = limit
	}
	if length < 0 {
		length = 0
	}
	return offset, length
}

// ReadFile reads a file (or segment) from the remote.
func (s *Client) ReadFile(remotePath string, offset, length int64, mode string, localPath string) (*FileResult, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat remote file: %w", err)
	}
	totalSize := fi.Size()

	inlineCap := int64(maxInlineRead)
	if mode == "file" {
		inlineCap = 0 // io.CopyN bounds itself to the file; no memory blow-up
	}
	offset, length = normalizeReadRange(totalSize, offset, length, inlineCap)

	result := &FileResult{
		Mode:      mode,
		TotalSize: totalSize,
		Offset:    offset,
		Length:    length,
		HasMore:   offset+length < totalSize,
	}

	// File mode: write to local file.
	if mode == "file" {
		if localPath == "" {
			return nil, fmt.Errorf("local_path required for file mode")
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek: %w", err)
		}
		localFile, err := os.Create(localPath)
		if err != nil {
			return nil, fmt.Errorf("create local file: %w", err)
		}
		defer localFile.Close()
		n, err := io.CopyN(localFile, f, length)
		if err != nil {
			return nil, fmt.Errorf("copy to local: %w", err)
		}
		result.BytesRead = n
		result.LocalPath = localPath
		return result, nil
	}

	// Read the segment.
	buf := make([]byte, length)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, fmt.Errorf("read: %w", err)
	}
	segment := buf[:n]
	result.BytesRead = int64(n)
	result.Length = int64(n)

	// Encode based on mode.
	switch mode {
	case "hex":
		result.Data = fmt.Sprintf("%x", segment)
	case "text", "":
		result.Data = encoding.EncodeText(segment)
	default:
		return nil, fmt.Errorf("unknown mode %q (use text, hex, or file)", mode)
	}

	return result, nil
}

// WriteFile writes data to a remote file, either inline or from a local file.
func (s *Client) WriteFile(remotePath string, offset int64, data string, mode string, localPath string, localOffset, length int64) (int64, error) {
	if localPath != "" {
		return s.writeFromLocal(remotePath, offset, localPath, localOffset, length)
	}
	return s.writeInline(remotePath, offset, data, mode)
}

func (s *Client) writeInline(remotePath string, offset int64, data string, mode string) (int64, error) {
	var raw []byte
	switch mode {
	case "hex":
		decoded, err := encoding.HexDecode(data)
		if err != nil {
			return 0, fmt.Errorf("hex decode: %w", err)
		}
		raw = decoded
	case "text", "":
		raw = encoding.DecodeText(data)
	default:
		return 0, fmt.Errorf("unknown mode %q", mode)
	}

	flag := os.O_RDWR | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	f, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}
	n, err := f.Write(raw)
	if err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}
	return int64(n), nil
}

func (s *Client) writeFromLocal(remotePath string, offset int64, localPath string, localOffset, length int64) (int64, error) {
	localFile, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("open local file: %w", err)
	}
	defer localFile.Close()

	fi, err := localFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat local file: %w", err)
	}
	if length <= 0 || localOffset+length > fi.Size() {
		length = fi.Size() - localOffset
	}
	if _, err := localFile.Seek(localOffset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek local: %w", err)
	}

	flag := os.O_RDWR | os.O_CREATE
	if offset <= 0 {
		flag |= os.O_TRUNC
	}
	remoteFile, err := s.client.OpenFile(remotePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer remoteFile.Close()

	if offset > 0 {
		if _, err := remoteFile.Seek(offset, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek remote: %w", err)
		}
	}

	n, err := io.CopyN(remoteFile, localFile, length)
	if err != nil {
		return 0, fmt.Errorf("copy: %w", err)
	}
	return n, nil
}
