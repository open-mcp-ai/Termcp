package mcp

// CROSS-FILE SEARCH, ANSWERED FROM HERE (issue #357 §1.1).
//
// `read <target> file <path> --grep RE` filtered nothing: the flag parsed, was never
// passed to the handler, and the reply was the whole file. A caller who names a filter and
// receives unfiltered content has the one kind of answer this project refuses to ship —
// a confidently correct reply about a different question.
//
// THE SEARCH RUNS IN THIS PROCESS, over SFTP, and that is a design decision rather than an
// implementation detail:
//
//   - ONE SEMANTICS EVERYWHERE. The issue's own invariant is that the `rg` and no-`rg`
//     paths must not diverge, "or scripts split into two behaviour sets". The cheapest way
//     to guarantee that is to have one path: Go's regexp over the bytes, which are the
//     bytes read from a POSIX host or a Windows one. There is no remote `grep`, so there
//     is no dialect, no `Select-String` output format to parse, and no difference between
//     a host that has `rg` and one that does not.
//   - IT WORKS WHERE ONLY FILES DO. A session opened for file access may have no shell on
//     the far side at all; asking the target to search itself would make this verb depend
//     on a capability the rest of the file surface does not need.
//   - IT IS BOUNDED BY THE SAME CAPS AS EVERYTHING ELSE. The walk reuses the directory
//     listing's entry and depth caps, and every file read has its own byte ceiling, so a
//     search that cannot finish says so instead of flooding the caller's context.
//
// The cost is that the bytes travel. That is why the caps below exist and why each one
// names itself in the trailer: `no match` and `did not look there` must never arrive in
// the same shape.

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	filesftp "github.com/open-mcp-ai/termcp/internal/sftp"
	mcpgo "github.com/open-mcp-ai/termcp/mcp"
)

const (
	// maxSearchFileBytes bounds ONE file's contribution. A search reads whole files into
	// this process, and a 2 GB log would be a resource decision made by a flag; the cap
	// reports the file it skipped instead.
	maxSearchFileBytes = 1 << 20
	// maxSearchMatches bounds the reply. Unlike a channel read there is no cursor to page
	// with — the answer is a list — so the limit is a refusal to grow, and the trailer
	// names the flag that changes it.
	maxSearchMatches = 200
	// maxSearchContext bounds the context lines on each side, like grep's own -C.
	maxSearchContext = 5
	// searchProbeBytes is how far into a file the binary check looks.
	searchProbeBytes = 8192
)

// fileSearch is one search request, already validated.
type fileSearch struct {
	re         *regexp.Regexp
	nameGlob   string
	fileGlob   string
	context    int
	maxMatches int
}

// searching reports whether this read is a search rather than a plain read.
func (f fileSearch) searching() bool { return f.re != nil || f.nameGlob != "" }

// searchTrailer counts what the walk did, so a short answer says why it is short.
type searchTrailer struct {
	files    int
	matches  int
	capped   bool
	skipped  []string // reasons, in the order they were hit
	depth    int
	root     string
	pattern  string
	nameGlob string
}

func (t *searchTrailer) note(reason string) { t.skipped = append(t.skipped, reason) }

// line renders the closing summary.
//
// IT IS A COMMENT LINE, `#`-prefixed, so `read … | write …` into a file still produces
// something a shell can grep, and it is one line so it costs one line of context.
func (t *searchTrailer) line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d file(s) under %s, depth %d; %d match(es)", t.files, t.root, t.depth, t.matches)
	if t.capped {
		fmt.Fprintf(&b, ", limit %d reached (raise --grep-limit or narrow with --glob)", maxSearchMatches)
	}
	if len(t.skipped) > 0 {
		// NAMED, NOT COUNTED. "3 skipped" leaves the caller unable to tell a binary blob
		// from the file they were looking for; the first few names cost one line.
		shown := t.skipped
		if len(shown) > 3 {
			shown = shown[:3]
		}
		fmt.Fprintf(&b, "; skipped %d: %s", len(t.skipped), strings.Join(shown, ", "))
	}
	return b.String()
}

// matchLines renders the matches of one file's text.
//
// THE SHAPE IS GREP'S, deliberately: `path:line:text` for a hit and `path-line-text` for
// context, because a caller that has used grep can read this without a legend, and one
// that pipes it into something already knows the delimiters. Context lines are only
// produced when `--context` asked for them.
func matchLines(fileLabel string, text string, f fileSearch, t *searchTrailer) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	// A trailing newline produces one empty final element; it is not a line.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	hit := make([]bool, len(lines))
	any := false
	for i, l := range lines {
		if f.re.MatchString(l) {
			hit[i] = true
			any = true
		}
	}
	if !any {
		return nil
	}
	sep := ":"
	ctxSep := "-"
	out := make([]string, 0, 16)
	printed := make([]bool, len(lines))
	for i := range lines {
		if !hit[i] {
			continue
		}
		lo, hi := i-f.context, i+f.context
		if lo < 0 {
			lo = 0
		}
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for j := lo; j <= hi; j++ {
			if printed[j] {
				continue
			}
			printed[j] = true
			if t.matches >= f.maxMatches {
				t.capped = true
				return out
			}
			if hit[j] {
				prefix := fileLabel
				if prefix != "" {
					prefix += sep
				}
				out = append(out, fmt.Sprintf("%s%d%s%s", prefix, j+1, sep, lines[j]))
				t.matches++
			} else {
				prefix := fileLabel
				if prefix != "" {
					prefix += ctxSep
				}
				out = append(out, fmt.Sprintf("%s%d%s%s", prefix, j+1, ctxSep, lines[j]))
			}
		}
	}
	return out
}

// fileMatchesGlob applies `--glob` to a file's base name.
//
// BASE NAME ONLY, and that is the promise: `--glob '*.go'` means "every Go file at any
// depth", which is the question callers actually have. A pattern with a separator in it is
// matched against the path instead, so `--glob 'cmd/*.go'` works as written rather than
// matching nothing.
func fileMatchesGlob(glob, fullPath, name string) bool {
	if glob == "" {
		return true
	}
	subject := name
	if strings.ContainsRune(glob, '/') {
		subject = fullPath
	}
	ok, err := path.Match(glob, subject)
	if err != nil {
		// An invalid pattern is refused before the walk starts (see parseFileSearch), so
		// reaching here means the pattern was valid for one host's rules and not another;
		// matching nothing is the safe reading.
		return false
	}
	return ok
}

// isBinaryText reports whether the bytes look like a binary file.
//
// A NUL BYTE IN THE FIRST 8 KB, which is the same test git uses and the cheapest one that
// is right about the two cases that matter: a JPEG and a UTF-16 log without a BOM. The
// second is why the check is on the DECODED text: a UTF-16 file whose `--encoding` was
// named has already been decoded here, so its NULs are gone and it is searched.
func isBinaryText(s string) bool {
	probe := s
	if len(probe) > searchProbeBytes {
		probe = probe[:searchProbeBytes]
	}
	return strings.IndexByte(probe, 0) >= 0
}

// searchRemoteTree answers `--grep`/`--name` over a directory.
//
// IT REUSES THE LISTING'S WALK, caps and error reporting: a subdirectory that cannot be
// read is listed as a failure by the same code `read <session> file <dir>` uses, so a
// search does not silently skip a tree the caller cannot see into.
func (s *Server) searchRemoteTree(ctx context.Context, cli *filesftp.Client, sessionID, root, charset string, f fileSearch, maxEntries, maxDepth int) ([]string, *searchTrailer, error) {
	listing, err := cli.ListDir(root, maxEntries, maxDepth)
	if err != nil {
		return nil, nil, err
	}
	t := &searchTrailer{root: root, depth: maxDepth}
	if listing.Truncated > 0 {
		t.note(fmt.Sprintf("%d entr(y|ies) not walked (--limit %d)", listing.Truncated, maxEntries))
	}
	if f.re != nil {
		t.pattern = f.re.String()
	}
	t.nameGlob = f.nameGlob
	for _, lerr := range listing.Entries {
		if lerr.Error != "" {
			t.note(lerr.Path + ": " + firstLineOf(lerr.Error))
		}
	}

	// DETAILS FIRST, CONTENT SECOND. `--name` is answered from the listing alone — it is
	// the cheaper question ("which paths are called this") and reading every file to
	// answer it would pay for bytes the caller did not ask about.
	if f.nameGlob != "" {
		for _, e := range listing.Entries {
			if e.IsDir || e.Error != "" {
				continue
			}
			t.files++
			if fileMatchesGlob(f.nameGlob, e.Path, e.Name) {
				if t.matches >= f.maxMatches {
					t.capped = true
					break
				}
				t.matches++
			}
		}
		out := make([]string, 0, t.matches)
		for _, e := range listing.Entries {
			if !e.IsDir && e.Error == "" && fileMatchesGlob(f.nameGlob, e.Path, e.Name) {
				out = append(out, e.Path)
			}
		}
		return out, t, nil
	}

	files := make([]filesftp.DirEntry, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		if e.IsDir || e.Error != "" {
			continue
		}
		// A LINK IS NOT FOLLOWED, the same choice `grep -r` makes for links it finds in a
		// tree. The reason here is sharper than convention: the paths rule admits the LINK
		// (it is inside the directory the caller named) and the READ follows it to whatever
		// it points at, so following links would make a symlink the one way around the
		// whitelist that this per-file check cannot see. A caller who wants the target can
		// name it: `read <session> file <link-target>` is admitted or refused on its own.
		if e.Type == "link" {
			t.note(e.Path + " is a link (not followed)")
			continue
		}
		if !fileMatchesGlob(f.fileGlob, e.Path, e.Name) {
			continue
		}
		files = append(files, e)
	}
	// SORTED, so a re-run reads the same and two hosts can be compared.
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	// EVERY FILE THE WALK READS PASSES ADMISSION FIRST (the same gate a single
	// `read <session> file <path>` passes). Without this a restricted session whose
	// whitelist names ONE file could search its parent directory and read every sibling:
	// the path it named was admitted, the paths the walk chose were never asked about.
	// A refusal is REPORTED, not skipped silently, for the same reason a subdirectory that
	// cannot be read is reported: "not there" and "not allowed" must not look alike.
	var out []string
	for _, e := range files {
		if t.matches >= f.maxMatches {
			t.capped = true
			break
		}
		if e.Size > maxSearchFileBytes {
			t.note(e.Path + " too large")
			continue
		}
		if bad, held := s.checkAdmission(ctx, "file_read", fileReadRequest(sessionID, e.Path)); bad != nil || held {
			t.note(e.Path + " " + refusalWord(bad, held))
			continue
		}
		res, rerr := cli.ReadFile(e.Path, 0, 0, "text", "", charset, true, false)
		if rerr != nil {
			t.note(e.Path + ": " + firstLineOf(rerr.Error()))
			continue
		}
		if res.Data == nil {
			continue
		}
		t.files++
		text := *res.Data
		if isBinaryText(text) {
			t.note(e.Path + " binary")
			continue
		}
		out = append(out, matchLines(e.Path, text, f, t)...)
	}
	return out, t, nil
}

// searchRemoteFile answers `--grep` over ONE file, which is the spelling the issue found
// silently unfiltered.
//
// THE PATH IS NOT PRINTED HERE, and it is the one difference from the tree form: the
// caller named this file, so every line repeating it would be paying for an answer the
// caller already knows. The tree form prints it because there the path IS the answer.
func (s *Server) searchRemoteFile(cli *filesftp.Client, remotePath, charset string, f fileSearch) ([]string, *searchTrailer, error) {
	t := &searchTrailer{root: remotePath, depth: 0}
	if f.re != nil {
		t.pattern = f.re.String()
	}
	info, err := cli.StatFile(remotePath)
	if err != nil {
		return nil, t, err
	}
	if info.Size > maxSearchFileBytes {
		t.note(remotePath + " too large")
		return nil, t, nil
	}
	res, err := cli.ReadFile(remotePath, 0, 0, "text", "", charset, true, false)
	if err != nil {
		return nil, t, err
	}
	if res.Data == nil {
		return nil, t, nil
	}
	t.files = 1
	text := *res.Data
	if isBinaryText(text) {
		t.note(remotePath + " binary")
		return nil, t, nil
	}
	return matchLines("", text, f, t), t, nil
}

// parseFileSearch validates the search flags once, before any bytes move.
//
// A BAD PATTERN IS THE CALLER'S TYPO AND IS ANSWERED AS ONE, with the flag that carries
// it. Letting it reach the walk would make it look like "nothing matched", which is a
// different statement about the same input.
func parseFileSearch(pattern, nameGlob, fileGlob string, context, limit int, singleFile bool) (fileSearch, *mcpgo.CallToolResult) {
	var f fileSearch
	if pattern != "" && nameGlob != "" {
		return f, toolError(CodeInvalidArgument, "%s",
			"`--grep` searches FILE CONTENTS and `--name` searches FILE NAMES: they are different questions and "+
				"one call cannot answer both. Send one — `--name '*.log'` lists the paths, `--grep ERROR` shows the lines.")
	}
	if nameGlob != "" && singleFile {
		return f, toolError(CodeInvalidArgument, "%s",
			"`--name` asks which paths are called something, and this call names ONE file: the question is already "+
				"answered by the path. Send a directory to search a tree, or use `--grep` to search this file's contents.")
	}
	if pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return f, toolError(CodeInvalidArgument, "%s",
				"--grep takes a regular expression and this one does not compile ("+err.Error()+
					"). An empty pattern is refused too: it matches every line and would return the file.")
		}
		f.re = re
	}
	if nameGlob != "" {
		if _, err := path.Match(nameGlob, "probe"); err != nil {
			return f, toolError(CodeInvalidArgument, "%s",
				"--name takes a glob and this one does not compile ("+err.Error()+"): `*.log` matches by base name at "+
					"any depth, and a pattern containing `/` is matched against the full path.")
		}
		f.nameGlob = nameGlob
	}
	if fileGlob != "" {
		if _, err := path.Match(fileGlob, "probe"); err != nil {
			return f, toolError(CodeInvalidArgument, "%s",
				"--glob takes a glob and this one does not compile ("+err.Error()+"): `*.go` matches by base name at "+
					"any depth, and a pattern containing `/` is matched against the full path.")
		}
		f.fileGlob = fileGlob
	}
	f.context = context
	if f.context < 0 {
		f.context = 0
	}
	if f.context > maxSearchContext {
		f.context = maxSearchContext
	}
	f.maxMatches = limit
	if f.maxMatches <= 0 {
		f.maxMatches = maxSearchMatches
	}
	if f.maxMatches > maxSearchMatches {
		f.maxMatches = maxSearchMatches
	}
	return f, nil
}

// fileReadRequest is the call one file read makes, so the walk asks admission with the
// same request shape a single read sends: same tool, same two arguments, same gate.
func fileReadRequest(sessionID, remotePath string) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "file_read"
	req.Params.Arguments = argsOf("session_id", sessionID, "remote_path", remotePath)
	return req
}

// refusalWord names why a file was not read, in the trailer's one-line vocabulary.
func refusalWord(bad *mcpgo.CallToolResult, held bool) string {
	if held {
		return "needs approval"
	}
	if bad == nil {
		return "refused"
	}
	if f := failureFromResult(bad); f != nil {
		return f.code + " " + firstLineOf(f.msg)
	}
	return "refused"
}
