package sftp

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	pkgsftp "github.com/pkg/sftp"

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
// offset. A nil offset truncates the file; a pointer to 0 overwrites from the first
// byte without truncating. Uses io.Copy to stream without buffering in memory.
//
// A WHOLE-FILE WRITE GOES THROUGH A TEMPORARY SIBLING, the same rule
// WriteFromLocalProgress follows and for the same reason (#158): a stream that dies
// partway — the reader errored, the caller cancelled, the link dropped — must not
// leave a truncated file where the real one goes. This path had no such protection,
// so `read <a> file <x> | write <b> file <y>` where x could not be opened left an
// empty <y> behind: a copy that failed, reported failed, and destroyed the
// destination anyway.
//
// An explicit offset still patches in place: a patch cannot be assembled in a sibling
// without copying the rest of the file first.
func (s *Client) StreamWriteFrom(r io.Reader, remotePath string, offset *int64) (int64, error) {
	writePath := remotePath
	if offset == nil {
		writePath = siblingOf(remotePath, ".termcp-part-"+randomSuffix())
	}

	f, off, err := s.openForWrite(writePath, offset)
	if err != nil {
		return 0, err
	}
	// A failed or cancelled write must not leave the part file behind.
	success := false
	defer func() {
		_ = f.Close()
		if writePath != remotePath && !success {
			_ = s.RemoveFile(writePath)
		}
	}()

	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}

	n, err := io.Copy(f, r)
	if err != nil {
		return n, err
	}
	// The rename is the commit point: until it happens the target still holds the old
	// file, and after it the target holds all of the new one.
	if writePath != remotePath {
		if err := f.Close(); err != nil {
			return n, fmt.Errorf("close %s: %w", writePath, err)
		}
		if err := s.client.Rename(writePath, remotePath); err != nil {
			_ = s.RemoveFile(writePath)
			return n, fmt.Errorf("rename %s -> %s: %w", writePath, remotePath, err)
		}
	}
	success = true
	return n, nil
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

// ReadFile reads a segment of a remote file and renders it for a reply.
//
// charset is the caller's --encoding: the bytes are decoded from it to UTF-8 BEFORE
// the text rendering, because rendering first is what turned a PowerShell log into
// `\xff\xfeP\x00S\x00...` — true, and unreadable (issue #177). Empty means UTF-8,
// which is what this file read assumed everywhere until the flag existed.
func (s *Client) ReadFile(remotePath string, offset, length int64, mode string, localPath, charset string) (*FileResult, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat remote file: %w", err)
	}
	// A DIRECTORY IS A CALLER MISTAKE, NOT A TRANSPORT FAULT, and it has to be
	// caught HERE. Reading one through SFTP fails deep inside the reader and comes
	// back as `sftp: "read /tmp/x: is a directory" (SSH_FX_FAILURE)` — an
	// ErrSSHFxFailure indistinguishable from a genuine fault, complete with the
	// protocol's own wording. Stat already knows the answer, so say it in terms the
	// caller can act on.
	if fi.IsDir() {
		return nil, fmt.Errorf("%s: %w; `get <session> file <path> --op stat` lists it, and the shell can list it too", remotePath, ErrIsDirectory)
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
		localFile, err := os.Create(NativeLocalPath(localPath))
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
		body := fmt.Sprintf("%x", segment)
		result.Data = &body
	case "text", "":
		body := encoding.EncodeText(segment)
		result.Data = &body
	default:
		return nil, fmt.Errorf("unknown mode %q (use text, hex, or file)", mode)
	}

	// THE CHARSET DECODES; IT DOES NOT FILTER. `hex` is the byte-exact door and stays
	// untouched by --encoding, because a caller asking for hex asked for the bytes.
	if charset != "" && mode != "hex" {
		decoded, err := encoding.Decode(charset, segment)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", remotePath, err)
		}
		body := encoding.EncodeText(decoded)
		result.Data = &body
	}

	return result, nil
}

// WriteFile writes data to a remote file, either inline or from a local file.
func (s *Client) WriteFile(remotePath string, offset *int64, data string, mode string, localPath string, localOffset, length int64) (int64, error) {
	if localPath != "" {
		return s.writeFromLocal(remotePath, offset, localPath, localOffset, length)
	}
	return s.writeInline(remotePath, offset, data, mode)
}

func (s *Client) writeInline(remotePath string, offset *int64, data string, mode string) (int64, error) {
	var raw []byte
	switch mode {
	case "hex":
		decoded, err := encoding.HexDecode(data)
		if err != nil {
			return 0, fmt.Errorf("hex decode: %w", err)
		}
		raw = decoded
	case "text", "":
		// `hex:` IS A PREFIX, AND IT IS READ HERE BECAUSE THE DOCUMENTATION
		// PROMISES IT. `help write` says "--data hex:00ff" writes two bytes, but
		// nothing decoded the prefix: the seven characters `hex:00ff` went into the
		// file as text and the reply said 200 with an honest-looking byte count.
		//
		// That is the one failure mode worth spending code on, because it is
		// SILENT: every other bad argument in this surface is refused by name, while
		// this one produces a file that parses as neither hex nor the intended
		// bytes, and the caller has no reason to look.
		//
		// Only the text mode interprets the prefix: `--mode hex --data hex:00ff` is a
		// caller writing those characters deliberately, and a mode that already says
		// "hex" must not then read a prefix off the front of its own payload.
		if hex, ok := strings.CutPrefix(data, "hex:"); ok {
			decoded, err := encoding.HexDecode(hex)
			if err != nil {
				return 0, fmt.Errorf("hex decode after the `hex:` prefix: %w", err)
			}
			raw = decoded
			break
		}
		raw = encoding.DecodeText(data)
	default:
		return 0, fmt.Errorf("unknown mode %q", mode)
	}

	f, off, err := s.openForWrite(remotePath, offset)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// SEEK ONLY WHEN NEEDED, BUT ALWAYS KNOW WHERE WE ARE. The file's own offset
	// starts at 0, so writing at 0 needs no seek; writing anywhere else does. This
	// is the half of the offset-0 bug that made it invisible: the flag was right
	// and the position was right, so only the truncate decision was wrong.
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek: %w", err)
		}
	}
	n, err := f.Write(raw)
	if err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}
	return int64(n), nil
}

// NativeLocalPath rewrites a POSIX-shell path into the spelling this host's file
// system API understands.
//
// The daemon is usually driven from a shell, and a shell reports paths in its own
// dialect: Git Bash hands out `/d/home/x` for `D:\home\x`, and os.Open on Windows
// has no idea what that is — it answers "The system cannot find the path
// specified", which names neither the path nor the problem (found by driving a
// real Windows daemon from Git Bash). Only a leading `/<letter>/` is rewritten:
// on Windows that spelling is never a native path, so nothing that used to work
// changes meaning.
func NativeLocalPath(p string) string {
	if runtime.GOOS != "windows" || len(p) < 3 || p[0] != '/' || p[2] != '/' {
		return p
	}
	c := p[1]
	if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
		return p
	}
	return strings.ToUpper(string(c)) + ":" + filepath.FromSlash(p[2:])
}

// ProgressFunc receives incremental byte counts during a transfer.
type ProgressFunc func(n int64)

// progressReader wraps r and reports bytes read to onProgress.
type progressReader struct {
	r          io.Reader
	onProgress ProgressFunc
}

func (p progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 && p.onProgress != nil {
		p.onProgress(int64(n))
	}
	return n, err
}

func (s *Client) writeFromLocal(remotePath string, offset *int64, localPath string, localOffset, length int64) (int64, error) {
	return s.WriteFromLocalProgress(context.Background(), remotePath, offset, localPath, localOffset, length, nil)
}

// WriteFromLocalProgress streams a local file to remotePath, reporting progress.
//
// ATOMIC ON A FULL REWRITE. When offset == nil the caller is replacing the file,
// and a sync copy that gets interrupted (host watchdog, link reset) leaves a
// half-written file on the far side — which docker or a service then fails to run
// (#158). So a whole-file write streams to a temporary sibling (`.termcp-part-…`)
// and renames it over the target only on a clean exit. An aborted transfer deletes
// the part file: no half-written binaries are ever left behind.
//
// An explicit offset patches the target in place, because a patch cannot be
// assembled in a sibling without copying the rest of the file first.
func (s *Client) WriteFromLocalProgress(ctx context.Context, remotePath string, offset *int64, localPath string, localOffset, length int64, onProgress ProgressFunc) (int64, error) {
	localFile, err := os.Open(NativeLocalPath(localPath))
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

	// Choose the target: a temporary sibling for a full rewrite, the real path for a patch.
	isWholeRewrite := offset == nil
	writePath := remotePath
	if isWholeRewrite {
		writePath = siblingOf(remotePath, ".termcp-part-"+randomSuffix())
	}

	flag, off := writeOpenFlags(offset)
	remoteFile, err := s.client.OpenFile(writePath, flag)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	// A failed or cancelled write must not leave the part file behind.
	success := false
	defer func() {
		_ = remoteFile.Close()
		if isWholeRewrite && !success {
			_ = s.RemoveFile(writePath)
		}
	}()

	if off > 0 {
		if _, err := remoteFile.Seek(off, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek remote: %w", err)
		}
	}

	var reader io.Reader = io.LimitReader(localFile, length)
	if onProgress != nil {
		reader = progressReader{r: reader, onProgress: onProgress}
	}

	// Copy in chunks so ctx.Done() is observed during the transfer: io.CopyN
	// would otherwise block until EOF even after context cancellation.
	buf := make([]byte, 32<<10)
	var written int64
	for written < length {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		toRead := int64(len(buf))
		if remain := length - written; remain < toRead {
			toRead = remain
		}
		nr, rerr := reader.Read(buf[:toRead])
		if nr > 0 {
			nw, werr := remoteFile.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if werr != nil {
				return written, fmt.Errorf("write remote: %w", werr)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return written, fmt.Errorf("read local: %w", rerr)
		}
	}

	if isWholeRewrite {
		// Close before renaming: Windows rejects a rename while a handle is open.
		if err := remoteFile.Close(); err != nil {
			return written, fmt.Errorf("close temporary file: %w", err)
		}
		if err := s.RenameFile(writePath, remotePath); err != nil {
			// Cross-platform replace fallback (Windows rejects overwrite).
			_ = s.RemoveFile(remotePath)
			if err := s.RenameFile(writePath, remotePath); err != nil {
				return written, fmt.Errorf("rename temporary file over %s: %w", remotePath, err)
			}
		}
	}

	success = true
	return written, nil
}

// writeOpenFlags decides how a write opens its target.
//
// A POINTER, NOT A SENTINEL: nil means the caller did not specify an offset (the
// file is truncated and rewritten from the start), while a pointer to 0 means
// "overwrite from the first byte, preserving the rest". They used to share the
// value 0 under `offset <= 0`, which made the destructive one the default.
//
// The returned offset is always non-negative: 0 for the truncate case, and
// whatever the caller passed otherwise.
func writeOpenFlags(offset *int64) (int, int64) {
	flag := os.O_RDWR | os.O_CREATE
	if offset == nil {
		return flag | os.O_TRUNC, 0
	}
	off := *offset
	if off < 0 {
		off = 0
	}
	return flag, off
}

// maxReplaceSize bounds how large a file ReplaceInFile will load into memory.
// An exact-replace primitive is for editing text, not for rewriting a 4 GB log:
// loading the latter would OOM the daemon rather than edit it. Callers with
// bigger files have file write + offsets.
const maxReplaceSize = 16 << 20 // 16 MiB

// ReplaceInFile replaces exactly one occurrence of oldText with newText.
//
// THE UNIQUENESS CONSTRAINT IS THE FEATURE. A replace that "just replaces" is how
// an edit lands in the wrong place: an agent asked to change `timeout = 5` in a
// config with three such lines would silently pick the first, and the mistake is
// invisible until the wrong service misbehaves. Matching zero or several times is
// an error that names the count, so the caller narrows the pattern instead of
// guessing which one it got.
//
// ATOMIC ON POSIX: the result is written to a temporary file in the SAME directory
// and renamed over the original. rename(2) within a filesystem is atomic, so a
// reader either sees the old file or the new one — never a half-written one, which
// is what an in-place write produces if the process dies mid-way.
//
// Returns the number of bytes written (the new file's size).
func (s *Client) ReplaceInFile(remotePath, oldText, newText string) (int64, error) {
	if oldText == "" {
		return 0, fmt.Errorf("old_text must not be empty")
	}
	f, err := s.client.Open(remotePath)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat remote file: %w", err)
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("%s: %w; `get <session> file <path> --op stat` lists it, and the shell can list it too", remotePath, ErrIsDirectory)
	}
	if fi.Size() > maxReplaceSize {
		return 0, fmt.Errorf("%s is %d bytes, over the %d-byte limit for exact replace; use file read + file write with --offset for a file this large",
			remotePath, fi.Size(), maxReplaceSize)
	}

	original, err := io.ReadAll(io.LimitReader(f, maxReplaceSize+1))
	if err != nil {
		return 0, fmt.Errorf("read remote file: %w", err)
	}
	// CLOSE BEFORE RENAMING OVER IT. The deferred Close above runs when the function
	// returns, which is after the rename — fine on POSIX, where an open handle does not
	// block a rename, and fatal on Windows, where the rename and then the remove both
	// fail with "The process cannot access the file because it is being used by another
	// process". Closing here (and tolerating the second Close from the defer) makes the
	// handle's lifetime match the read that needs it.
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close remote file: %w", err)
	}

	count := strings.Count(string(original), oldText)
	if count == 0 {
		return 0, fmt.Errorf("%w: %q does not appear in %s", ErrPatternNotFound, previewForError(oldText), remotePath)
	}
	if count > 1 {
		return 0, fmt.Errorf("%w: %q appears %d times in %s; include more surrounding context so it matches once",
			ErrPatternAmbiguous, previewForError(oldText), count, remotePath)
	}

	updated := strings.Replace(string(original), oldText, newText, 1)

	// Temporary file in the same directory, so the rename stays within one
	// filesystem and is therefore atomic.
	tmpPath := siblingOf(remotePath, ".termcp-replace-"+randomSuffix())

	if err := s.writeWholeFile(tmpPath, []byte(updated)); err != nil {
		_ = s.RemoveFile(tmpPath)
		return 0, fmt.Errorf("write temporary file: %w", err)
	}
	// Preserve the mode: a replace must not silently change permissions, which is
	// exactly what a freshly created temp file would otherwise do (0644).
	if err := s.client.Chmod(tmpPath, fi.Mode().Perm()); err != nil {
		_ = s.RemoveFile(tmpPath)
		return 0, fmt.Errorf("preserve permissions on temporary file: %w", err)
	}
	// RENAME OVER THE TARGET IS NOT PORTABLE. On POSIX the rename replaces the
	// destination atomically, which is the whole reason for the temp file. Windows
	// answers SSH_FX_FAILURE "Access is denied" when the destination exists, so the
	// atomic swap is unavailable there and the best remaining sequence is remove then
	// rename — a window where the file is briefly absent, chosen deliberately over a
	// replace that simply does not work.
	if err := s.RenameFile(tmpPath, remotePath); err != nil {
		if rmErr := s.RemoveFile(remotePath); rmErr != nil {
			_ = s.RemoveFile(tmpPath)
			return 0, fmt.Errorf("rename temporary file over %s: %w (and removing the target first failed: %w)",
				remotePath, err, rmErr)
		}
		if err := s.RenameFile(tmpPath, remotePath); err != nil {
			_ = s.RemoveFile(tmpPath)
			return 0, fmt.Errorf("rename temporary file over %s after removing it: %w", remotePath, err)
		}
	}
	return int64(len(updated)), nil
}

// writeWholeFile creates (or truncates) remotePath and writes data.
func (s *Client) writeWholeFile(remotePath string, data []byte) error {
	f, err := s.client.Create(remotePath)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// previewForError shortens a pattern for an error message. A replace pattern can be
// a whole paragraph, and pasting it into an error would bury the count and the hint.
func previewForError(s string) string {
	const limit = 60
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// randomSuffix returns a short unique token for a temporary filename.
func randomSuffix() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand failing is not a reason to refuse the edit; the pid is enough
		// to avoid a collision within one process.
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// openForWrite opens remotePath for writing, and says WHICH part is missing when
// the open fails because something does not exist.
//
// "open remote file: file does not exist" is a loop for a WRITE: the file is the
// thing the caller is creating, and creating a new file in an existing directory
// succeeds. The missing piece is the parent directory, and the caller cannot tell
// the two apart from the message — it reads as "the file you asked me to create
// does not exist", which is true of every file that is about to be created.
//
// Stat-ing the parent turns the same failure into the fact: the directory is
// named, and the path that cannot be created in it is named beside it.
//
// The original error is wrapped rather than replaced, so the classification the
// caller's status depends on (SSH_FX_NO_SUCH_FILE → 404) still travels with it.
func (s *Client) openForWrite(remotePath string, offset *int64) (*pkgsftp.File, int64, error) {
	flag, off := writeOpenFlags(offset)
	f, err := s.client.OpenFile(remotePath, flag)
	if err == nil {
		return f, off, nil
	}
	// Only the not-found case is worth a second round trip; a permission refusal or
	// a transport fault is already saying the right thing.
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, pkgsftp.ErrSSHFxNoSuchFile) {
		return nil, 0, fmt.Errorf("open remote file: %w", err)
	}
	if dir, ok := parentOf(remotePath); ok {
		if _, serr := s.client.Stat(dir); serr != nil {
			return nil, 0, fmt.Errorf("the directory %s does not exist, so %s cannot be created in it: %w", dir, remotePath, err)
		}
	}
	return nil, 0, fmt.Errorf("open remote file: %w", err)
}

// parentOf returns the containing directory of a REMOTE path, spelled the way the
// caller spelled it. See siblingOf for why the local path package cannot be used.
//
// ok is false when the path has no separator at all (a bare name, whose directory
// is the server's cwd and not something this can name).
func parentOf(remotePath string) (string, bool) {
	i := strings.LastIndexAny(remotePath, `/\`)
	if i < 0 {
		return "", false
	}
	if i == 0 {
		return remotePath[:1], true
	}
	return remotePath[:i], true
}

// siblingOf builds a hidden name beside a REMOTE path, keeping the path's own
// spelling.
//
// `path.Dir` is the wrong tool: it only knows "/". A Windows target hands over
// `C:\Users\x\agent.conf`, which path.Dir reports as ("." , whole-string) — so the
// temporary file came out as `.C:\Users\x\agent.conf.termcp-replace-…`, a name the
// server rejected. The remote path is not a local path and cannot be parsed with the
// local platform's rules.
//
// Both separators are honoured and the found one is reused, so the name is built the
// way the caller spelled it. Normalising to "/" would work for a POSIX server and
// break a Windows one; guessing from the string alone is the only option, and echoing
// the caller's own spelling back is the least surprising answer.
func siblingOf(remotePath, suffix string) string {
	i := strings.LastIndexAny(remotePath, `/\`)
	if i < 0 {
		return "." + remotePath + suffix
	}
	dir, base, sep := remotePath[:i], remotePath[i+1:], remotePath[i:i+1]
	if dir == "" {
		// "/x" -> the root; keep the separator so the path stays absolute.
		return sep + "." + base + suffix
	}
	return dir + sep + "." + base + suffix
}

// ReadToLocalProgress streams remotePath into a local file, reporting progress.
//
// THE LOCAL SIDE IS THE ONE THAT MUST NOT BE LEFT HALF-WRITTEN, for the same reason
// a remote target must not: whoever consumes the download — a build, a checksum, an
// installer — reads the name it asked for. So the bytes go to `<local>.part` and the
// rename happens on a clean exit; an interrupted download deletes the part file.
func (s *Client) ReadToLocalProgress(ctx context.Context, remotePath, localPath string, offset, length int64, onProgress ProgressFunc) (int64, error) {
	f, err := s.client.Open(remotePath)
	if err != nil {
		return 0, fmt.Errorf("open remote file: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat remote file: %w", err)
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("%s: %w; `get <session> file <path> --op stat` lists it, and the shell can list it too", remotePath, ErrIsDirectory)
	}
	totalSize := fi.Size()
	offset, length = normalizeReadRange(totalSize, offset, length, 0)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek: %w", err)
	}

	partPath := NativeLocalPath(localPath) + ".termcp-part"
	localFile, err := os.Create(partPath)
	if err != nil {
		return 0, fmt.Errorf("create local file: %w", err)
	}
	success := false
	defer func() {
		_ = localFile.Close()
		if !success {
			_ = os.Remove(partPath)
		}
	}()

	var reader io.Reader = io.LimitReader(f, length)
	if onProgress != nil {
		reader = progressReader{r: reader, onProgress: onProgress}
	}

	// Chunked so ctx.Done() is observed: io.CopyN would run to EOF regardless.
	buf := make([]byte, 32<<10)
	var written int64
	for written < length {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		toRead := int64(len(buf))
		if remain := length - written; remain < toRead {
			toRead = remain
		}
		nr, rerr := reader.Read(buf[:toRead])
		if nr > 0 {
			nw, werr := localFile.Write(buf[:nr])
			written += int64(nw)
			if werr != nil {
				return written, fmt.Errorf("write local: %w", werr)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return written, fmt.Errorf("read remote: %w", rerr)
		}
	}
	if err := localFile.Close(); err != nil {
		return written, fmt.Errorf("close local file: %w", err)
	}
	if err := os.Rename(partPath, localPath); err != nil {
		return written, fmt.Errorf("rename into place: %w", err)
	}
	success = true
	return written, nil
}
