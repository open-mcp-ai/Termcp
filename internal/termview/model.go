package termview

// This package models a terminal screen from the bytes a channel wrote (#312).
//
// IT EXISTS BECAUSE TWO CALLERS NEED THE SAME ANSWER. The Web UI's timeline rail asks
// "which byte of the log is on which row", and the MCP surface asks "what is on the
// screen, where is the cursor, is a full-screen program in control". Both questions are
// answered by replaying the log through a terminal model, and a second implementation
// would be a second answer: the differential test against xterm.js
// (TestRowLayoutAgreesWithXterm) is what makes one implementation trustworthy, and it
// cannot vouch for two.
//
// THE LOG IS THE INPUT, NOT A RECORDING OF ROWS. A window resize reflows every line, so
// a stored row mapping is stale the moment the window changes; the log and the width are
// the only inputs the mapping has, so it is derived from them per request. That is also
// why nothing here holds a live socket: `Feed` takes bytes, in the order they arrived.

// Span is one terminal row's byte range in a channel's log. A null/absent entry is a row
// holding no bytes — an erased line, or a row no output ever reached — which draws no
// cell rather than reusing a neighbour's status.
//
// The rows tile the log: every byte the terminal consumed belongs to the row the cursor
// was on when it arrived, the newline that ends a row included. A row is therefore never
// the blank between two others, and a mark cannot fall between them — a zero-length input
// mark sitting on the very newline that submitted the line colours that line, which is
// the row a reader looks at when asking where the input was.
//
// Erasing is the deliberate exception: a row the terminal cleared holds nothing, because
// its bytes are not on screen any more. That is what the erase meant.
type Span struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// The model's limits. The scrollback cap is the xterm option the client sets
// (terminal-view.js): xterm drops the oldest lines past it, and because rows are indexed
// the way the terminal does, the layout has to drop them too or every row number below
// the trim point would be off by the difference. xterm's buffer holds the scrollback
// *plus* the screen (see getCorrectBufferLength in xterm.js), so the cap is raised by the
// terminal's height in capacity() rather than being a hard number.
const (
	Scrollback = 100000
	TabStop    = 8
	// MaxCSIParams bounds the parameters of a CSI sequence still in flight: the longest
	// real one is a handful of bytes, and a stream that never ends a sequence must not
	// grow the body without limit.
	MaxCSIParams = 64
)
