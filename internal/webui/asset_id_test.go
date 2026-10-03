package webui

import (
	"strings"
	"testing"
)

// TestShellTabCopyUsesServerAssignedIndex guards the browser half of issue #73.
// The channel index belongs to the server: it is returned in /shells metadata and
// copied into the tab's channel state. A client-side creation counter must never
// be used as the locator's source, because closing an earlier channel makes that
// counter disagree with the MCP/REST resolver.
func TestShellTabCopyUsesServerAssignedIndex(t *testing.T) {
	js := readAssetLF(t, "static/js/terminal-view.js")
	sessions := readAssetLF(t, "static/js/sessions.js")

	create := between(t, js, "function createChannelTab(", "\n}\n")
	for _, want := range []string{
		"var urlIndex = Number(opt.index)",
		"'shell-' + urlIndex",
		"resourceUrlShell(win._parentSid || win._sid || '', urlIndex)",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("createChannelTab no longer uses %q as the channel locator source:\n%s", want, create)
		}
	}
	if strings.Contains(create, "opt.label") || strings.Contains(create, "optLabel") {
		t.Error("createChannelTab accepts a client-provided label; the tab number must be the server-assigned index")
	}
	if !strings.Contains(create, "Number.isInteger(urlIndex)") {
		t.Error("createChannelTab lacks the guard that detects a missing server index")
	}
	if !strings.Contains(create, "tabCopyBtn.disabled = true") {
		t.Error("a tab without a server index must disable its copy button rather than emit a guessed locator")
	}
	if strings.Contains(js, "_channelSeq") {
		t.Error("terminal-view.js still keeps a client-side channel sequence; the index must come from the server")
	}

	// Every server-list reconciliation path must pass s.index through to the tab
	// creator. Otherwise a tab opened after a session refresh would silently fall
	// back to the old local sequence.
	for _, want := range []string{
		"createChannelTab(win, sid, { index: s.index || 0 });",
		"createChannelTab(win, sid, { index: s.index || 0, readOnlyHistory: true });",
	} {
		if !strings.Contains(sessions, want) {
			t.Errorf("sessions.js does not pass server index through: %q", want)
		}
	}

	// The new-channel response carries its index too, and the session-start path
	// seeds the primary tab from the index the server returned with the ids. No
	// normal tab path is allowed to invent a number.
	if !strings.Contains(js, "j.shell_id || j.session_id, { index: j.index || 0 }") {
		t.Error("new shell response does not pass its server index to createChannelTab")
	}
	if !strings.Contains(js, "{ index: opt.index > 0 ? opt.index : 1 }") {
		t.Error("primary shell tab does not take the server-supplied index from the session-start response")
	}
	if !strings.Contains(js, "{ index: j.index || 0 }") {
		t.Error("session-start response index is not forwarded to the primary tab")
	}

}
