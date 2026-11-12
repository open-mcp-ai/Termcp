package mcp

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/termview"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// `read --screen` answers the other question about a channel (#312).
//
// EVERY OTHER READ ON THIS SURFACE REPLIES IN THE LOG'S COORDINATES: byte offsets, lines
// of a stream, a window the caller pages with `--cursor`. That is the archive, and it is
// the right answer to "what did this channel write". It is the wrong answer to "what is on
// the screen", because a terminal is not a stream: `\r` overwrites, `CSI K` erases, `CSI H`
// moves, and a prompt repainted ten times occupies one row and ten byte ranges.
//
// THE MODEL IS THE SAME ONE THE WEB UI DRAWS WITH (internal/termview): the bytes are
// replayed at the channel's width into rows, and each row's bytes are what that row
// displays. One implementation, one answer, checked against xterm.js byte for byte.
//
// A FULL-SCREEN PROGRAM IS A SECOND BUFFER. The transcript records nothing of the
// alternate screen — the rail must not colour a program's painting — so while one is in
// control this reply answers from the model's own grid (#314), which is where the program's
// characters actually go. The ceiling is stated in the headers rather than hidden: the grid
// models where characters land, not how they look.

// screenTailRows bounds how much of a screen one reply carries when the caller does not
// say. A terminal is a few dozen rows; asking for more is asking for scrollback, which is
// what the byte read is for.
const screenTailRows = 200

// screenReadChunk is how much of the log one replay step reads. It matches the Web UI's
// rail for the same reason: the parser is amortized constant, so the chunk size decides
// only how many calls it takes.
const screenReadChunk = 256 * 1024

// replayLayout builds the channel's screen from its log. The caller decides what to ask it.
func (s *Server) replayLayout(cs *session.ChildShell) (*termview.Layout, int64, bool) {
	info := cs.Info()
	cols, rows := info.Cols, info.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	total, err := s.sessMgr.OutputSize(cs.ParentSessionID(), cs.ID)
	if err != nil {
		return nil, 0, false
	}
	layout := termview.New(cols, rows)
	for off := int64(0); off < total; {
		// ASK FOR WHAT IS LEFT, AND TAKE NO MORE. OutputSize says how many bytes this
		// channel's log holds; a read that comes back with more than that (a dead
		// session's archived transcript is served beside the live buffer) would be
		// replayed as new terminal output, and the screen then shows the whole session
		// twice — once modelled, once raw with its carriage returns intact. The size is
		// the contract, so the excess is dropped rather than modelled.
		want := min(int64(screenReadChunk), total-off)
		chunk, _, err := s.sessMgr.OutputByteRange(cs.ParentSessionID(), cs.ID, off, int(want))
		if err != nil {
			return nil, 0, false
		}
		if len(chunk) == 0 {
			break
		}
		if int64(len(chunk)) > want {
			chunk = chunk[:want]
		}
		layout.Feed(off, chunk)
		off += int64(len(chunk))
	}
	return layout, total, true
}

// channelScreenState reports whether a program is holding or drawing on this channel's
// terminal: the alternate screen is up, or the cursor is hidden while it repaints.
//
// IT REPLAYS THE LOG RATHER THAN GUESSING FROM THE TAIL: the answer is the model's, and the
// model is the only thing here that knows a `?1049h` from the same bytes printed as text.
// It runs only when a compound line is about to be typed, which is why paying for a replay
// is acceptable — the alternative is a guard that fires on the common case.
func (s *Server) channelScreenState(handle string) (altScreen bool) {
	cs, bad := s.requireShell(handle)
	if bad != nil || cs == nil {
		return false
	}
	layout, _, ok := s.replayLayout(cs)
	if !ok {
		return false
	}
	return layout.Alt() || !layout.CursorVisible()
}

// readScreen replays the channel's log and answers with the screen.
func (s *Server) readScreen(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	cs, bad := s.requireShell(stmt.Target)
	if bad != nil || cs == nil {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeShellNotFound,
			msg: "`--screen` reads a terminal channel; this target has no screen to model"})
	}
	info := cs.Info()
	cols, rows := info.Cols, info.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	layout, total, ok := s.replayLayout(cs)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed,
			msg: "the channel's log is not readable"})
	}

	want := rows
	if v, ok := stmt.Flag("tail"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			want = n
		}
	} else if win.LinesLimit > 0 {
		want = min(want, win.LinesLimit)
	}
	want = min(want, screenTailRows)

	// A FULL-SCREEN PROGRAM IS A DIFFERENT BUFFER, and answering from the transcript while
	// one is in control would answer about rows the program is not painting on.
	if alt := layout.AltLines(); alt != nil {
		if len(alt) > want {
			alt = alt[len(alt)-want:]
		}
		var body strings.Builder
		for _, row := range alt {
			body.WriteString(row)
			body.WriteByte(10)
		}
		cursorRow, cursorCol := layout.Cursor()
		return envelope.Slot{
			Code: 200,
			Headers: map[string]string{
				"Path":           stmt.Path,
				"Screen":         "the full-screen program's own screen at " + strconv.Itoa(cols) + "x" + strconv.Itoa(rows),
				"Cols":           strconv.Itoa(cols),
				"Rows":           strconv.Itoa(rows),
				"Cursor":         strconv.Itoa(cursorRow) + "," + strconv.Itoa(cursorCol),
				"Cursor-Visible": yesNo(layout.CursorVisible()),
				"Alt-Screen":     "yes",
				"Paste-Mode":     yesNo(layout.BracketedPaste()),
				"Screen-Rows":    strconv.Itoa(len(alt)),
				"Content-Range":  strconv.FormatInt(total, 10) + "/" + strconv.FormatInt(total, 10),
			},
			Body: body.String(),
		}
	}

	count := layout.RowCount()
	// PAGING IS THE SCREEN'S OWN COORDINATES (#312): `--top` names the first row in the
	// model's numbering and `--rows` how many, so a caller can walk back through output
	// that has scrolled off. Without `--top` the reply is the tail, which is what a
	// terminal shows.
	paged := false
	from := 0
	span := want
	if v, ok := stmt.Flag("top"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			paged = true
			from = min(n, count)
			span = rows
			if v2, ok := stmt.Flag("rows"); ok {
				if m, err := strconv.Atoi(v2); err == nil && m > 0 {
					span = m
				}
			}
			span = min(span, screenTailRows)
			// A WINDOW PAST THE END IS NOT A WINDOW OF BLANK ROWS. The model has `count`
			// rows; asking for more than that returns padding a caller cannot tell apart
			// from the empty rows a terminal really does show, and `Screen-Rows` then
			// describes the request instead of the answer.
			span = min(span, count-from)
		}
	}
	if !paged {
		// THE LAST N ROWS THAT SAY SOMETHING, not the last N slots of the buffer.
		//
		// Two things separate the two. The model's row list runs to the screen's height,
		// and the rows below the cursor are blank by definition — a terminal is a fixed
		// grid, not a list of lines. And a row can hold bytes that display as nothing: a
		// prompt's colour reset, an erase, the move that put the cursor there. Answering
		// `--tail 2` with either is true of the grid and useless to a caller asking what
		// happened last.
		from = max(0, count-(want+rows))
		span = count - from
	}
	// One read for the whole range, sliced per row: the rows are adjacent by construction,
	// and one read per row would be a tool call per row.
	spans := layout.Window(from, span)
	unionStart, unionEnd := int64(-1), int64(0)
	for _, sp := range spans {
		if sp == nil || sp.End <= sp.Start {
			continue
		}
		if unionStart < 0 || sp.Start < unionStart {
			unionStart = sp.Start
		}
		if sp.End > unionEnd {
			unionEnd = sp.End
		}
	}
	var tailBytes []byte
	if unionStart >= 0 {
		tailBytes, _, _ = s.sessMgr.OutputByteRange(cs.ParentSessionID(), cs.ID, unionStart, int(unionEnd-unionStart))
	}
	rowText := func(sp *termview.Span) string {
		if sp == nil || sp.End <= sp.Start || unionStart < 0 {
			return ""
		}
		lo, hi := sp.Start-unionStart, sp.End-unionStart
		if lo < 0 || hi > int64(len(tailBytes)) || hi <= lo {
			return ""
		}
		// A row's bytes are the terminal's own: the colours it painted and the erase it
		// performed describe a screen the caller cannot see, so the reply carries what is
		// displayed.
		text := ansi.Strip(string(tailBytes[lo:hi]))
		return strings.TrimRight(text, "\r\n")
	}

	var keptRows []string
	// SCREEN-TOP IS WHERE THE RETURNED ROWS START, not where the window ended. Reporting
	// the tail's end made the documented round trip a lie: a caller that asked again from
	// that `--top` got a different part of the screen, because the rows below the cursor are
	// blank and the tail skips them.
	top := from
	if paged {
		// A page is a page: empty rows stay, because a caller walking a numbered range
		// needs the numbering to line up with what it will ask for next.
		for _, sp := range spans {
			keptRows = append(keptRows, rowText(sp))
		}
	} else {
		firstKept := -1
		for row := len(spans) - 1; row >= 0 && len(keptRows) < want; row-- {
			if text := rowText(spans[row]); text != "" {
				keptRows = append(keptRows, text)
				firstKept = row
			}
		}
		// Collected newest-first; a screen reads oldest-first.
		slices.Reverse(keptRows)
		if firstKept >= 0 {
			top = from + firstKept
		}
	}

	var body strings.Builder
	for _, row := range keptRows {
		body.WriteString(row)
		body.WriteByte(10)
	}
	cursorRow, cursorCol := layout.Cursor()
	return envelope.Slot{
		Code: 200,
		Headers: map[string]string{
			"Path":           stmt.Path,
			"Screen":         "replayed from the log at " + strconv.Itoa(cols) + "x" + strconv.Itoa(rows),
			"Cols":           strconv.Itoa(cols),
			"Rows":           strconv.Itoa(rows),
			"Cursor":         strconv.Itoa(cursorRow) + "," + strconv.Itoa(cursorCol),
			"Cursor-Visible": yesNo(layout.CursorVisible()),
			"Alt-Screen":     yesNo(layout.Alt()),
			"Paste-Mode":     yesNo(layout.BracketedPaste()),
			"Screen-Top":     strconv.Itoa(top),
			"Screen-Total":   strconv.Itoa(count),
			"Screen-Rows":    strconv.Itoa(len(keptRows)),
			"Content-Range":  strconv.FormatInt(total, 10) + "/" + strconv.FormatInt(total, 10),
		},
		Body: body.String(),
	}
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
