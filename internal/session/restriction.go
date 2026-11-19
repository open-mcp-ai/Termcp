package session

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/capability"
)

// Restriction trims what may act on a session, across every surface that can
// act on one: the MCP tools, the REST endpoints, and the WebSocket the browser
// terminal speaks. It is the mechanism behind extensions: an extension is an
// ordinary session whose capabilities were narrowed at creation time, so a
// handle handed to an agent can only do what the extension's author decided —
// and nothing the author did not list is silently reachable.
//
// The allow list holds CAPABILITY ids ("shell.input", "file.read"), not tool
// names. A tool name is one surface's spelling of an operation; the capability
// is the operation, so a template that grants "file.read" grants it on the MCP
// tool, the REST route and the WebSocket alike without the author naming three
// things. internal/capability owns the name → id mapping in both directions.
//
// Enforcing it on one transport only would make the whitelist a matter of which
// door the caller picked, so each surface asks these same questions rather than
// restating the rule. internal/mcp's guard and internal/webui's REST and
// WebSocket refusals are thin wrappers over Allows/AllowsPath.
//
// Both halves are whitelists and the zero value means unrestricted: an ordinary
// session carries a Restriction{} and every operation behaves exactly as before.
// Building one through NewRestriction is the only supported route, because path
// patterns must be validated once, at extension-load time, rather than failing
// mid-conversation on a pattern nobody has looked at.
type Restriction struct {
	allow []string
	paths []pathRule
}

// NewRestriction copies the capability whitelist and compiles every path pattern
// so a malformed one is reported when an extension is saved, not when an agent
// first reaches for a file. Blank patterns are dropped rather than counted as
// rules.
//
// An entry that is neither a capability id nor a name the capability table can
// resolve is kept as written and matches only that exact string. The reason is
// compatibility, not leniency: extensions saved before the switch hold tool
// names, and a manifest on disk outlives the process that wrote it. Allows
// translates a tool name onto its capability id, so those templates keep
// granting exactly what they always granted.
func NewRestriction(allow []string, paths []string) (Restriction, error) {
	r := Restriction{allow: append([]string(nil), allow...)}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		rule, err := compilePathRule(p)
		if err != nil {
			return Restriction{}, err
		}
		r.paths = append(r.paths, rule)
	}
	return r, nil
}

// Allows reports whether a capability may run against this session. An empty
// allow list is the unrestricted default; any other list is an exact-id
// whitelist, so a restricted session is a strict subset of an unrestricted one.
//
// id is normally a capability id. A tool name is accepted as well and translated
// first, and an entry that holds a tool name (a template written before the
// switch, which is what is on disk on an upgraded instance) matches the
// capability that name belongs to. Both spellings resolve to the same operation,
// so a template grants exactly what it always granted — see NewRestriction.
func (r Restriction) Allows(id string) bool {
	if len(r.allow) == 0 {
		return true
	}
	canonical, toolName := id, id
	if mapped, ok := capability.IDForTool(id); ok {
		canonical, toolName = mapped, id
	} else {
		toolName = capability.ToolForID(id)
	}
	for _, a := range r.allow {
		if a == canonical || a == toolName {
			return true
		}
	}
	return false
}

// AllowsTool reports whether the capability behind an MCP tool name may run
// against this session. It is Allows with the name resolved first, kept as a
// separate call because the surfaces hold different things: internal/mcp has a
// tool name in hand, internal/webui has a route shape.
func (r Restriction) AllowsTool(toolName string) bool {
	if id, ok := capability.IDForTool(toolName); ok {
		return r.Allows(id)
	}
	// An unknown tool has no capability, so a whitelist cannot name it; only an
	// unrestricted session lets it through.
	return len(r.allow) == 0
}

// AllowsPath reports whether a remote path is inside the whitelist. An empty rule
// set is the unrestricted default; otherwise the first matching rule grants it
// and no match denies it. Rules are alternatives (OR), not a conjunction: pair a
// tree entry with a glob by listing both.
func (r Restriction) AllowsPath(p string) bool {
	if len(r.paths) == 0 {
		return true
	}
	clean := path.Clean(p)
	for _, rule := range r.paths {
		switch rule.kind {
		case ruleTree:
			// A component-boundary prefix, not a string one: granting /srv/data must
			// never let an agent reach /srv/database by name adjacency.
			if clean == rule.literal || strings.HasPrefix(clean, rule.literal+"/") {
				return true
			}
		case ruleGlob:
			if ok, _ := path.Match(rule.glob, clean); ok {
				return true
			}
		case ruleRegexp:
			if rule.re.MatchString(clean) {
				return true
			}
		}
	}
	return false
}

// ForbiddenTool renders the refusal for a tool a session's whitelist does not
// carry. Both surfaces that can act on a session — the MCP guard and the REST
// handlers — answer with this text, so a caller who meets the whitelist on
// either one reads the same sentence and is pointed at the same remedy.
//
// The wording names BOTH surfaces' spelling, because a refusal is read on whichever
// one produced it. It used to name only `extension(action=get)`, which is the MCP
// tool: a caller driving the DSL read that advice, ran it, and got "unknown
// directive" — a second error from following the first one's instructions (issue
// #172). The DSL's spelling comes first because the DSL is the surface this project
// points a caller at, and the tool name is what an MCP client has instead.
func ForbiddenTool(toolName string) string {
	msg := "tool " + toolName + " is not permitted on this session: it is an extension session whose capability list does not include it. " +
		"Use `get ext <name>` (`extension` with `action=get` on the MCP tool surface) to see what this session was created to allow."
	if toolName == "shell_archive" || toolName == "session_terminate" {
		return msg + " To release one on-demand instance, use `close ext <name> <shell_id>` with its shell_id. " +
			"For a resident instance, ask the extension owner to manage its lifecycle."
	}
	return msg + " If this operation belongs in the template, ask the extension owner to review its capability list; retrying this call will not change it."
}

// ForbiddenPath renders the refusal for a path argument outside the whitelist.
// arg is the argument that carried the path, so the caller learns which of a
// file tool's several paths was the offending one.
func ForbiddenPath(arg, path string) string {
	return "path argument " + arg + " is outside the directories this extension session may reach: " + path +
		". Use `get ext <name>` (`extension` with `action=get` on the MCP tool surface) to see the allowed paths."
}

// ruleKind selects the matcher a pathPattern compiled into.
type ruleKind int

const (
	ruleTree ruleKind = iota
	ruleGlob
	ruleRegexp
)

// pathRule is one entry of the path whitelist. Exactly one field is meaningful,
// per kind.
type pathRule struct {
	kind    ruleKind
	literal string         // ruleTree: the cleaned directory whose subtree is granted
	glob    string         // ruleGlob: a slash-based pattern for path.Match
	re      *regexp.Regexp // ruleRegexp: the compiled expression
}

// compilePathRule picks a matcher from the pattern's own shape:
//
//	~/skills/foo/   no magic characters → a directory subtree
//	*.md            * ? [ present       → a slash-based glob
//	re:^/srv/.*\.md$ re: prefix         → a regular expression
//
// Globs and trees are slash-based — the form SFTP reports a remote POSIX path
// in — regardless of the termcp host's own OS, and path.Match has no **, so a
// recursive match is expressed as a tree entry rather than a glob. The regex
// branch is safe to hand to an agent that writes its own patterns: Go's regexp
// is RE2, linear-time with no catastrophic backtracking, so a pattern cannot
// burn CPU here the way a backtracking engine would let it.
func compilePathRule(p string) (pathRule, error) {
	if rest, ok := strings.CutPrefix(p, "re:"); ok {
		re, err := regexp.Compile(rest)
		if err != nil {
			return pathRule{}, fmt.Errorf("invalid regex %q: %w", p, err)
		}
		return pathRule{kind: ruleRegexp, re: re}, nil
	}
	if strings.ContainsAny(p, "*?[") {
		if _, err := path.Match(p, "probe"); err != nil {
			return pathRule{}, fmt.Errorf("invalid glob %q: %w", p, err)
		}
		return pathRule{kind: ruleGlob, glob: p}, nil
	}
	return pathRule{kind: ruleTree, literal: path.Clean(p)}, nil
}
