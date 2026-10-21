// Package envelope renders one tool reply slot as an HTTP-style response.
//
// The format is HTTP because a model has decades of prior on it: a status line
// ("TERM/1 206 Partial Content"), header lines, a blank line, then the bytes. A
// status code says what happened to THIS command; the headers only carry what is
// news (a handle it did not know, a channel that just died) or what changes the
// next decision (how many bytes remain, so it knows to continue with --cursor).
//
// Two projections of one value: Text for the model, Structured for a client that
// wants to branch programmatically. Both are produced HERE, from one value, so
// they cannot disagree about which keys exist — header key sets are compared in
// the tests, and both carry the body verbatim. Omitting it from Structured would
// lose the bytes for a client that renders only that projection.
package envelope

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Protocol is the version token on the status line.
const Protocol = "TERM/1"

// Slot is one response unit: one statement, one target in a batch, or one frame.
type Slot struct {
	// Code is the HTTP status code (200, 206, 400, …).
	Code int
	// Reason is the status line's text; empty means the canonical phrase.
	Reason string
	// Headers are the envelope fields, keyed by their wire name. Order in the
	// rendered text is fixed by headerOrder so two identical replies are
	// byte-identical.
	Headers map[string]string
	// Body is the raw payload: bytes read from a stream, or generated text (a
	// `ps` listing, an `info` document).
	Body string
}

// canonicalReason is the phrase for every code the tool can return. Keeping the
// table here (rather than relying on net/http's) means the wire format does not
// change if the standard library renames a phrase.
var canonicalReason = map[int]string{
	200: "OK",
	202: "Accepted",
	204: "No Content",
	206: "Partial Content",
	400: "Bad Request",
	403: "Forbidden",
	404: "Not Found",
	409: "Conflict",
	422: "Unprocessable",
	500: "Internal Server Error",
}

// headerOrder is the render order, and the reason a reply is reproducible: the
// status line, then the command echo, then the state lines, then the window
// lines, then the teaching/diagnosis lines.
var headerOrder = []string{
	"Path",
	"Status",
	"Exit-Code",
	"Handle",
	"Content-Range",
	"Error-Code",
	"Idempotent",
	"Usage",
}

// reasonFor returns the phrase for a code.
func reasonFor(code int) string {
	if r, ok := canonicalReason[code]; ok {
		return r
	}
	return "Status"
}

// StatusLine is the first line of the rendered form.
func (s Slot) StatusLine() string {
	reason := s.Reason
	if reason == "" {
		reason = reasonFor(s.Code)
	}
	return fmt.Sprintf("%s %d %s", Protocol, s.Code, reason)
}

// IsError reports whether this slot is a failure. isError in the tool reply is
// exactly this, for every slot: a 206 that returned the requested range is a
// success even though it is partial, and a 500 whose command failed is a failure
// even though the tool call itself worked.
func (s Slot) IsError() bool { return s.Code >= 400 }

// ContentRange renders the RFC 9110 form for a window of a byte stream.
//
// total < 0 means the length is unknown and is rendered as `*`. end is
// INCLUSIVE, as the RFC specifies, which is also what makes `end+1` the next
// --cursor.
func ContentRange(start, end, total int64) string {
	if total < 0 {
		return fmt.Sprintf("bytes %d-%d/*", start, end)
	}
	return fmt.Sprintf("bytes %d-%d/%d", start, end, total)
}

// UnsatisfiedRange renders the `*/total` form: a reply that carries no range (a
// 204), but where the caller still benefits from knowing how long the stream is.
// Without it, an empty reply says nothing about whether anything exists.
func UnsatisfiedRange(total int64) string {
	if total < 0 {
		return "bytes */*"
	}
	return fmt.Sprintf("bytes */%d", total)
}

// Cut truncates the body to at most keep bytes and rewrites Content-Range to
// describe what is actually returned.
//
// The two must move together: a range that claims bytes the body no longer
// carries is worse than no range at all, because the caller's next read starts
// where the header said and skips the rest of the stream.
func (s Slot) Cut(keep int) Slot {
	if keep >= len(s.Body) {
		return s
	}
	if keep < 0 {
		keep = 0
	}
	start, _, ok := parseRange(s.Headers["Content-Range"])
	if !ok {
		s.Body = s.Body[:keep]
		return s
	}
	total := rangeTotal(s.Headers["Content-Range"])
	s.Body = s.Body[:keep]
	s.Headers["Content-Range"] = ContentRange(start, start+int64(keep)-1, total)
	if keep == 0 {
		s.Headers["Content-Range"] = UnsatisfiedRange(total)
	}
	return s
}

// parseRange reads back the start and end of a Content-Range this package wrote.
func parseRange(v string) (start, end int64, ok bool) {
	v, found := strings.CutPrefix(v, "bytes ")
	if !found {
		return 0, 0, false
	}
	span, _, found := strings.Cut(v, "/")
	if !found {
		return 0, 0, false
	}
	lo, hi, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(lo, 10, 64)
	end, err2 := strconv.ParseInt(hi, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return start, end, true
}

// rangeTotal reads the total out of a Content-Range, or -1 when unknown.
func rangeTotal(v string) int64 {
	_, total, found := strings.Cut(v, "/")
	if !found || total == "*" {
		return -1
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// Text renders the whole slot: status line, ordered headers, blank line, body.
//
// The body is appended verbatim. A body that does not end in a newline is NOT
// given one: the envelope is a description of bytes, and adding a byte would
// break "the body is the stream".
func (s Slot) Text() string {
	var b strings.Builder
	b.WriteString(s.StatusLine())
	b.WriteByte('\n')
	for _, key := range orderedKeys(s.Headers) {
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(s.Headers[key])
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(s.Body)
	return b.String()
}

// orderedKeys returns the headers in render order, with any header this build
// does not know about placed after the known ones in alphabetical order. The
// fallback exists so a new header is never silently dropped from the text while
// appearing in the structured projection (which would break the key-set
// equality the two are supposed to have).
func orderedKeys(headers map[string]string) []string {
	known := make(map[string]bool, len(headerOrder))
	out := make([]string, 0, len(headers))
	for _, k := range headerOrder {
		known[k] = true
		if _, ok := headers[k]; ok {
			out = append(out, k)
		}
	}
	var extra []string
	for k := range headers {
		if !known[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// structValue converts a header's string value to the JSON type its name
// implies, so the two projections agree on the VALUE and not just the key.
//
// Only the numeric fields are converted: a status code and an exit code are
// numbers, while Content-Range is a URI-syntax string. Nothing else is
// reinterpreted, because a second interpretation of a value is a second truth.
func structValue(key, value string) any {
	switch key {
	case "Exit-Code":
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	}
	return value
}

// Structured renders the slot as the JSON value that mirrors it: the status line
// as three keys, then every header by its own name, then the body.
//
// The body is always included. A client that renders only this projection is the
// reason the field exists, so omitting it would leave that client with no bytes
// at all. The TEXT projection is where the redundancy gets resolved (see
// Reply.StructuredOnly) — this one stays unconditional.
func (s Slot) Structured() map[string]any {
	out := make(map[string]any, len(s.Headers)+4)
	out["protocol"] = Protocol
	out["code"] = s.Code
	reason := s.Reason
	if reason == "" {
		reason = reasonFor(s.Code)
	}
	out["reason"] = reason
	for _, key := range orderedKeys(s.Headers) {
		out[key] = structValue(key, s.Headers[key])
	}
	out["body"] = s.Body
	return out
}

// Reply assembles slots into the tool result's two projections.
type Reply struct {
	Slots []Slot

	// StructuredContent also sends the reply as its JSON projection. It is OFF by
	// default, and that default is the fix rather than a preference: the two
	// projections carry the SAME bytes, so sending both bills every reply twice and
	// shows a human the same answer in two dialects on every call. The text form is
	// what a caller usually reads, so the JSON twin is the copy almost nobody
	// consumes — it has to be asked for.
	//
	// Set by the caller (term's structured_content argument) and travels with the
	// reply because both projections are produced in one place, and the choice
	// belongs to the value rather than to whichever transport renders it.
	StructuredContent bool
}

// Text renders every slot, separated by one blank line between responses (an
// HTTP message ends at its body; the separator is what makes two responses two).
//
// An empty string when the reply asked for the structured projection only — see
// Reply.StructuredOnly. The caller (the tool result) then sends no text content,
// which is the point: a client reading only structuredContent gets the same
// answer for half the bytes.
func (r Reply) Text() string {
	parts := make([]string, 0, len(r.Slots))
	for _, s := range r.Slots {
		parts = append(parts, s.Text())
	}
	return strings.Join(parts, "\n")
}

// Structured renders every slot as an array, index for index with Text's slots.
func (r Reply) Structured() []map[string]any {
	out := make([]map[string]any, 0, len(r.Slots))
	for _, s := range r.Slots {
		out = append(out, s.Structured())
	}
	return out
}

// IsError reports whether ANY slot failed. The tool result's isError is the OR of
// the slots: a batch where one target failed is a reply that contains a failure,
// and hiding it behind a false would make the flag untrustworthy. The per-slot
// truth is still in each slot's status line and its structured twin.
func (r Reply) IsError() bool {
	for _, s := range r.Slots {
		if s.IsError() {
			return true
		}
	}
	return false
}
