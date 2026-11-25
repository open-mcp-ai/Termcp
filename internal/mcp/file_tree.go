package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sftp"
	mcpgo "github.com/open-mcp-ai/termcp/mcp"
)

// A DIRECTORY TARGET ON A WRITE (#340).
//
// `write` carries ONE payload, so a directory target cannot mean "write these bytes
// into it" — there are no bytes. That fact has to be said out loud twice: once when the
// switch is absent (the caller may not know that a batch form exists) and once when the
// switch is present but the payload is not a batch (the caller may think --recursive
// reads their mind).

// dirWriteRefusal is the 400 for `write <session> file <dir>` with no switch.
//
// IT NAMES THE TWO WORKING SPELLINGS, because a refusal that only says "no" leaves the
// caller to guess: a model that reads "is a directory" tries the same call again with a
// different path. The first spelling needs no flag at all — several statements in one
// cmd is ONE call, with one reply slot per file — and the second is the batch form.
func dirWriteRefusal(remotePath string) string {
	return "`" + remotePath + "` is a directory, and `write` carries the bytes of ONE file: a directory has " +
		"none of its own, so there is nothing this call could put there.\n" +
		"Two ways to write several files, both ONE call:\n" +
		"  one statement per file — `cmd: \"write <session> file " + joinRemoteForHint(remotePath, "a.txt") +
		" --data hi; write <session> file " + joinRemoteForHint(remotePath, "b.txt") + " --data yo\"`\n" +
		"  one batch payload — `write <session> file " + remotePath + " --recursive --data " +
		`'{"a.txt":"hi","sub/b.txt":"yo"}'` + "`\n" +
		"The batch form takes a JSON OBJECT of path→content, refuses a path that is absolute or escapes the " +
		"directory, and answers one line per file (preview it first with --dry-run)."
}

// dirWriteCaps bound one batch write. They are small on purpose: the payload travels
// inside a tool call, and a caller with more than this to put on a host should be
// uploading a file, not a flag.
const (
	maxTreeFiles = 100
	maxTreeBytes = 1 << 20
)

// writeTree writes a JSON object of path→content under remotePath.
//
// EVERY ENTRY IS CHECKED BEFORE ANYTHING IS WRITTEN, because the payload is one value:
// a batch that half-applied would leave the caller comparing the reply against the
// directory to work out what landed. Keys are refused by name when they are absolute,
// when they climb out of the directory, or when the payload is not an object at all.
func (s *Server) writeTree(ctx context.Context, cli *sftp.Client, sessionID, remotePath, data, mode string, dryRun bool) *mcpgo.CallToolResult {
	var files map[string]string
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&files); err != nil {
		return toolError(CodeInvalidArgument, "%s",
			"`--recursive` takes a JSON OBJECT of path→content: "+
				`--data '{"a.txt":"hi","dir/b.txt":"yo"}'`+
				".\nThis payload does not parse as one ("+err.Error()+"), so there is no way to know which file "+
				"the bytes were meant for. To write a single file, drop --recursive and name it.")
	}
	if len(files) == 0 {
		return toolError(CodeInvalidArgument, "%s",
			"`--recursive` was given an empty object, so there is nothing to write. Name at least one file: "+
				`--data '{"a.txt":"hi"}'`)
	}

	// SORTED, so the report is reproducible and a re-run reads the same.
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// VALIDATE EVERY KEY FIRST. One bad path refuses the whole call: the alternative
	// is a partial write whose boundary the caller has to reconstruct from the report.
	var total int64
	for _, p := range paths {
		if err := checkTreeKey(p); err != nil {
			return toolError(CodeInvalidArgument, "%s", err.Error())
		}
		total += int64(len(files[p]))
	}
	if len(paths) > maxTreeFiles {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf(
			"this batch names %d files, over the %d-file cap. Write the first %d and the rest in a second call, "+
				"or put the tree in an archive and copy it as one file.", len(paths), maxTreeFiles, maxTreeFiles))
	}
	if total > maxTreeBytes {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf(
			"this batch carries %d bytes, over the %d-byte cap. A payload this size belongs in a file, not in "+
				"--data: write the files one at a time, or upload an archive and unpack it with `run`.",
			total, maxTreeBytes))
	}

	// THE DIRECTORY IS CREATED WHEN IT IS ABSENT, and only then. `write <session> file
	// /srv/new --recursive` means "put this tree at /srv/new", which is the same
	// promise `mkdir -p` makes and the same one the single-file form makes about a
	// file: creating a file's parent is not creating the file.
	if _, err := cli.StatFile(remotePath); err != nil {
		if !dryRun {
			if merr := cli.MakeDir(remotePath); merr != nil {
				return toolError(CodeOperationFailed, "create directory %s: %s", remotePath, merr.Error())
			}
		}
	}

	var b strings.Builder
	written, failed := 0, 0
	var bytes int64
	for _, p := range paths {
		full := joinRemoteForHint(remotePath, p)
		if dryRun {
			b.WriteString("would write  " + full + "  " + fmt.Sprint(len(files[p])) + " bytes\n")
			written++
			bytes += int64(len(files[p]))
			continue
		}
		if parent, ok := splitRemoteParent(full); ok {
			if err := cli.MakeDir(parent); err != nil {
				failed++
				b.WriteString("FAIL  " + full + "  " + firstLineOf(err.Error()) + "\n")
				continue
			}
		}
		rep, err := cli.WriteFile(full, nil, files[p], mode, "", 0, 0)
		if err != nil {
			failed++
			b.WriteString("FAIL  " + full + "  " + firstLineOf(err.Error()) + "\n")
			continue
		}
		written++
		bytes += rep.BytesWritten
		b.WriteString("ok    " + full + "  " + fmt.Sprint(rep.BytesWritten) + " bytes\n")
	}
	verb := "wrote"
	if dryRun {
		verb = "would write"
	}
	b.WriteString("\n" + verb + " " + fmt.Sprint(written) + " file(s), " + fmt.Sprint(bytes) + " bytes")
	if failed > 0 {
		b.WriteString(", " + fmt.Sprint(failed) + " failed — a failed entry is the only one that did not happen")
	}
	b.WriteString("\n`read " + sessionID + " file " + remotePath + "` lists what is there now.")
	return mcpgo.NewToolResultText(b.String())
}

// checkTreeKey refuses a path that does not stay inside the target directory.
//
// ABSOLUTE PATHS ARE REFUSED RATHER THAN JOINED: a key of "/etc/cron.d/x" under
// /srv/data would otherwise write outside the tree the caller named, and a caller that
// typed an absolute path in a batch is far more likely to have pasted the wrong line
// than to have meant "escape the target".
func checkTreeKey(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("a batch key is empty: every key is a path relative to the target directory")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || (len(p) > 1 && p[1] == ':') {
		return fmt.Errorf("`%s` is an absolute path, and a batch key is RELATIVE to the directory named by the "+
			"target. Write `%s` (or name the absolute path as the target of its own `write`).", p, strings.TrimLeft(p, "/\\"))
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fmt.Errorf("`%s` climbs out of the target directory, so the batch would write outside the "+
				"tree the caller named. Name the directory you mean as the target instead.", p)
		}
	}
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, "\\") {
		return fmt.Errorf("`%s` ends with a separator, so it names a directory — and a batch key names a FILE "+
			"with its content. Write `%sname`.", p, p)
	}
	return nil
}

// splitRemoteParent returns the containing directory of a remote path, spelled the way
// the caller's path is spelled.
func splitRemoteParent(p string) (string, bool) {
	i := strings.LastIndexAny(p, `/\`)
	if i <= 0 {
		return "", false
	}
	return p[:i], true
}

// joinRemoteForHint joins a directory and a child for the text of a refusal, with the
// separator the target already uses.
func joinRemoteForHint(dir, name string) string {
	switch {
	case dir == "":
		return name
	case strings.HasSuffix(dir, "/"):
		return dir + name
	case strings.Contains(dir, "\\") && !strings.Contains(dir, "/"):
		return dir + "\\" + name
	default:
		return dir + "/" + name
	}
}

// renderDirListing turns a listing document into the table a reader can scan.
//
// THE TYPE COLUMN COMES FIRST because it is what the next call depends on: a directory
// can be read again with a deeper --depth, a link is where the bytes actually are, and
// a file is what `read <session> file <path>` takes. Size and mtime follow, then the
// mode, which is the third question after "what is it" and "how big".
func renderDirListing(doc map[string]any) string {
	path := getString(doc, "path", "")
	entries, _ := doc["entries"].([]any)
	var b strings.Builder
	kind := "directory"
	if len(entries) == 1 {
		if e, ok := entries[0].(map[string]any); ok {
			if t := getString(e, "type", ""); t != "dir" {
				kind = t
			}
		}
	}
	b.WriteString(path + "  (" + kind + ")\n")
	if len(entries) == 0 {
		b.WriteString("(empty)\n")
	}
	for _, raw := range entries {
		e, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t := getString(e, "type", "file")
		mark := "-"
		switch t {
		case "dir":
			mark = "d"
		case "link":
			mark = "l"
		}
		name := getString(e, "name", "")
		// BELOW THE FIRST LEVEL THE ROW SHOWS THE FULL PATH, not the basename:
		// `go.mod` twice in one reply is not an address, and the caller's next call
		// needs one. The indentation stays, because the tree shape is the thing a
		// deeper listing is for.
		if depth := int(getFloatAny(e, "depth", 1)); depth > 1 {
			if p := getString(e, "path", ""); p != "" {
				name = p
			}
			name = strings.Repeat("  ", depth-1) + name
		}
		if t == "dir" {
			name += "/"
		}
		if link := getString(e, "link", ""); link != "" {
			name += " -> " + link
		}
		size := ""
		if t == "file" {
			size = fmt.Sprint(int64(getFloatAny(e, "size", 0)))
		}
		when := ""
		if ms := int64(getFloatAny(e, "mod_time", 0)); ms > 0 {
			when = clockTime(ms)
		}
		line := mark + "  " + padRight(name, 34) + padLeft(size, 9) + "  " + padRight(when, 16) + " " +
			getString(e, "mode", "")
		if why := getString(e, "error", ""); why != "" {
			line += "  ! " + why
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	if n := int(getFloatAny(doc, "truncated", 0)); n > 0 {
		b.WriteString("(" + fmt.Sprint(n) + " more not listed; `--limit N` raises the cap, up to 2000)\n")
	}
	if n := int(getFloatAny(doc, "directories_not_walked", 0)); n > 0 {
		b.WriteString("(" + fmt.Sprint(n) + " directories not walked; `--depth N` goes deeper, up to 5)\n")
	}
	if n := int(getFloatAny(doc, "failed", 0)); n > 0 {
		b.WriteString("(" + fmt.Sprint(n) + " entries could not be read; each says why above)\n")
	}
	return b.String()
}

// padRight and padLeft are column fitting for a table that is read, not parsed.
func padRight(s string, n int) string {
	if len(s) >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-len(s))
}

func padLeft(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat(" ", n-len(s)) + s
}

// clockTime formats a millisecond timestamp as a UTC stamp: the listing is a fact about
// a remote host whose timezone is not ours, so the one time format this project stores
// is rendered in UTC rather than in the reader's.
func clockTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}
