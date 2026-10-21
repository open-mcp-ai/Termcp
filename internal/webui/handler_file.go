package webui

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// resolveFileSession resolves the session a file route names and applies the
// session's capability whitelist before the handler touches the filesystem.
//
// The tool name is passed in rather than derived from the method and path: the
// whitelist's vocabulary is the MCP tool surface's (file_read, file_write, …),
// which is what an extension template's author writes, and deriving it here
// would invent a second vocabulary the template does not speak.
func (h *Handler) resolveFileSession(sessionID, tool string, paths [][2]string, w http.ResponseWriter) (*session.Session, *ssh.Client, bool) {
	sess := h.Sessions.Get(sessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return nil, nil, false
	}
	// The whitelist is checked before the liveness check: a refused call should
	// read as refused, not as a transient state of the session.
	if restRefuseTool(w, sess, tool) {
		return nil, nil, false
	}
	if restRefusePaths(w, sess, paths...) {
		return nil, nil, false
	}
	// File operations need a live connection; a closed (DEAD) session keeps its
	// output readable via output-range but has no transport left to SFTP over.
	if sess.Info().Status != api.SessionRunning {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "session is closed; file operations need a live connection (read output via /output-range)"})
		return nil, nil, false
	}
	sshClient := sess.SSHClient()
	if sess.SSHEndpoint != "internal" && sshClient == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session has no active SSH connection"})
		return nil, nil, false
	}
	return sess, sshClient, true
}

func (h *Handler) handleListFiles(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	slog.Debug("handleListFiles", "session", sid, "path", path)
	_, sshClient, ok := h.resolveFileSession(sid, capability.FileStat, [][2]string{{"path", path}}, w)
	if !ok {
		return
	}
	if sshClient == nil {
		// Internal: use local filesystem
		fi, err := os.Stat(path)
		if err != nil {
			slog.Error("file stat", "path", path, "err", err)
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		result := map[string]any{"name": filepath.Base(path), "size": fi.Size(), "is_dir": fi.IsDir(), "mod_time": clock.Millis(fi.ModTime())}
		if fi.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				slog.Error("file readdir", "path", path, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			var children []map[string]any
			for _, e := range entries {
				info, _ := e.Info()
				ch := map[string]any{"name": e.Name(), "is_dir": e.IsDir()}
				if info != nil {
					ch["size"] = info.Size()
					ch["mod_time"] = clock.Millis(info.ModTime())
				}
				children = append(children, ch)
			}
			result["children"] = children
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	// Remote: use SFTP
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		slog.Error("sftp client", "path", path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	result, err := sftpCli.StatFile(path)
	if err != nil {
		slog.Error("sftp stat", "path", path, "err", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}

	offset, length, isRange := downloadRange(r)

	hdr, err := requestResponseHeaders(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	_, sshClient, ok := h.resolveFileSession(sid, capability.FileRead, [][2]string{{"path", path}}, w)
	if !ok {
		return
	}
	if sshClient == nil {
		serveLocalFile(w, r, path, offset, length, isRange, hdr)
		return
	}
	serveRemoteFile(w, r, path, sshClient, offset, length, isRange, hdr)
}

// requestResponseHeaders decodes the ?h.<Name>=<value> parameters a download URL
// may carry (see api.AppendResponseHeaders) into headers ready to be written.
func requestResponseHeaders(q url.Values) (http.Header, error) {
	byName, err := api.ResponseHeadersFromQuery(q)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	for name, values := range byName {
		// Canonical form, so the framing-header check cannot be walked around by
		// spelling "content-length" in lowercase.
		hdr[http.CanonicalHeaderKey(name)] = values
	}
	return hdr, nil
}

// serverOwnedHeaders are the headers that frame the body: where it starts, where
// it ends, and how it is encoded on the wire. They belong to the stream being
// sent, not to the caller — a caller-set Content-Length describes a body of a
// different length than the one arriving, which truncates the download or hangs
// the client waiting for bytes that never come. A requested one is dropped.
var serverOwnedHeaders = map[string]bool{
	"Content-Length":    true,
	"Content-Range":     true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Trailer":           true,
}

// applyResponseHeaders writes the caller's requested headers onto the response.
// It runs immediately before the body starts, so a requested Content-Disposition
// (the usual reason to ask for one) overrides the one the streamer would have
// sent, while the framing headers above stay the server's.
func applyResponseHeaders(w http.ResponseWriter, hdr http.Header) {
	for name, values := range hdr {
		if serverOwnedHeaders[name] {
			continue
		}
		w.Header()[name] = values
	}
}

// downloadRange reads the byte window a download asks for. The Range header wins
// when present: it is what a browser and curl send, and it carries its own syntax
// that only parseRange can decode, so nothing is parsed here beyond noticing it.
// Otherwise the explicit ?offset=&length= query parameters are read, and a missing
// or unparsable one stays zero - meaning "from the start" and "to the end".
func downloadRange(r *http.Request) (offset, length int64, isRange bool) {
	// Range header takes priority over ?offset=&length= query params.
	if rh := r.Header.Get("Range"); rh != "" {
		isRange = true
	} else {
		offset, _ = strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		length, _ = strconv.ParseInt(r.URL.Query().Get("length"), 10, 64)
	}
	return offset, length, isRange
}

// serveLocalFile streams a file that lives on the termcp host itself. There is no
// SFTP involved, so http.ServeFile is used whenever possible: it implements Range
// and If-Modified-Since natively. The manual path exists only for an explicit
// ?offset=&length= window, which ServeFile has no way to express.
func serveLocalFile(w http.ResponseWriter, r *http.Request, path string, offset, length int64, isRange bool, hdr http.Header) {
	// Internal / local: http.ServeFile handles Range + If-Modified-Since natively.
	// Only use manual path when explicit ?offset=&length= query params are set.
	// ServeContent sets the headers it owns (framing, and Content-Type only when
	// unset), so the caller's go on first and survive where they may.
	if isRange || r.URL.Query().Get("offset") == "" {
		applyResponseHeaders(w, hdr)
		http.ServeFile(w, r, path)
		return
	}
	// Explicit ?offset=&length= for local files — manual partial stream.
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if length <= 0 || offset+length > fi.Size() {
		length = fi.Size() - offset
	}
	if offset > 0 {
		f.Seek(offset, io.SeekStart)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(path)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
	w.Header().Set("Accept-Ranges", "bytes")
	applyResponseHeaders(w, hdr)
	w.WriteHeader(http.StatusOK)
	io.CopyN(w, f, length)
}

// serveRemoteFile streams a file over SFTP from the session's SSH connection. It
// resolves the Range header against the remote size, then hands the window to
// StreamReadTo, which reads exactly the requested bytes instead of pulling the
// whole file to the termcp host first.
func serveRemoteFile(w http.ResponseWriter, r *http.Request, path string, sshClient *ssh.Client, offset, length int64, isRange bool, hdr http.Header) {
	// Remote — SFTP streaming.
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()

	stat, err := sftpCli.StatFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if stat.IsDir {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path is a directory"})
		return
	}

	// Resolve Range header for SFTP.
	if isRange {
		var ok bool
		offset, length, ok = parseRange(r.Header.Get("Range"), stat.Size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", stat.Size))
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	if length <= 0 || offset+length > stat.Size {
		length = stat.Size - offset
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(path)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
	w.Header().Set("Accept-Ranges", "bytes")
	status := http.StatusOK
	if isRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, stat.Size))
		status = http.StatusPartialContent
	}
	applyResponseHeaders(w, hdr)
	w.WriteHeader(status)

	if _, err := sftpCli.StreamReadTo(w, path, offset, length); err != nil {
		slog.Error("download stream failed", "path", path, "err", err)
	}
}

func (h *Handler) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}

	// Resolve offset: Content-Range header takes priority, then ?offset= query param.
	//
	// A POINTER, because the header and the query param are both optional and 0 is a
	// real position. With an int the two cases collapsed: `offset 0` truncated the
	// file exactly as "no offset given" did, so a client resuming an upload from the
	// first byte replaced the whole file instead of patching it.
	var offset *int64
	if cr := r.Header.Get("Content-Range"); cr != "" {
		var start, end, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err == nil {
			offset = &start
		}
	} else if q := r.URL.Query().Get("offset"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			offset = &v
		}
	}

	// Extract file content: parse multipart form if present, otherwise use raw body.
	var reader io.Reader = r.Body
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			if f, fh, err := r.FormFile("file"); err == nil {
				reader = f
				defer f.Close()
				_ = fh // suppress unused warning
			}
		}
	}

	_, sshClient, ok := h.resolveFileSession(sid, capability.FileWrite, [][2]string{{"path", path}}, w)
	if !ok {
		return
	}
	if sshClient == nil {
		// Internal / local filesystem.
		flag := os.O_RDWR | os.O_CREATE
		if offset == nil {
			flag |= os.O_TRUNC
		}
		f, err := os.OpenFile(path, flag, 0644)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer f.Close()
		if offset != nil && *offset > 0 {
			f.Seek(*offset, io.SeekStart)
		}
		n, err := io.Copy(f, reader)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"bytes_written": n})
		return
	}

	// Remote — SFTP streaming.
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()

	n, err := sftpCli.StreamWriteFrom(reader, path, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes_written": n})
}

func (h *Handler) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	_, sshClient, ok := h.resolveFileSession(sid, capability.FileDelete, [][2]string{{"path", path}}, w)
	if !ok {
		return
	}
	if sshClient == nil {
		if err := os.Remove(path); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	if err := sftpCli.RemoveFile(path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRenameFile renames/moves a file or directory on the same filesystem.
// PUT /api/sessions/{id}/files (canonical); POST /api/sessions/{id}/files/rename (compat).
func (h *Handler) handleRenameFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "from and to query params required"})
		return
	}
	if from == to {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and destination are the same"})
		return
	}

	_, sshClient, ok := h.resolveFileSession(sid, capability.FileRename, [][2]string{{"from", from}, {"to", to}}, w)
	if !ok {
		return
	}
	if sshClient == nil {
		if err := os.Rename(from, to); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	if err := sftpCli.RenameFile(from, to); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleMakeDir(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path query param required"})
		return
	}

	_, sshClient, ok := h.resolveFileSession(sid, capability.FileMkdir, [][2]string{{"path", path}}, w)
	if !ok {
		return
	}
	if sshClient == nil {
		if err := os.MkdirAll(path, 0755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	if err := sftpCli.MakeDir(path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
