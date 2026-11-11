package directive

// The summary table: one line per flag, for the verbs that have a page.
//
// WHY IT EXISTS. `help <verb>` answered with the whole page — `help read` is 10683
// bytes — and there was no way to ask a smaller question. A caller who wants to know
// what `--grep` takes pays for the essay, every time, and the verbs whose pages are
// longest are the ones with the most flags to ask about.
//
// The page is not the problem: it is the best writing in this codebase, and the
// reason a caller can learn the surface from `help` alone. What was missing was a way
// to make it opt-in. So `help <verb>` is the template plus this table, and
// `help <verb> --all` is the page, unchanged.
//
// A FLAG WITH NO LINE HERE IS NOT AN ERROR. The table falls back to the full page
// when a verb has no entries at all, so a verb migrates one at a time and a verb that
// has not migrated behaves exactly as it did. What must not happen is a verb that
// renders a table with holes in it, which is why the test asserts that every flag of a
// verb WITH entries has one: the failure mode of a hand-kept list is silence.
//
// Each line is the answer to "what does this flag take, and what happens if I get it
// wrong", not a restatement of the flag's name.
var flagSummaries = map[string]map[string]string{
	"read": {
		"from-line":  "start at a complete line; returned lines keep their real numbers",
		"tail":       "the last N lines of the window instead of the first",
		"cursor":     "byte offset to resume from (end+1 of the last Content-Range)",
		"grep":       "filter BEFORE the window; each match keeps its real line number",
		"grep-limit": "cap the matches (default 200); the reply reports the true total",
		"until":      "wait for this regex or preset (exit = OSC 133, prompt = shell prompt) instead of returning at once",
		"stream":     "o = stdout only, e = stderr only; default is both, and the other is reported as omitted",
		"encoding":   "the charset of these bytes; without it a non-UTF-8 window is refused rather than guessed",
		"raw":        "the bytes as they arrived: no ANSI stripping on a window, no escape-decoding on --at, no \\n escaping on a file read (valueless)",
		"lines":      "on a file read: number each line from the file's own first line (valueless)",
		"frame":      "read one JSON-RPC frame; advances only when the frame was complete (valueless)",
		"since":      "on a peer: the frame history after N — does not consume",
		"at":         "page inside one frame, named by its number",
		"offset":     "byte offset inside the frame named by --at",
		"bytes":      "page size for --at (4 KB default)",
		"reader":     "registered reader id to consume from; keeps its own cursor independent of the default reader",
		"mode":       "on a file read: text (default), hex for bytes, or file to download to a local_path",
	},
}

// FlagSummary returns the one-line summary for one flag of one verb, if it has one.
func FlagSummary(op, flag string) (string, bool) {
	byFlag, ok := flagSummaries[op]
	if !ok {
		return "", false
	}
	line, ok := byFlag[flag]
	return line, ok
}

// HasSummaries reports whether a verb has a summary table at all, which is the
// difference between "render the table" and "this verb has not migrated yet".
func HasSummaries(op string) bool {
	_, ok := flagSummaries[op]
	return ok
}

// SummaryExample is the one line a caller copies to get started, per verb. It is
// separate from the table because an example is not a flag and does not belong in a
// column of them.
var summaryExamples = map[string]string{
	"read": "read a1f3:1 --grep 'error|fail' --tail 5        # the last 5 failures, with line numbers",
}

// SummaryExample returns the worked example for a verb, if it has one.
func SummaryExample(op string) (string, bool) {
	line, ok := summaryExamples[op]
	return line, ok
}
