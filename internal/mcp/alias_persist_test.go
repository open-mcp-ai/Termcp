package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// AN ALIAS PERSISTS WHEN CREATED WITH `create alias`, AND DIES WHEN REMOVED.
//
// `create alias` writes `<dir>/aliases.json` through aliasStore, and a server
// started on the same data directory reads it back at boot.
func TestCreateAliasPersistsAcrossServerRestarts(t *testing.T) {
	dir := t.TempDir()

	// First server instance: create a saved alias.
	s1 := newTestServer(t)
	s1.SetDataDir(dir)
	defer termText(t, s1, map[string]any{"cmd": "rm alias myrun"})

	res := termText(t, s1, map[string]any{"cmd": "create alias myrun ls host"})
	if !strings.HasPrefix(res, "TERM/1 204") {
		t.Fatalf("create alias failed:\n%s", res)
	}

	// ls alias shows it under the SAVED ALIASES header.
	listing := termText(t, s1, map[string]any{"cmd": "ls alias"})
	if !strings.Contains(listing, "SAVED ALIASES (saved to disk):") || !strings.Contains(listing, "myrun = ls host") {
		t.Fatalf("saved alias missing from listing:\n%s", listing)
	}

	// Second server instance on the SAME data directory simulates a restart.
	s2 := newTestServer(t)
	s2.SetDataDir(dir)

	// The saved alias expanded on the fresh server.
	out := termText(t, s2, map[string]any{"cmd": "myrun"})
	if !strings.Contains(out, "Path: ls host") {
		t.Fatalf("saved alias did not expand on fresh server:\n%s", out)
	}

	// Removing the saved alias removes it from disk.
	drop := termText(t, s2, map[string]any{"cmd": "rm alias myrun"})
	if !strings.HasPrefix(drop, "TERM/1 204") {
		t.Fatalf("rm alias failed:\n%s", drop)
	}
	afterDrop := termText(t, s2, map[string]any{"cmd": "ls alias"})
	if strings.Contains(afterDrop, "myrun") {
		t.Fatalf("dropped alias still in listing:\n%s", afterDrop)
	}

	// And the dropped alias no longer expands.
	gone := termText(t, s2, map[string]any{"cmd": "myrun"})
	if !strings.Contains(gone, "unknown directive: myrun") {
		t.Fatalf("dropped alias still expanded:\n%s", gone)
	}
}

// AN EXTENSION MAY DECLARE AN `[alias]` TABLE, ADDRESSED AS `<ext>.<alias>`.
func TestExtensionNamespaceAliasExpandsAndResolvesPositionalArgs(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t)
	s.SetDataDir(dir)

	rec := extensions.NewReconciler(extensions.NewStore(dir), nil, nil, nil, nil)
	s.SetExtensionReconciler(rec)

	// Write an extension template with an [alias] table.
	ext := &extensions.Extension{
		Name:    "jira",
		Target:  "internal",
		Command: "echo",
		Args:    []string{"stub"},
		Alias: map[string]string{
			"search": "ls host",
		},
	}
	if err := rec.Store().Save(ext); err != nil {
		t.Fatalf("save ext: %v", err)
	}

	// ls alias lists the extension alias under its own heading.
	listing := termText(t, s, map[string]any{"cmd": "ls alias"})
	if !strings.Contains(listing, "EXTENSION ALIASES (<ext>.<alias>):") ||
		!strings.Contains(listing, "jira.search = ls host") {
		t.Fatalf("extension alias missing from listing:\n%s", listing)
	}

	// Invoking jira.search expands to ls host.
	out := termText(t, s, map[string]any{"cmd": "jira.search"})
	if !strings.Contains(out, "Path: ls host") {
		t.Fatalf("jira.search did not expand:\n%s", out)
	}
}

// AN ALIAS MAY NOT REDEFINE A DIRECTIVE.
func TestAliasCannotShadowABuiltinDirective(t *testing.T) {
	s := newTestServer(t)
	res := termText(t, s, map[string]any{"cmd": "create alias read ls host"})
	if !strings.HasPrefix(res, "TERM/1 400") {
		t.Fatalf("shadowing `read` should be refused with 400:\n%s", res)
	}
	if !strings.Contains(res, "already a directive name") {
		t.Fatalf("expected refusal reason:\n%s", res)
	}
}

// DOT NAMES ARE RESERVED FOR EXTENSION NAMESPACES.
func TestUserAliasCannotContainDot(t *testing.T) {
	s := newTestServer(t)
	res := termText(t, s, map[string]any{"cmd": "create alias my.cmd ls host"})
	if !strings.HasPrefix(res, "TERM/1 400") {
		t.Fatalf("user alias with dot should be refused with 400:\n%s", res)
	}
	if !strings.Contains(res, "dot names are reserved") {
		t.Fatalf("expected refusal reason:\n%s", res)
	}
}
