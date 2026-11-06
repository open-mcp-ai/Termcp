package mcp

import (
	"unicode/utf8"
)

// cdpTarget is the part of a target's description this door renders.
type cdpTarget struct {
	ID    string
	Type  string
	Title string
	URL   string
}

func describeTarget(t *cdpSession) string {
	return describeTargetFields(t.Type, t.Title, t.URL)
}

// describeTargetFields renders one target as a line a caller can scan, and BOUNDS the
// url (issue #249.3).
//
// A devtools url carries the page's whole query string, so one tracking-heavy page can
// be a kilobyte on its own and the table stops being a table: the entries no longer line
// up and a small `bytes_limit` cuts the LIST rather than the url that caused it. The
// prefix is what identifies a page in practice; the rest is there when the caller asks
// for the target by id.
//
// The url is not dropped, only shortened, and the shortening is visible ("…"), so a
// caller that needs the full value knows to get it from the target instead of mistaking
// a truncated url for the real one.
func describeTargetFields(typ, title, url string) string {
	if title == "" {
		title = "(untitled)"
	}
	if len(url) > targetURLPreview {
		// Never cut inside a UTF-8 sequence: half a character reads as mojibake and
		// makes the whole row look corrupt.
		cut := targetURLPreview
		for cut > 0 && !utf8.ValidString(url[:cut]) {
			cut--
		}
		url = url[:cut] + "…"
	}
	line := title + " — " + url
	if typ != "" && typ != "page" {
		line = typ + ": " + line
	}
	return line
}

// targetURLPreview is how much of a target's url one listing row shows.
//
// 120 is about a terminal line and comfortably past the part that identifies a page
// (scheme + host + path). It is a rendering bound, not a security one: the whole url is
// still reachable through the target's own page.
const targetURLPreview = 120
