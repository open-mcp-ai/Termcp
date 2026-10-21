package transcript

import (
	"bytes"
	"context"
	"time"
	"unicode/utf8"

	"github.com/open-mcp-ai/termcp/internal/ansi"
)

// Params is a transcript read request: which bytes to return, how to render
// them, and how long to wait for a live cursor. The field names are the shared
// contract of both surfaces - the MCP tool maps its JSON arguments here, the
// HTTP API maps its query string here.
type Params struct {
	StripAnsi bool
	Timeout   float64
	MaxLines  int
	MaxBytes  int
	ReaderID  int
	// Offset names a POSITIONAL read when non-nil, and says nothing when nil.
	//
	// The same shape as TailLines, for the same reason: 0 is a real position (the
	// first byte), so an int could not distinguish "read from the start" from "no
	// positional read at all". The sentinel was -1 and it held only because every
	// caller happened to set another selector too; a struct literal that omitted
	// this field read from byte 0 without saying so.
	Offset *int64
	// TailLines names a LINE TAIL when non-nil, and says nothing when nil.
	//
	// A pointer, because ZERO IS A REAL REQUEST: `--tail 0` asks for zero lines,
	// which is a different instruction from "no tail at all" — and the two used to
	// share the value 0, so the smallest request produced the largest answer (the
	// whole buffer). No in-band sentinel can fix that for an int: -1 can say "not
	// given", but 0 would still be ambiguous in a struct literal that simply omits
	// the field. nil is unambiguous, and the compiler makes every caller state which
	// one it means.
	TailLines *int
	FromLine  int64
	Stream    string // "", "all", "o", "e"
	// TailBytes forces the byte-tail read (last MaxBytes aligned to a line
	// start) even on a live shell. The MCP tool derives it from tail_lines and
	// the closed-shell default; the HTTP tail=1 parameter sets it directly.
	TailBytes bool
	// RequireUTF8 refuses a window whose bytes are not valid UTF-8 instead of
	// returning a string that its transport will silently alter.
	//
	// It is opt-in because the two surfaces need opposite things. A reply that
	// becomes JSON cannot carry arbitrary bytes — encoding/json substitutes U+FFFD,
	// which shifts offsets and corrupts content invisibly — so the model-facing
	// read wants to fail loudly. A terminal repaint (the Web UI, StripAnsi=false)
	// is drawing the bytes it was given and is expected to be handed whatever is
	// there; refusing its window would take the terminal down over output it can
	// display perfectly well.
	RequireUTF8 bool
}

// render turns this read's bytes into its output string, enforcing the
// byte-fidelity contract only where the caller asked for it (see RequireUTF8).
func (p Params) render(raw []byte) (string, error) {
	if p.RequireUTF8 {
		return Render(raw, p.StripAnsi)
	}
	return clean(raw, p.StripAnsi), nil
}

// ValidateParams checks the parts of the read contract both surfaces share.
// Surface-specific ranges (the MCP tool's blocking timeout) stay with the
// surface. The messages are part of the tool contract: callers get told why
// instead of silently getting a different read.
func ValidateParams(p Params) error {
	if p.TailLines != nil && *p.TailLines < 0 {
		return errf(ErrInvalid, "tail_lines must be >= 0, got %d", *p.TailLines)
	}
	if p.Offset != nil && *p.Offset < 0 {
		return errf(ErrInvalid, "offset must be >= 0, got %d", *p.Offset)
	}
	if p.FromLine < 0 {
		return errf(ErrInvalid, "from_line must be >= 0, got %d", p.FromLine)
	}
	if p.Stream != "" && p.Stream != "all" && p.Stream != "o" && p.Stream != "e" {
		return errf(ErrInvalid, "stream must be \"all\", \"o\" or \"e\", got %q", p.Stream)
	}
	// from_line picks the window by line number, so it replaces the byte/cursor
	// selectors rather than combining with them: each names a different position
	// for the same read, and honoring both would mean silently choosing one.
	if p.FromLine > 0 {
		if p.TailLines != nil {
			return errf(ErrInvalid, "from_line and tail_lines are mutually exclusive; use one selector per read")
		}
		if p.Offset != nil {
			return errf(ErrInvalid, "from_line and offset are mutually exclusive; use one selector per read")
		}
		if p.ReaderID > 0 {
			return errf(ErrInvalid, "from_line addresses the persisted log; reader_id is a live cursor — use one selector per read")
		}
	}
	return nil
}

// Window is the slice of a shell's output that a read returned, plus where that
// slice sits in the whole stream. Those offsets are what lets a caller page:
// End is where the next read starts, and HasMore says whether anything follows
// it.
//
// Every byte field describes the RAW stream; Output is the rendering of it
// (ANSI-stripped when asked, stream-filtered when asked). PartialTail is that
// rule applied to line boundaries: how many bytes at the end of the raw window
// follow the last '\n' (0 = the window ends on a line boundary). The FromLine
// group is filled only by the from_line selector, which reads whole lines from
// the line index.
type Window struct {
	Output string
	// Raw is the bytes Output was rendered from, byte-for-byte. Callers that page
	// by offset need this rather than len(Output): rendering (ANSI stripping,
	// escapes, and the U+FFFD JSON would otherwise substitute) changes the length,
	// so an offset derived from the rendered string points at the wrong byte.
	Raw         []byte
	Start       int64
	End         int64
	Total       int64
	HasMore     bool
	PartialTail int64
	FromLine    int64
	ToLine      int64
	TotalLines  int64
	LineBytes   []int64
}

// Read produces the window a read asked for. The four forms share a result
// shape but not a source, which is the point of one read entry point: a tail or
// a positional read copies bytes, so it works on a live shell and on a closed
// one alike, while a reader_id read is a live cursor that consumes the stream
// and only exists while the shell does.
func Read(ctx context.Context, src *Source, p Params) (Window, error) {
	if err := ValidateParams(p); err != nil {
		return Window{}, err
	}
	if p.ReaderID > 0 && src.Live == nil {
		return Window{}, errf(ErrInvalid, "reader_id requires a live shell; closed sessions are read with offset/tail_lines")
	}

	var w Window
	switch {
	case p.FromLine > 0:
		// Line-addressed read, served from the persisted log: log.bin is the
		// stream of record for live and DEAD shells alike, and the line index
		// (log.lines) numbers exactly those bytes. Reading the live buffer here
		// could race its own compaction for long-past lines; the log cannot.
		if src.MsgMgr == nil {
			return Window{}, errf(ErrInvalid, "from_line is unavailable: this transcript has no persistent log")
		}
		totalLines, err := src.MsgMgr.LineCount(src.SessID, src.ShellID)
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		if p.FromLine > totalLines {
			return Window{}, errf(ErrInvalid, "from_line %d out of range (stream has %d complete line(s))", p.FromLine, totalLines)
		}
		total, err := src.Len()
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		start, err := src.MsgMgr.LineStartOffset(src.SessID, src.ShellID, p.FromLine)
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		max := p.MaxBytes
		if max <= 0 {
			max = int(total - start)
			if max < 0 {
				max = 0
			}
		}
		raw, _, err := src.MsgMgr.OutputByteRange(src.SessID, src.ShellID, start, max)
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		raw = truncateAtLines(raw, p.MaxLines)
		end := start + int64(len(raw))
		// The window starts at a line start, so counting its newlines numbers
		// every complete line it holds, and their byte lengths come from the
		// gaps between them - no second pass over the index needed.
		newlines := int64(bytes.Count(raw, []byte{'\n'}))
		lineBytes := make([]int64, 0, newlines)
		prev := start - 1
		for i, c := range raw {
			if c == '\n' {
				lineBytes = append(lineBytes, start+int64(i)-prev)
				prev = start + int64(i)
			}
		}
		filtered, err := filterStream(raw, start, src, p.Stream)
		if err != nil {
			return Window{}, err
		}
		rendered, err := p.render(filtered)
		if err != nil {
			return Window{}, err
		}
		w = Window{
			Output:      rendered,
			Raw:         filtered,
			Start:       start,
			End:         end,
			Total:       total,
			HasMore:     end < total,
			PartialTail: rawPartialTail(raw),
			FromLine:    p.FromLine,
			ToLine:      p.FromLine - 1 + newlines,
			TotalLines:  totalLines,
			LineBytes:   lineBytes,
		}
	case p.TailLines != nil || p.TailBytes || (src.Live == nil && p.Offset == nil):
		raw, st, tot, err := src.scanTailWindow(p.TailLines, p.MaxBytes)
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		filtered, err := filterStream(raw, st, src, p.Stream)
		if err != nil {
			return Window{}, err
		}
		rendered, err := p.render(filtered)
		if err != nil {
			return Window{}, err
		}
		w = Window{Output: rendered, Raw: filtered, Start: st, End: tot, Total: tot, PartialTail: rawPartialTail(raw)}
	case p.Offset != nil:
		offset := *p.Offset
		total, err := src.Len()
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		max := p.MaxBytes
		if max <= 0 {
			max = int(total - offset)
			if max < 0 {
				max = 0
			}
		}
		raw, _, err := src.ByteRange(offset, max)
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		raw = truncateAtLines(raw, p.MaxLines)
		end := offset + int64(len(raw))
		filtered, err := filterStream(raw, offset, src, p.Stream)
		if err != nil {
			return Window{}, err
		}
		rendered, err := p.render(filtered)
		if err != nil {
			return Window{}, err
		}
		w = Window{Output: rendered, Raw: filtered, Start: offset, End: end, Total: total, HasMore: end < total, PartialTail: rawPartialTail(raw)}
	default:
		// Live streaming cursor path (unchanged semantics).
		pre := src.Live.ReaderCursor(p.ReaderID)
		if pre < 0 {
			return Window{}, errf(ErrReader, "reader_id %d is not registered on this shell", p.ReaderID)
		}
		out, raw, partial, err := src.Live.ReadTerminalStream(ctx, p.ReaderID, time.Duration(p.Timeout*float64(time.Second)), p.StripAnsi, p.MaxLines, p.MaxBytes)
		if err != nil {
			return Window{}, errf(ErrOperation, "%s", err.Error())
		}
		end := src.Live.ReaderCursor(p.ReaderID)
		total := src.Live.BufferLen()
		// The browser path strips ANSI inside ReadTerminalStream, so `out` is the
		// rendered form and `raw` is what it was rendered from. Checking the RAW
		// bytes is what makes the refusal honest: a stripped string could pass
		// utf8.Valid while the stream it came from was binary.
		if p.RequireUTF8 {
			if err := RenderCheck(raw); err != nil {
				return Window{}, err
			}
		}
		output := out
		if p.Stream != "" && p.Stream != "all" {
			filtered, ferr := filterStream(raw, pre, src, p.Stream)
			if ferr != nil {
				return Window{}, ferr
			}
			output = clean(filtered, p.StripAnsi)
		}
		w = Window{Output: output, Raw: raw, Start: pre, End: end, Total: total, HasMore: end < total, PartialTail: partial}
	}
	return w, nil
}

// clean renders raw bytes for a consumer: stripping is optional (the Web UI's
// terminal repaints raw bytes; model-facing surfaces want text).
func clean(raw []byte, stripAnsi bool) string {
	if !stripAnsi {
		return string(raw)
	}
	return ansi.Compact(ansi.Strip(string(raw)))
}

// Render turns raw window bytes into the string a caller receives.
//
// It refuses to hand back a string that cannot survive the trip: a Go string can
// hold arbitrary bytes, but every consumer of this value re-encodes it as JSON,
// and encoding/json REPLACES invalid UTF-8 with U+FFFD instead of failing. That
// substitution is silent and irreversible, and it is not cosmetic — it shifts
// byte offsets, so a caller that pages by offset (mcp recv) reads from the wrong
// place, and a caller that inspects magic bytes sees \ufffd where 0x89 was.
//
// So the loss is refused here, where the raw bytes are still available to name
// the offending offset, rather than reported as mojibake three layers up. The
// caller's remedy is a byte-oriented read: text mode renders escapes, and the
// window boundaries stay byte-exact.
func renderInvalidUTF8(raw []byte) error {
	i := invalidUTF8Index(raw)
	return errf(ErrInvalid, "the window contains bytes that are not valid UTF-8 "+
		"(first at byte %d of %d); a JSON reply would replace them with U+FFFD and silently "+
		"shift every offset after them. Read this range byte-wise instead "+
		"(mode=hex or mode=file) or ask for a narrower window", i, len(raw))
}

// Render applies clean and enforces the byte-fidelity contract above.
func Render(raw []byte, stripAnsi bool) (string, error) {
	out := clean(raw, stripAnsi)
	if !utf8.ValidString(out) {
		return "", renderInvalidUTF8(raw)
	}
	return out, nil
}

// invalidUTF8Index reports where the first invalid sequence starts, for the error.
func invalidUTF8Index(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}

// filterStream keeps only the bytes of raw (which sits at stream offset start)
// that stream asks for: "o" keeps everything outside the recorded stderr
// ranges, "e" the bytes inside them, "" and "all" return raw unchanged. The
// filter never moves an offset - the window's Start/End describe the raw
// window whatever it returns; only the returned content changes.
func filterStream(raw []byte, start int64, src *Source, stream string) ([]byte, error) {
	if stream == "" || stream == "all" {
		return raw, nil
	}
	if src.MsgMgr == nil {
		return nil, errf(ErrInvalid, "stream filtering requires a persistent transcript log")
	}
	ranges, err := src.MsgMgr.StderrRanges(src.SessID, src.ShellID)
	if err != nil {
		return nil, errf(ErrOperation, "%s", err.Error())
	}
	return selectBytes(raw, start, ranges, stream == "e"), nil
}

// selectBytes is the byte-level half of the filter, over prepared ranges.
func selectBytes(raw []byte, start int64, stderrRanges [][2]int64, stderr bool) []byte {
	if stderr && len(stderrRanges) == 0 {
		return nil
	}
	end := start + int64(len(raw))
	var out []byte
	pos := start // for !stderr: the start of the stdout gap not yet copied
	for _, r := range stderrRanges {
		rs, re := r[0], r[1]
		if rs < start {
			rs = start
		}
		if re > end {
			re = end
		}
		if rs >= re {
			continue
		}
		if stderr {
			out = append(out, raw[rs-start:re-start]...)
			continue
		}
		if rs > pos {
			out = append(out, raw[pos-start:rs-start]...)
		}
		pos = re
	}
	if !stderr && pos < end {
		out = append(out, raw[pos-start:]...)
	}
	return out
}

// rawPartialTail is the read window's line-boundary fact: bytes after the
// last '\n' of the RAW window. Computed on raw, never on the cleaned text —
// the stream is what a line protocol is reading, and cleaning may merge or
// drop its line endings.
func rawPartialTail(raw []byte) int64 {
	partial := int64(len(raw))
	if idx := bytes.LastIndexByte(raw, '\n'); idx >= 0 {
		partial -= int64(idx + 1)
	}
	return partial
}

// countLines counts newline-delimited lines in raw bytes; a trailing partial
// line (no closing '\n') counts as one line.
func countLines(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	n := int64(bytes.Count(b, []byte{'\n'}))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// truncateAtLines keeps only complete newline-terminated lines up to maxLines.
// Buffer parity: when the window does not reach the end of the stream, an
// incomplete trailing line stays in the stream for the next page.
func truncateAtLines(raw []byte, maxLines int) []byte {
	if maxLines <= 0 {
		return raw
	}
	seen := 0
	for i, c := range raw {
		if c != '\n' {
			continue
		}
		seen++
		if seen == maxLines {
			return raw[:i+1]
		}
	}
	return raw
}

// tailScanCeiling caps backward tail scan at 8 MiB of raw bytes.
const tailScanCeiling = 8 << 20

// defaultTailCap is the DEAD default read: last 8 KiB.
const defaultTailCap = 8192

// scanTailWindow returns the last tailLines lines of the stream (or, when
// tailLines == 0, the last maxBytes bytes aligned to a line start) via a
// bounded backward scan, plus the raw start offset of the returned window.
// The returned window always ends at the stream end.
func (o *Source) scanTailWindow(tailLines *int, maxBytes int) (raw []byte, start, total int64, err error) {
	var raw0 []byte
	total, err = o.Len()
	if err != nil || total == 0 {
		return nil, 0, total, err
	}
	if tailLines != nil && *tailLines == 0 {
		// AN EXPLICIT ZERO: an empty window at the end of the stream.
		return nil, total, total, nil
	}
	if tailLines == nil {
		// No line limit: the byte tail, aligned to a line start.
		cap := int64(maxBytes)
		if cap <= 0 {
			if o.Live != nil {
				cap = defaultTailCap
			} else {
				cap = total
			}
		}
		if cap > total {
			cap = total
		}
		start = total - cap
		raw0, _, err = o.ByteRange(start, int(cap))
		if err != nil {
			return nil, 0, total, err
		}
		if start > 0 {
			if idx := bytes.IndexByte(raw0, '\n'); idx >= 0 {
				raw0 = raw0[idx+1:]
				start += int64(idx + 1)
			}
		}
		return raw0, start, total, nil
	}

	window := int64(64 * 1024)
	for {
		if window > tailScanCeiling {
			window = tailScanCeiling
		}
		start = total - window
		if start < 0 {
			start = 0
		}
		raw0, _, err = o.ByteRange(start, int(window))
		if err != nil {
			return nil, 0, total, err
		}
		if countLines(raw0) >= int64(*tailLines) || start == 0 || window >= tailScanCeiling {
			break
		}
		window *= 2
	}

	if n := countLines(raw0); n > int64(*tailLines) {
		raw0 = takeLastLines(raw0, int64(*tailLines))
	}
	if maxBytes > 0 && int64(len(raw0)) > int64(maxBytes) {
		cut := int64(len(raw0)) - int64(maxBytes)
		if idx := bytes.IndexByte(raw0[cut:], '\n'); idx >= 0 {
			raw0 = raw0[cut+int64(idx)+1:]
		} else {
			raw0 = raw0[cut:]
		}
	}
	start = total - int64(len(raw0))
	return raw0, start, total, nil
}

// takeLastLines keeps only the final N newline-terminated lines of raw.
// A trailing '\n' terminates the last line, so it is not counted as a boundary
// of a following (empty) line: takeLastLines("A\nB\nC\n", 2) == "B\nC\n".
func takeLastLines(raw []byte, n int64) []byte {
	if n <= 0 || len(raw) == 0 {
		return nil
	}
	remaining := n
	end := len(raw)
	if raw[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if raw[i] != '\n' {
			continue
		}
		remaining--
		if remaining <= 0 {
			return raw[i+1:]
		}
	}
	return raw
}

// RenderCheck refuses raw bytes that no JSON consumer can reproduce.
//
// It is separate from Render because one path (the live cursor) has the rendered
// string built by the shell rather than here: there the assertion has to be made
// about the RAW bytes the renderer consumed, not about its output.
func RenderCheck(raw []byte) error {
	if !utf8.Valid(raw) {
		return renderInvalidUTF8(raw)
	}
	return nil
}
