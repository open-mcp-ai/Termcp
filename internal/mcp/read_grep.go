package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

const (
	// grepDefaultLimit bounds how many matching lines a single --grep returns.
	// A pattern like "." matches every line, so without a bound the "filtered"
	// read would be larger than the unfiltered one it replaced.
	grepDefaultLimit = 200
	// grepRecommendedLimit is the number of matches one reply should stay under, published
	// as a recommendation. It is not a ceiling: the caller pays for the reply, and a
	// silent cap would answer a different question than the one asked.
	grepRecommendedLimit = 5000
	// grepMaxScanBytes bounds how much transcript one --grep will read. The scan
	// is local (memory or a local file), but "local" is not "free": a 200 MB log
	// is still 200 MB of reading, and a caller that asked for the last 20 errors
	// does not want to wait for a full sweep of a file whose beginning is
	// irrelevant. The tail is what callers actually search.
	grepRecommendedScan = 8 << 20 // 8 MiB
)

// grepMaxScanBytes is grepRecommendedScan, and a variable ONLY so a test can lower it:
// reaching 8 MiB through a real pipe would take minutes, and the behaviour under test —
// "did the reply say the scan stopped early" — is not about the size.
var grepMaxScanBytes = grepRecommendedScan

// readGrep implements `read <handle> --grep <regex>`.
//
// WHY THIS EXISTS AND WHERE IT DOES NOT GO. Termcp's own transcript is the one
// buffer a caller cannot grep with the target's tools: the command already ran, its
// bytes are in this daemon, and pulling 2000 lines across the wire so the model can
// find 3 of them is the most expensive thing a terminal gateway can do. This filters
// locally, before the window, so `lines_limit` counts MATCHES rather than raw lines.
//
// It deliberately does NOT exist as a `term()` argument. `lines_limit` answers "how
// much reply will I get"; a pattern answers "which lines do I want". Folding them
// together makes `lines_limit` mean either "the first 200 raw lines, then filter"
// (which returns nothing when the matches are later — a false negative that reads
// like "no errors") or "scan everything, then take 200 matches" (which hides a full
// scan behind a parameter that looks like a size cap). Both are wrong, and the caller
// cannot tell which one it got. A directive states its own cost.
func (s *Server) readGrep(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// NO BLOCKING WINDOW, AND THAT IS THE WHOLE POINT OF A SEARCH. `read --grep` looks at
	// bytes that are already in the transcript; waiting for bytes that are not there
	// would make "no match" take as long as the caller's patience and answer the same.
	return s.readGrepFrom(ctx, stmt, win, 0, 0)
}

// readGrepFrom is readGrep over the region that starts at from, blocking up to
// blockFor seconds for bytes to appear.
//
// The offset is what lets `run --grep` search THIS command's output rather than the
// whole channel: a run knows where its input started, and a search that ignored that
// would answer with matches from a command the caller has already dealt with.
//
// THE BLOCKING WINDOW IS THE DIFFERENCE BETWEEN THE TWO CALLERS. A run has just typed a
// command, so its bytes are NOT there yet, and a zero window made it scan nothing and
// answer `no line matched ... in the 0 lines scanned` — honest, and useless (found by
// driving it, not by a test). The run passes the part of its window the wait did not
// spend; a plain `read --grep` passes zero.
func (s *Server) readGrepFrom(ctx context.Context, stmt directive.Statement, win termArgs, from int64, blockFor float64) envelope.Slot {
	pattern, _ := stmt.Flag("grep")
	re, err := regexp.Compile(pattern)
	if err != nil {
		return badRequest(stmt, "`--grep` is a Go regular expression and this one does not compile: "+err.Error())
	}

	limit := grepDefaultLimit
	if v, ok := stmt.Flag("grep-limit"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return badRequest(stmt, "`--grep-limit` must be a number, got "+strconv.Quote(v))
		}
		if n > 0 {
			// NO SILENT CEILING. It used to be capped at grepMaxLimit, so a caller that asked
			// for 10000 matches got 5000 and learned the ceiling only if the reply happened to
			// truncate — a space budget is the caller's to choose (it pays for the reply), and
			// a capped number is a different call than the one it made. The scan is bounded
			// separately (grepMaxScanBytes), so a large limit cannot make the server read more.
			limit = n
		}
	}

	// Read the whole transcript, not the caller's window: the window applies to the
	// MATCHES, so it must come after the filter. The read itself is bounded by
	// grepMaxScanBytes.
	args := argsOf("shell_id", stmt.Target,
		"max_bytes", float64(grepMaxScanBytes),
		"max_lines", float64(0),
		"timeout", blockFor,
	)
	if v, ok := stmt.Flag("stream"); ok {
		args["stream"] = v
	}
	if boolFlag(stmt, "raw") {
		args["strip_ansi"] = false
	}
	// THE SAME ENCODING APPLIES TO A SEARCH, and it has to: `--grep 以下` against a
	// GBK transcript compared a UTF-8 pattern with GBK bytes, scanned 282 real lines,
	// matched none, and answered 200 — indistinguishable from "that string is not
	// there" (issue #149). The decode happens in the reader, so the pattern and the
	// text it is matched against are both characters.
	if v, ok := stmt.Flag("encoding"); ok {
		args["charset"] = v
	}
	// A GREP DOES NOT CONSUME THE TRANSCRIPT: it uses a positional read (from_line 1),
	// which is stateless and never touches the caller's live cursor. An ordinary read
	// is a cursor and moves it; a search is a look, and moving the cursor on a look
	// is how a caller that greps for "error" loses the bytes it wanted to grep for
	// "warn". Two searches must see the same transcript, and a later plain read must
	// still see the bytes it would have seen.
	//
	// OFFSET 0, NOT from_line 1. The line index is built asynchronously, and a
	// channel that just exited can be readable by bytes while line N is not yet
	// indexed — the observed symptom was `Lines-Scanned: 1` on a buffer holding eight
	// lines. An offset read needs no index: the bytes are the bytes.
	args["offset"] = float64(from)

	res, fail := s.callTool(ctx, "shell_output", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}

	body := getString(doc, "output", "")
	startLine := int64(1)
	if v := int64(getFloatAny(doc, "from_line", 0)); v > 0 {
		startLine = v
	}

	var matched []string
	var hits int
	scanned := 0
	truncated := false
	for i, line := range strings.Split(body, "\n") {
		if line == "" && i == len(strings.Split(body, "\n"))-1 {
			break // the trailing newline is not a line
		}
		scanned++
		if !re.MatchString(line) {
			continue
		}
		hits++
		if len(matched) >= limit {
			truncated = true
			continue // keep counting hits so the reply can report the true total
		}
		// REAL line numbers, not match ordinals: a caller that wants context asks
		// for it with --from-line, and that number must be the file's, not this
		// reply's.
		matched = append(matched, fmt.Sprintf("%d:%s", startLine+int64(i), line))
	}

	if hits == 0 {
		// A GREP THAT MATCHED NOTHING DELIVERED NOTHING, so it is a 204 — and the count of
		// lines scanned is the half that says whether that means "no" or "not here". It
		// rides in the Hint header for the reason noContentSaying documents: a body under
		// a 204 contradicts the status line, and a 206 with no bytes would claim a
		// partial delivery that does not exist.
		return noContentSaying(map[string]string{
			"Path":          stmt.Path,
			"Content-Range": envelope.UnsatisfiedRange(int64(getFloatAny(doc, "total_bytes", 0))),
			"Grep":          pattern,
			"Lines-Scanned": fmt.Sprint(scanned),
		}, "no line matched `"+pattern+"` in the "+fmt.Sprint(scanned)+" lines scanned")
	}

	headers := map[string]string{
		"Path":          stmt.Path,
		"Grep":          pattern,
		"Lines-Scanned": fmt.Sprint(scanned),
		"Matches":       fmt.Sprint(hits),
	}
	// A SCAN THAT STOPPED EARLY SAYS SO (issue #204).
	//
	// One --grep reads at most grepMaxScanBytes of transcript, which is a real
	// protection — "the last 20 errors" should not sweep a 200 MB log — but a matched
	// count from a PARTIAL scan reads exactly like a count from a complete one. A
	// caller told "3 matches" in a 40 MB channel has no way to know that 32 MB were
	// never looked at, and that is the same failure as a silent truncation: the reply
	// answers a different question than the one asked, and says nothing.
	//
	// Scanning starts at `from` (0 for a plain grep, the command's own offset for
	// `run --grep`), so the window is [from, end) and the channel is `total`.
	if endOff := int64(getFloatAny(doc, "end_offset", 0)); endOff < int64(getFloatAny(doc, "total_bytes", 0)) {
		headers["Scan-Window"] = fmt.Sprintf(
			"searched bytes %d-%d of %d; the rest was not scanned (raise --from-line, or search a narrower tail)",
			from, endOff, int64(getFloatAny(doc, "total_bytes", 0)))
	}
	if truncated {
		headers["Truncated"] = fmt.Sprintf("showing %d of %d matches; raise --grep-limit (recommended max %d)", len(matched), hits, grepRecommendedLimit)
	}

	// The line window applies to the MATCHES: `--tail 5` on a grep means the last
	// five matching lines, which is what "the last few errors" means.
	lines := matched
	if v, ok := stmt.Flag("tail"); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < len(lines) {
			lines = lines[len(lines)-n:]
		}
	}
	if win.LinesLimit > 0 && win.LinesLimit < len(lines) {
		lines = lines[:win.LinesLimit]
		headers["Truncated"] = fmt.Sprintf("showing %d of %d matches; raise lines_limit or use --tail", win.LinesLimit, hits)
	}

	body2 := strings.Join(lines, "\n")
	if win.BytesLimit > 0 && len(body2) > win.BytesLimit {
		body2 = body2[:win.BytesLimit]
		headers["Truncated"] = "reply truncated at bytes_limit; narrow the pattern or raise the limit"
	}

	return envelope.Slot{Code: 200, Headers: headers, Body: body2}
}
