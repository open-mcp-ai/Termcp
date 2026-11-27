package mcp

// A PIPELINE IS A COPY, and this file is the copy.
//
// `read … | write …` exists for one job: moving bytes from one node to another
// WITHOUT them passing through the caller's context. Doing it today takes two calls
// and a local intermediate file — download, then upload — and the caller pays for the
// round trip even though it never looks at the bytes.
//
// NOTHING IS STORED IN BETWEEN. The two ends are wired straight to each other:
// `StreamReadTo` writes into an `io.Pipe` that `StreamWriteFrom` reads, and both are
// `io.Copy` underneath, so a gigabyte crosses this process in 32 KiB pieces and never
// lands in memory or on disk. The one asymmetry is a CHANNEL source: a transcript has
// no incremental reader, so its span arrives as one `[]byte` from the same read path
// `read` uses, and is streamed out from there. That is the read the caller would have
// made anyway — it is not a copy of the file held anywhere.
//
// ponytail: a channel source holds one span in memory, bounded by the transcript's own
// retention. If that ever needs to be unbounded, the missing piece is a chunked
// transcript reader and only the source half of runCopy changes.

import (
	"bytes"
	"context"
	"fmt"
	"github.com/open-mcp-ai/termcp/internal/sftp"
	"io"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/task"
	"github.com/open-mcp-ai/termcp/internal/transcript"
	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// pipeCopy runs `read … | write …` as one background task.
//
// IT DOES NOT WAIT, which is the rule the transfer paths already follow (#158): the
// caller is usually an agent behind a tool-call watchdog, and a copy larger than that
// watchdog's patience would be killed partway, leaving a truncated file on the far
// side with no error anywhere. A receipt costs milliseconds and the bytes keep moving.
//
// The reply window is accepted and DELIBERATELY UNUSED: it bounds the receipt, not the
// bytes. The directive layer passes it, so the signature stays.
func (s *Server) pipeCopy(ctx context.Context, stmt directive.Statement, _ termArgs) []envelope.Slot {
	dst := stmt.Pipe
	if dst == nil {
		return []envelope.Slot{badRequest(stmt, "a pipeline needs both ends")}
	}

	// THE DESTINATION IS A FILE, AND ONLY A FILE. A copy into a channel would type the
	// bytes into a terminal, and a terminal is not a file: it echoes, it wraps, it
	// interprets control sequences, and it cannot say "done". Each of those turns a
	// byte-perfect copy into a corrupt one, so the refusal names the reason.
	if dst.Kind != "file" || strings.TrimSpace(dst.Key) == "" {
		return []envelope.Slot{badRequest(stmt,
			"the right side of `|` must be `write <session> file <path>`: a pipeline copies BYTES, and a "+
				"terminal is not a file — it echoes, wraps and interprets what it is given. "+
				"Name the destination file, or write the bytes into the terminal on purpose with `run`.")}
	}
	for _, f := range []string{"op", "old-text", "new-text", "data", "local-path", "mode"} {
		if dst.FlagState(f) != directive.FlagAbsent {
			return []envelope.Slot{badRequest(stmt,
				"`write … --"+f+"` is a different operation: the bytes of a pipeline come from the left "+
					"side, so a second source of them is refused rather than silently ignored. "+
					"Drop --"+f+" from the right side, or write the two statements separately.")}
		}
	}

	// THE SOURCE IS A CHANNEL OR A FILE. A peer read hands back FRAMES — a framing, not
	// a byte stream — so piping one would copy the framing into the file.
	if stmt.Kind != "" && stmt.Kind != "file" {
		return []envelope.Slot{badRequest(stmt,
			"the left side of `|` must be a channel (`read <session> <index>`) or a file "+
				"(`read <session> file <path>`); `"+stmt.Kind+"` is a frame source, and a frame is not a "+
				"byte stream. Use `read "+stmt.Target+" "+stmt.Kind+"` on its own to get frames.")}
	}

	// A PIPELINE DOES NOT GO THROUGH A TOOL CALL, so it would otherwise be the one file
	// door where a Windows spelling is NOT adapted — the same path working as
	// `read … file` and failing as `read … file … | write …`. The rule is the same one
	// (sftp.WireSpelling) and it is applied to both ends here, before either is used.
	var adapted []string
	if spelled, ok := sftp.WireSpelling(stmt.Key); ok {
		adapted = append(adapted, stmt.Key+" -> "+spelled)
		stmt.Key = spelled
	}
	if spelled, ok := sftp.WireSpelling(dst.Key); ok {
		adapted = append(adapted, dst.Key+" -> "+spelled)
		dst.Key = spelled
	}

	src, bad := s.copySource(stmt)
	if bad != nil {
		return []envelope.Slot{*bad}
	}

	// A PIPELINE IS TWO FILE OPERATIONS, AND BOTH ENDS ARE ADMITTED (issue #358).
	//
	// This path used to call neither the guard nor the whitelist: it read `stmt.Key` and
	// wrote `dst.Key` straight through the SFTP client, so an extension declaring
	// `paths = ["/srv/x"]` refused `read <its session> file /etc/shadow` with 403 and
	// then copied the same file with `read … | write …` — the whitelist bypassed by
	// punctuation, and a session without `file.write` writing anyway.
	//
	// EACH END IS JUDGED BY ITS OWN SESSION, which is what makes the two-ended case
	// answerable: the source needs the right to read where it reads, the destination the
	// right to write where it writes. A read-only extension can therefore be a
	// destination and not a source, which is the same asymmetry the single-ended forms
	// already have.
	//
	// THE DESTINATION IS REVIEWED AS ONE ACTION, not as a stream: `file.write` is a
	// reviewed capability, and a copy that landed while a human was still deciding would
	// be the gate bypassed by a 202. The copy starts only once the decision comes back,
	// and the reviewer is told it is a copy — see copy_from in summarizeOperation.
	if slot := s.admitCopyEnd(ctx, stmt, sourceEndFor(src)); slot != nil {
		return []envelope.Slot{*slot}
	}
	destArgs := argsOf(
		"session_id", dst.Target,
		"remote_path", dst.Key,
		"copy_from", src.describe(),
		"copy_src", src.toArgs(),
	)
	if slot := s.admitCopyEnd(ctx, stmt, copyEnd{tool: "file_write", args: destArgs}); slot != nil {
		return []envelope.Slot{*slot}
	}

	// Its own context: the request's is cancelled the moment this handler returns,
	// which is now, before the copy is done.
	taskCtx, cancel := context.WithCancel(context.Background())
	// Path is the destination and localPath is the source, which is the order
	// renderTask prints them in ("source -> destination"). Passing them the other way
	// round made every receipt read backwards.
	t := s.tasks.Start(task.Copy, dst.Target, dst.Key, src.describe(), cancel)
	go func() {
		defer cancel()
		n, err := s.runCopy(taskCtx, src, dst, t.Add)
		t.SetTotal(n)
		s.tasks.Finish(t.ID(), err)
	}()

	// THE SAME RECEIPT SHAPE EVERY OTHER TRANSFER USES (202 + Task + a body that says
	// how to wait), because a caller that has waited for an upload once should not have
	// to learn a second spelling for a copy.
	v, _ := s.tasks.Get(t.ID())
	return []envelope.Slot{{
		Code: 202,
		Headers: map[string]string{
			"Path":   stmt.Path,
			"Task":   t.ID(),
			"Status": "running",
		},
		Body: renderTask(v) + "\n" + src.describe() + " -> " + dst.Target + " file " + dst.Key +
			"\n`get task " + t.ID() + "` waits for it (the wait is the call's `timeout`)." +
			adaptedNote(adapted),
	}}
}

// copyEnd names one half of a pipeline for admission: the capability it needs and the
// arguments that describe it.
type copyEnd struct {
	tool string
	args map[string]any
}

// admitCopyEnd sends one half of a pipeline through the same admission exit every
// handler uses, and returns the slot to answer with when it says no (issue #358).
//
// IT GOES THROUGH checkAdmission RATHER THAN THE HANDLERS, because the handlers would
// do the work: this asks the question and leaves the bytes alone. What comes back is
// one of three things — nil (allowed), a refusal (reachability, capability or path
// whitelist), or a review-pending document that is returned as the reply, so the caller
// learns a human is deciding rather than that the copy failed.
func (s *Server) admitCopyEnd(ctx context.Context, stmt directive.Statement, end copyEnd) *envelope.Slot {
	req := mcp.CallToolRequest{}
	req.Params.Name = end.tool
	req.Params.Arguments = end.args
	bad, held := s.checkAdmission(ctx, end.tool, req)
	if bad == nil && !held {
		return nil
	}
	if view := reviewPendingFrom(bad); view != nil {
		slot := view.slot(stmt.Path)
		return &slot
	}
	slot := s.slotFromFailure(ctx, stmt, failureFromResult(bad))
	return &slot
}

// sourceEndFor describes the left half of a pipeline for admission.
func sourceEndFor(src copySource) copyEnd {
	if src.channel != "" {
		// A CHANNEL SOURCE IS A TRANSCRIPT READ, and its capability is the channel's own:
		// a session whose whitelist omits shell.output must not be able to pipe its
		// transcript into a file either.
		return copyEnd{tool: "shell_output", args: argsOf("shell_id", src.channel)}
	}
	return copyEnd{tool: "file_read", args: argsOf("session_id", src.session, "remote_path", src.path)}
}

// copySource is what the left side of a pipeline contributes: the bytes' address.
type copySource struct {
	// channel is a shell channel id; session+path is a remote file. Exactly one is set.
	channel string
	session string
	path    string
	// offset/length narrow a file; the params narrow a channel. Both are the CALLER's
	// addressing, and neither is a window: a window protects the caller's context, and
	// these bytes never enter it.
	offset int64
	length int64
	params transcript.Params
}

func (c copySource) toArgs() map[string]any {
	m := map[string]any{
		"channel": c.channel,
		"session": c.session,
		"path":    c.path,
		"offset":  float64(c.offset),
		"length":  float64(c.length),
	}
	if c.params.Offset != nil {
		m["p_offset"] = float64(*c.params.Offset)
	}
	if c.params.TailLines != nil {
		m["p_tail"] = float64(*c.params.TailLines)
	}
	if c.params.FromLine > 0 {
		m["p_from_line"] = float64(c.params.FromLine)
	}
	if c.params.Stream != "" {
		m["p_stream"] = c.params.Stream
	}
	return m
}

func copySourceFromArgs(m map[string]any) (copySource, bool) {
	if len(m) == 0 {
		return copySource{}, false
	}
	src := copySource{
		channel: getString(m, "channel", ""),
		session: getString(m, "session", ""),
		path:    getString(m, "path", ""),
		offset:  int64(getFloat64(m, "offset", 0)),
		length:  int64(getFloat64(m, "length", 0)),
	}
	if v, ok := m["p_offset"].(float64); ok {
		n := int64(v)
		src.params.Offset = &n
	}
	if v, ok := m["p_tail"].(float64); ok {
		n := int(v)
		src.params.TailLines = &n
	}
	if v, ok := m["p_from_line"].(float64); ok && v > 0 {
		src.params.FromLine = int64(v)
	}
	src.params.Stream = getString(m, "p_stream", "")
	return src, src.channel != "" || src.path != ""
}

// replayApprovedCopy executes a held pipeline write once a human approves it (issue #358).
//
// IT RUNS SYNCHRONOUSLY inside ExecuteApprovedOperation rather than returning a 202 task,
// because the reviewer who clicked Approve expects the file to be there when the click
// returns — and if the copy fails (disk full, source gone), the Web UI error toast needs
// the error right then instead of silently finishing in a background task nobody is
// polling.
func (s *Server) replayApprovedCopy(ctx context.Context, sessionID, remotePath string, rawSrc map[string]any) (*mcp.CallToolResult, error) {
	src, ok := copySourceFromArgs(rawSrc)
	if !ok {
		return toolError(CodeInvalidArgument, "%s", "approved copy is missing its source"), nil
	}
	dst := &directive.Statement{Target: sessionID, Key: remotePath}
	n, err := s.runCopy(ctx, src, dst, nil)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{"bytes_written": n, "remote_path": remotePath}), nil
}

func (c copySource) describe() string {
	if c.channel != "" {
		return c.channel
	}
	return c.session + " file " + c.path
}

// copySource resolves the left side, refusing a value it cannot honour rather than
// ignoring it.
//
// THE REPLY WINDOW DOES NOT REACH THIS FAR. `bytes_limit` bounds what one reply hands
// the caller; the copy's bytes never enter the caller's context, so applying it here
// made the destination a truncated prefix while the task still reported 100% done —
// a corrupt file that looks finished (#290). How many bytes move is the caller's
// explicit `--bytes` / `--offset`, and nothing else.
func (s *Server) copySource(stmt directive.Statement) (copySource, *envelope.Slot) {
	if stmt.Kind == "file" {
		src := copySource{session: stmt.Target, path: stmt.Key}
		for flag, into := range map[string]*int64{"offset": &src.offset, "bytes": &src.length} {
			if raw, ok := stmt.Flag(flag); ok {
				n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || n < 0 {
					slot := badRequest(stmt, "--"+flag+" takes a byte count, got "+strconv.Quote(raw))
					return copySource{}, &slot
				}
				*into = n
			}
		}
		return src, nil
	}

	src := copySource{channel: stmt.Target}
	args := map[string]any{}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"cursor", "offset"},
		[2]string{"offset", "offset"},
		[2]string{"from-line", "from_line"},
		[2]string{"tail", "tail_lines"},
	); bad {
		return copySource{}, &slot
	}
	if v, ok := args["offset"]; ok {
		n := int64(v.(float64))
		src.params.Offset = &n
	}
	if v, ok := args["tail_lines"]; ok {
		n := int(v.(float64))
		src.params.TailLines = &n
	}
	if v, ok := args["from_line"]; ok {
		src.params.FromLine = int64(v.(float64))
	}
	if v, ok := stmt.Flag("stream"); ok {
		src.params.Stream = v
	}
	// NO WINDOW, AND NO RENDERING: a copy is the bytes. StripAnsi stays off and the raw
	// span is what moves, so what lands in the destination file is what the channel
	// carried rather than what a terminal would have shown.
	src.params.StripAnsi = false
	return src, nil
}

// runCopy wires the two ends together and moves the bytes.
func (s *Server) runCopy(ctx context.Context, src copySource, dst *directive.Statement, progress func(int64)) (int64, error) {
	// A REQUEST-SCOPED LEASE, AND THIS IS THE REQUEST. ctx is the task's own context,
	// created when the copy was accepted and cancelled only when the copy ends, so the
	// lease lives exactly as long as the bytes are moving. `sftpClient` cannot be used
	// here: it refuses every `use` target (`internal`, `use:<host>`) because a
	// `use` transport is a pool lease and that helper has nowhere to hand it back.
	dstCli, releaseDst, bad := s.sftpClientContext(ctx, dst.Target)
	if bad != nil {
		return 0, fmt.Errorf("%s", errorReplyText(bad))
	}
	defer func() {
		dstCli.Close()
		releaseDst()
	}()

	if src.channel == "" {
		return s.copyRemoteFile(ctx, src, dst, dstCli, progress)
	}
	return s.copyChannel(ctx, src, dst, dstCli, progress)
}

// copyRemoteFile is the file-to-file case: two SFTP ends and an io.Pipe between them,
// which is an io.Copy that never holds more than one chunk.
func (s *Server) copyRemoteFile(ctx context.Context, src copySource, dst *directive.Statement, dstCli sftpWriter, progress func(int64)) (int64, error) {
	srcCli, releaseSrc, bad := s.sftpClientContext(ctx, src.session)
	if bad != nil {
		return 0, fmt.Errorf("%s", errorReplyText(bad))
	}
	defer func() {
		srcCli.Close()
		releaseSrc()
	}()

	pr, pw := io.Pipe()
	readErr := make(chan error, 1)
	go func() {
		_, err := srcCli.StreamReadTo(pw, src.path, src.offset, src.length)
		// CloseWithError is what unblocks the writer when the read fails, and what
		// tells it the stream ended cleanly when it does not.
		pw.CloseWithError(err)
		readErr <- err
	}()

	n, err := dstCli.StreamWriteFrom(&copyReader{ctx: ctx, r: pr, add: progress}, dst.Key, nil)
	if err != nil {
		// The reader is parked in a Write nobody will drain; closing the read end
		// releases it before we wait for its error.
		pr.CloseWithError(err)
		<-readErr
		return n, fmt.Errorf("write %s: %w", dst.Key, err)
	}
	if rerr := <-readErr; rerr != nil {
		return n, fmt.Errorf("read %s: %w", src.path, rerr)
	}
	return n, nil
}

// copyChannel is the channel-to-file case. The span comes from the same read path
// `read` uses, so `--cursor`, `--from-line`, `--tail` and `--stream` mean here exactly
// what they mean there; only the window is absent, because the bytes are not entering
// the caller's context.
func (s *Server) copyChannel(ctx context.Context, src copySource, dst *directive.Statement, dstCli sftpWriter, progress func(int64)) (int64, error) {
	srcT, bad := s.resolveOutputSource(src.channel)
	if bad != nil {
		return 0, fmt.Errorf("%s", errorReplyText(bad))
	}
	w, err := transcript.Read(ctx, srcT, src.params)
	if err != nil {
		return 0, err
	}
	n, err := dstCli.StreamWriteFrom(&copyReader{ctx: ctx, r: bytes.NewReader(w.Raw), add: progress}, dst.Key, nil)
	if err != nil {
		return n, fmt.Errorf("write %s: %w", dst.Key, err)
	}
	return n, nil
}

// sftpWriter is the half of *sftp.Client a copy needs, so the two source shapes can
// share the destination code.
type sftpWriter interface {
	StreamWriteFrom(r io.Reader, remotePath string, offset *int64) (int64, error)
	Close() error
}

// copyReader is the reader the destination pulls from. It does two jobs that have to
// happen in this order, on the one path where the bytes are actually moving:
//
//   - IT REPORTS PROGRESS, so a long copy has a live number in `get task` instead of
//     one that jumps from 0 to done.
//   - IT HONOURS CANCELLATION. Neither `StreamReadTo` nor `StreamWriteFrom` takes a
//     context — they are `io.Copy` underneath — so a `close task <id>` would mark the
//     task cancelled and the bytes would keep moving. Checking here aborts the copy at
//     the next chunk, and for the file-to-file case it also unwedges the reader
//     goroutine, because the write end of the pipe is what it is blocked on.
type copyReader struct {
	ctx context.Context
	r   io.Reader
	add func(int64)
}

func (c *copyReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 && c.add != nil {
		c.add(int64(n))
	}
	return n, err
}
