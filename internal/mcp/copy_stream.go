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
	"io"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
	"github.com/open-mcp-ai/termcp/internal/task"
	"github.com/open-mcp-ai/termcp/internal/transcript"
)

// pipeCopy runs `read … | write …` as one background task.
//
// IT DOES NOT WAIT, which is the rule the transfer paths already follow (#158): the
// caller is usually an agent behind a tool-call watchdog, and a copy larger than that
// watchdog's patience would be killed partway, leaving a truncated file on the far
// side with no error anywhere. A receipt costs milliseconds and the bytes keep moving.
func (s *Server) pipeCopy(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
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

	src, bad := s.copySource(stmt, win)
	if bad != nil {
		return []envelope.Slot{*bad}
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
			"\n`get task " + t.ID() + "` waits for it (the wait is the call's `timeout`).",
	}}
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

func (c copySource) describe() string {
	if c.channel != "" {
		return c.channel
	}
	return c.session + " file " + c.path
}

// copySource resolves the left side, refusing a value it cannot honour rather than
// ignoring it.
func (s *Server) copySource(stmt directive.Statement, win termArgs) (copySource, *envelope.Slot) {
	// `bytes_limit`, WHEN THE CALLER NAMED ONE, caps the copy. It has a default, so the
	// number alone cannot say whether anyone asked — reading the default as a request
	// would cut every copy to 200 bytes and report success.
	cap := 0
	if win.BytesLimitSet {
		cap = win.BytesLimit
	}

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
		if cap > 0 && (src.length == 0 || int64(cap) < src.length) {
			src.length = int64(cap)
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
	if cap > 0 {
		src.params.MaxBytes = cap
	}
	return src, nil
}

// runCopy wires the two ends together and moves the bytes.
func (s *Server) runCopy(ctx context.Context, src copySource, dst *directive.Statement, progress func(int64)) (int64, error) {
	dstCli, bad := s.sftpClient(dst.Target)
	if bad != nil {
		return 0, fmt.Errorf("%s", errorReplyText(bad))
	}
	defer dstCli.Close()

	if src.channel == "" {
		return s.copyRemoteFile(ctx, src, dst, dstCli, progress)
	}
	return s.copyChannel(ctx, src, dst, dstCli, progress)
}

// copyRemoteFile is the file-to-file case: two SFTP ends and an io.Pipe between them,
// which is an io.Copy that never holds more than one chunk.
func (s *Server) copyRemoteFile(ctx context.Context, src copySource, dst *directive.Statement, dstCli sftpWriter, progress func(int64)) (int64, error) {
	srcCli, bad := s.sftpClient(src.session)
	if bad != nil {
		return 0, fmt.Errorf("%s", errorReplyText(bad))
	}
	defer srcCli.Close()

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
