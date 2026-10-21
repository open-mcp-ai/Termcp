package mcp

import (
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/capability"
)

// This file is the human-readable half of review mode: it renders what a queued
// operation will DO, so the person deciding can decide.
//
// It lives apart from approval_gate.go on purpose. That file holds the admission
// decision and must not know any tool's name; this one is a translator between
// the tool surface's vocabulary and a sentence, so it necessarily names tools.
// Keeping the two apart is what makes the "no operation names in admission code"
// rule checkable at all (see internal/capability's source test).

// kindFor is the approval kind a call is queued under, read from the capability
// table rather than a list in this package.
//
// The empty kind means "this particular call is not reviewable", which is how a
// tool whose actions mix reads and writes keeps reads out of the queue: a
// dir listing and a truncate are one capability, and only the truncate is a
// decision. Two things depend on the split — a reviewer is not asked to click
// Accept on a listing (which is how a gate stops being read), and a session
// scoped to one kind lets the other through.
//
// The argument-dependent cases live here rather than in the capability table
// because they are properties of a CALL (which action this one invoked), not of
// the capability.
func kindFor(tool string, args map[string]any) approval.Kind {
	action := getString(args, "action", "")
	switch tool {
	case "forward":
		switch action {
		case "list":
			return ""
		case "close":
			return approval.KindForwardClose
		}
		return approval.KindForwardOpen
	case "file_fs":
		if action != "truncate" {
			return ""
		}
		return approval.KindFileTruncate
	case "file_link":
		if action == "readlink" {
			return ""
		}
		return approval.KindFileLink
	case "extension":
		switch action {
		case "create":
			return approval.KindExtensionCreate
		case "update":
			return approval.KindExtensionUpdate
		}
		return ""
	case "ssh_config":
		switch action {
		case "create", "copy":
			return approval.KindProfileCreate
		case "edit":
			return approval.KindProfileEdit
		}
		return ""
	}
	if c, ok := capability.ForTool(tool); ok {
		return c.Kind
	}
	return approval.Kind(tool)
}

// summarizeOperation renders one line a reviewer can decide on.
//
// This is the whole value of the feature: a reviewer who cannot tell what they
// are approving is not reviewing anything. It reads the same arguments the
// handler will use, so the summary cannot describe something different from
// what will run.
func summarizeOperation(tool string, args map[string]any) string {
	s := func(k string) string { return strings.TrimSpace(getString(args, k, "")) }
	n := func(k string, def float64) float64 { return getFloat64(args, k, def) }

	switch tool {
	case "file_write":
		path := s("remote_path")
		if lp := s("local_path"); lp != "" {
			return fmt.Sprintf("write to %s from local file %s", path, lp)
		}
		return fmt.Sprintf("write %s to %s", describeBytes(len(s("data")), s("mode")), path)

	case "file_delete":
		return "delete " + s("remote_path")

	case "file_rename":
		return fmt.Sprintf("move %s to %s", s("from_path"), s("to_path"))

	case "file_mkdir":
		return "create directory " + s("remote_path")

	case "file_perm":
		path := s("remote_path")
		switch s("action") {
		case "chmod":
			return fmt.Sprintf("chmod %04o on %s", int(n("mode", 0)), path)
		case "chown":
			return fmt.Sprintf("chown uid=%d gid=%d on %s", int(n("uid", 0)), int(n("gid", 0)), path)
		case "chtimes":
			return "set times on " + path
		default:
			return "change permissions on " + path
		}

	case "file_link":
		switch s("action") {
		case "readlink":
			return "read link " + s("remote_path") // read-only; kept for completeness
		case "symlink":
			return fmt.Sprintf("symlink %s -> %s", s("link_path"), s("target"))
		default:
			return fmt.Sprintf("hard link %s -> %s", s("new_path"), s("existing_path"))
		}

	case "file_fs":
		return fmt.Sprintf("truncate %s to %d bytes", s("remote_path"), int64(n("size", 0)))

	case "forward":
		switch s("action") {
		case "close":
			return "close port forward " + s("forward_id")
		case "remote":
			return fmt.Sprintf("open remote forward %s:%d -> %s:%d",
				s("local_host"), int(n("local_port", 0)), s("remote_host"), int(n("remote_port", 0)))
		case "dynamic":
			// The proxy is unauthenticated and loopback-only by construction (see
			// forward.CreateDynamic), so there is no address to report.
			return fmt.Sprintf("open SOCKS5 proxy on port %d (this host only)", int(n("local_port", 0)))
		default:
			// The listening address is part of what is being approved: with --bind-host
			// the same flags either stay on this machine or publish the tunnel to the
			// network, and a prompt that renders both as ":8080" hides the difference
			// from whoever has to decide.
			return fmt.Sprintf("open local forward %s:%d -> %s:%d%s",
				bindHostLabel(s("bind_host")), int(n("local_port", 0)),
				s("remote_host"), int(n("remote_port", 0)), exposedNote(s("bind_host")))
		}

	// Installing something is decided on its CONTENT, not its name: an approved
	// extension stays resident and is used from then on without asking again, so
	// "install an extension" alone would be approving a word. These summaries are
	// what the human reads to decide.
	case "extension":
		return summarizeExtension(s("action"), args)

	case "ssh_config":
		return summarizeProfile(s("action"), args)
	}
	return tool
}

// summarizeExtension renders an extension template for a reviewer: what it runs
// (or which endpoint it relays), what it may touch, and what it is for.
// bindHostLabel renders the address a local forward will listen on. An empty
// argument is not "unknown": it is the loopback default (forward.ResolveBindHost),
// and spelling it out shows the reviewer what saying nothing actually means.
func bindHostLabel(host string) string {
	if strings.TrimSpace(host) == "" {
		return "127.0.0.1"
	}
	return strings.TrimSpace(host)
}

// exposedNote says, in the same line, that a non-loopback bind is reachable from
// the network. The interface list is deliberately absent: whether 0.0.0.0 is
// actually exposed depends on the host's firewall, and a prompt that guesses
// "exposed to 192.168.1.0/24" would be more wrong than silence.
func exposedNote(host string) string {
	switch strings.TrimSpace(host) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return ""
	}
	return "  (reachable from the network, not just this machine)"
}

func summarizeExtension(action string, args map[string]any) string {
	s := func(k string) string { return strings.TrimSpace(getString(args, k, "")) }
	list := func(k string) string {
		items := getStringSlice(args, k)
		if len(items) == 0 {
			return ""
		}
		return strings.Join(items, ",")
	}

	name := s("name")
	if name == "" {
		name = "(unnamed)"
	}
	var head string
	switch action {
	case "update":
		head = "update extension " + name
	case "delete":
		return "delete extension " + name
	default:
		head = "install extension " + name + " (" + s("kind") + ")"
	}

	var parts []string
	if cmd := s("command"); cmd != "" {
		if argv := getStringSlice(args, "args"); len(argv) > 0 {
			cmd += " " + strings.Join(argv, " ")
		}
		parts = append(parts, "runs "+cmd)
	}
	if url := s("url"); url != "" {
		parts = append(parts, "relays "+url)
	}
	if target := s("target"); target != "" {
		parts = append(parts, "on "+target)
	}
	if allow := list("allow"); allow != "" {
		parts = append(parts, "allow "+allow)
	}
	if paths := list("paths"); paths != "" {
		parts = append(parts, "paths "+paths)
	}
	if fwd := summarizeForwards(args); fwd != "" {
		parts = append(parts, "forwards "+fwd)
	}
	if notes := s("notes"); notes != "" {
		parts = append(parts, "notes "+truncate(notes, 120))
	}
	if len(parts) == 0 {
		return head
	}
	return head + ": " + strings.Join(parts, "; ")
}

// summarizeForwards renders the tunnels a template declares, in the same
// direction vocabulary the tools use, so a reviewer can see an exposed port
// without reading the TOML.
func summarizeForwards(args map[string]any) string {
	raw, ok := args["forwards"].([]any)
	if !ok || len(raw) == 0 {
		return ""
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.TrimSpace(getString(m, "kind", "local"))
		local := int(getFloat64(m, "local_port", 0))
		remoteHost := strings.TrimSpace(getString(m, "remote_host", "localhost"))
		remote := int(getFloat64(m, "remote_port", 0))
		switch kind {
		case "dynamic":
			out = append(out, fmt.Sprintf("SOCKS5 :%d", local))
		case "remote":
			out = append(out, fmt.Sprintf("remote :%d -> %s:%d", local, remoteHost, remote))
		default:
			out = append(out, fmt.Sprintf("local :%d -> %s:%d", local, remoteHost, remote))
		}
	}
	return strings.Join(out, ", ")
}

// summarizeProfile renders a profile change for a reviewer. Credentials are
// never part of the summary: the reviewer needs to know WHICH host the agent may
// reach, not the secret used to reach it.
func summarizeProfile(action string, args map[string]any) string {
	s := func(k string) string { return strings.TrimSpace(getString(args, k, "")) }
	name := s("name")
	if name == "" {
		name = "(unnamed)"
	}
	host := s("host")
	user := s("user")
	target := host
	if user != "" && host != "" {
		target = user + "@" + host
	}
	switch action {
	case "create":
		if target == "" {
			return "create profile " + name
		}
		return "create profile " + name + " for " + target
	case "edit":
		if target == "" {
			return "edit profile " + name
		}
		return "edit profile " + name + " -> " + target
	case "copy":
		return "copy profile " + name
	case "delete":
		return "delete profile " + name
	}
	return action + " profile " + name
}

// describeBytes says how much a write carries, in the unit a human reads.
// The byte count is what the reviewer actually needs to judge scale; the mode
// is stated because hex and text mean different things for the same length.
func describeBytes(dataLen int, mode string) string {
	unit := "bytes"
	switch {
	case dataLen >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(dataLen)/(1<<20))
	case dataLen >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(dataLen)/(1<<10))
	}
	if mode != "" && mode != "text" {
		unit = mode + " bytes"
	}
	return fmt.Sprintf("%d %s", dataLen, unit)
}
