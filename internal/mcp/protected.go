package mcp

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/keys"
)

// PROTECTED PATHS ARE REFUSED BEFORE ANY CONFIGURATION IS CONSULTED.
//
// The access directory holds the root key that local callers sign tokens with.
// The permission model in docs/design/access-policy.md caps what such a token can
// do — a locally signed token always answers with the built-in MCP template, no
// matter what its claims say (§1.1) — so reading the key grants nothing a
// same-user process did not already have. That argument is about LOCAL callers.
//
// It does not cover an agent that reaches the daemon's filesystem through a
// session, and that is the case this file exists for. An agent with `file.read`
// and a path whitelist it authored itself can name `~/.termcp/access/root.key`;
// the restriction check would then be asking that same agent whether it may read
// the thing it just named. So the answer cannot come from configuration at all:
// it is a fixed refusal that no whitelist, no capability list and no extension
// can turn off.
//
// The two remaining ways in are not fixed here and cannot be:
//
//   - a shell on this host (`run` in an `internal` session) can `cat` the key;
//     the answer is not granting that session, which is what --no-internal is for;
//   - the OS user who owns the file can read it directly, which is the trust
//     boundary the whole two-key split rests on.
//
// Both are named in the design doc. What this file adds is that the most obvious
// route — ask termcp to read the path for you — does not work.

// protectedPaths lists, for one call, every path argument it carries. The list
// comes from the capability row so a new file operation declares its own paths
// once, where the rest of the operation is already described.
func protectedPathArgs(c gate, args map[string]any) []string {
	var out []string
	for _, key := range c.PathArgs {
		if v := strings.TrimSpace(getString(args, key, "")); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// refuseProtectedPath returns a refusal when any of the call's path arguments
// lands inside the access directory.
//
// IT APPLIES TO remote_path AS WELL AS local_path, deliberately. A remote path is
// on the far side of a session, and the far side is usually a different machine —
// but it is the SAME machine for the `internal` profile, and this guard runs
// before the target is resolved so it cannot know which. Refusing a remote host's
// own ~/.termcp/access is a false positive that costs an agent one path and
// closes the hole that matters, which is the trade this file chooses. A rule that
// depended on resolving the session first would leave the ordering up to whoever
// edits the resolver next.
func (g toolGuard) refuseProtectedPath(c gate, args map[string]any) *mcpgo.CallToolResult {
	if g.server == nil {
		return nil
	}
	dir := g.server.accessDir()
	if dir == "" {
		return nil
	}
	for _, p := range protectedPathArgs(c, args) {
		if pathTouches(p, dir) {
			return toolError(CodeForbidden, "%s",
				"that path is inside termcp's own access directory ("+dir+"), which holds the key local callers sign tokens with. "+
					"It is refused for every caller and cannot be granted: a credential that can be read by the processes it authorizes is not a credential. "+
					"This is not a whitelist setting.")
		}
	}
	return nil
}

// pathTouches reports whether a caller-supplied path names the protected
// directory or anything under it.
//
// EVERY SPELLING HAS TO BE CHECKED, not just the literal one, because the check
// runs before the path reaches the resolver that would have normalized it:
//
//	~/.termcp/access/root.key        the ordinary case
//	/root/.termcp/access/../access   a dot-segment that cleans back into place
//	/root/.termcp/access             the directory itself
//
// The cleaning is `path.Clean` (POSIX spelling) because that is what the file
// verbs carry on the wire on every platform — a Windows path arrives as /C:/…,
// see help write.
func pathTouches(given, protectedDir string) bool {
	if given == "" || protectedDir == "" {
		return false
	}
	// Compare against the directory in both spellings the wire uses: the POSIX
	// form the file verbs carry, and the native form this process resolved.
	guards := []string{path.Clean(protectedDir)}
	if native, err := filepath.Abs(protectedDir); err == nil {
		guards = append(guards, path.Clean(filepath.ToSlash(native)))
	}

	candidate := strings.TrimSpace(given)
	// A leading ~ is expanded lazily by the resolver, so expand it here too: the
	// literal "~/.termcp/..." would otherwise not match an absolute guard.
	if strings.HasPrefix(candidate, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			candidate = filepath.ToSlash(home) + candidate[1:]
		}
	}
	candidate = path.Clean(filepath.ToSlash(candidate))

	for _, guard := range guards {
		if candidate == guard || strings.HasPrefix(candidate, guard+"/") {
			return true
		}
	}
	return false
}

// accessDir is where the keys package keeps root.key, derived from the data
// directory this server was built with.
func (s *Server) accessDir() string {
	if s == nil || s.sessMgr == nil {
		return ""
	}
	dir := s.dataDir
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "access")
}

// keysAddr is the address the keys package resolves the root key from.
func (s *Server) keysAddr() keys.Addr {
	return keys.Addr{Dir: s.dataDir}
}
