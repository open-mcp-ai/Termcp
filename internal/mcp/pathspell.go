package mcp

import (
	"encoding/json"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/mcp"
)

// WINDOWS BACKSLASHES ARE ACCEPTED, AND THE REPLY SAYS SO (issue #186, the adaptation
// half).
//
// The rule lives in sftp.WireSpelling and is deliberately narrow: a drive-prefixed path
// containing a backslash is rewritten to the POSIX wire form, everything else is left
// exactly as typed, because `\` is an ordinary byte in a POSIX filename.
//
// WHERE IT IS APPLIED MATTERS MORE THAN THE RULE. This runs before admission, on the
// arguments the call is about to run with, so three things read ONE path:
//
//  1. the session's path whitelist (guard.checkSession) — a rule declaring `/C:/tc` now
//     admits `C:\tc\x`, which is the same tree;
//  2. the handler that opens the file;
//  3. the log line, which shows what actually ran.
//
// Adapting inside the handler instead would leave the whitelist comparing the typed
// string while the file layer used the rewritten one: the two would disagree about
// which path was authorised, and the disagreement would only ever surface as either a
// 403 on a path the caller was told was allowed, or an operation reaching a tree the
// template never granted.
//
// THE ADAPTATION IS VISIBLE, never silent. `path_adapted` names both spellings in the
// reply — one line, only when it happened — because a caller that is not told will keep
// writing the spelling that only works by accident, and the operator editing `paths`
// needs to know which spelling their rule was matched against.
func adaptCallPaths(g gate, args map[string]any) []string {
	if len(args) == 0 || !g.known {
		return nil
	}
	var adapted []string
	for _, key := range g.PathArgs {
		if isLocalPathArg(g, key) {
			// THE CALLER'S OWN MACHINE, NOT THE SESSION'S. `local_path` is opened by this
			// process, so a Windows path there is already correct and rewriting it would
			// point at a file that does not exist.
			continue
		}
		raw := getString(args, key, "")
		if raw == "" {
			continue
		}
		spelled, ok := sftp.WireSpelling(raw)
		if !ok {
			continue
		}
		args[key] = spelled
		adapted = append(adapted, raw+" -> "+spelled)
	}
	return adapted
}

// isLocalPathArg reports whether a path argument names a path on the caller's machine.
func isLocalPathArg(g gate, key string) bool {
	for _, local := range g.LocalPathArgs {
		if local == key {
			return true
		}
	}
	return false
}

// adaptedNote renders the notice as the trailing line a receipt carries. One spelling
// for every door, so a caller learns to recognise it once.
func adaptedNote(adapted []string) string {
	if len(adapted) == 0 {
		return ""
	}
	return "\nPath-Adapted: " + strings.Join(adapted, "; ")
}

// noteAdaptedPaths puts the adaptation into the reply the caller reads.
//
// A JSON DOCUMENT GETS A KEY, NOT A TRAILING LINE. The file tools answer with a document
// (`{"bytes_read":12,"data":…}`), and the terminal language renders that document as a
// `key: value` receipt by parsing it — a line appended after the JSON would make it
// unparseable, and the receipt would come back as raw JSON with the notice stuck on the
// end. Adding a key keeps the shape; a non-document reply (a plain sentence) gets the
// line, because there is nothing to parse there.
func noteAdaptedPaths(res *mcp.CallToolResult, adapted []string) *mcp.CallToolResult {
	if res == nil || len(adapted) == 0 || len(res.Content) == 0 {
		return res
	}
	note := strings.Join(adapted, "; ")
	out := *res
	out.Content = append([]mcp.TextContent(nil), res.Content...)
	text := out.Content[0].Text
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err == nil && doc != nil {
		doc["path_adapted"] = note
		// A FAILURE DOCUMENT CARRIES ITS SENTENCE IN `error`, and everything that renders
		// a failure — slotFromFailure, the terminal language's receipt — reads that field
		// and drops the rest. So the notice goes into BOTH: the key for a caller reading
		// the document, and the sentence for the caller reading the refusal, which is the
		// one who needs it most.
		if msg, ok := doc["error"].(string); ok {
			doc["error"] = msg + "\nPath-Adapted: " + note
		}
		if b, merr := json.Marshal(doc); merr == nil {
			out.Content[0].Text = string(b)
			return &out
		}
	}
	out.Content[0].Text = text + "\nPath-Adapted: " + note
	return &out
}
