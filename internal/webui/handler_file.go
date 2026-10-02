package webui

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func (h *Handler) resolveFileSession(sessionID string, w http.ResponseWriter) (*session.Session, *ssh.Client, bool) {
	sess := h.Sessions.Get(sessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
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
	_, sshClient, ok := h.resolveFileSession(sid, w)
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

	// Resolve offset/length: Range header takes priority, then ?offset=&length= query params.
	var offset, length int64
	var isRange bool
	if rh := r.Header.Get("Range"); rh != "" {
		isRange = true
	} else {
		offset, _ = strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		length, _ = strconv.ParseInt(r.URL.Query().Get("length"), 10, 64)
	}

	_, sshClient, ok := h.resolveFileSession(sid, w)
	if !ok {
		return
	}
	if sshClient == nil {
		// Internal / local: http.ServeFile handles Range + If-Modified-Since natively.
		// Only use manual path when explicit ?offset=&length= query params are set.
		if !isRange && r.URL.Query().Get("offset") == "" {
			http.ServeFile(w, r, path)
			return
		}
		if isRange {
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
		w.WriteHeader(http.StatusOK)
		io.CopyN(w, f, length)
		return
	}

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
	if isRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, stat.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

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
	var offset int64
	if cr := r.Header.Get("Content-Range"); cr != "" {
		var start, end, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err == nil {
			offset = start
		}
	} else {
		offset, _ = strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
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

	_, sshClient, ok := h.resolveFileSession(sid, w)
	if !ok {
		return
	}
	if sshClient == nil {
		// Internal / local filesystem.
		flag := os.O_RDWR | os.O_CREATE
		if offset <= 0 {
			flag |= os.O_TRUNC
		}
		f, err := os.OpenFile(path, flag, 0644)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer f.Close()
		if offset > 0 {
			f.Seek(offset, io.SeekStart)
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
	_, sshClient, ok := h.resolveFileSession(sid, w)
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

	_, sshClient, ok := h.resolveFileSession(sid, w)
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

	_, sshClient, ok := h.resolveFileSession(sid, w)
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
