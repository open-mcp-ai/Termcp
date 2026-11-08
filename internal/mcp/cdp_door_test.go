package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// TestTransientTargetIsReadOffTheKey: `cdp@ABC` names a target, not a peer.
//
// THE `@` STAYS OUT OF THE ADDRESS because it is not a node — it is a one-shot
// session on a target, and a node that exists for one call must not be listed.
func TestTransientTargetIsReadOffTheKey(t *testing.T) {
	if got := cdpTransientTarget("@ABC123"); got != "ABC123" {
		t.Errorf("cdpTransientTarget = %q, want ABC123", got)
	}
	for _, in := range []string{"", "1", "@", "ABC123"} {
		if got := cdpTransientTarget(in); got != "" {
			t.Errorf("cdpTransientTarget(%q) = %q, want empty", in, got)
		}
	}
}

// TestPeerAddressIsTheTreeAddress pins the shape every reply hands back: the session
// it was opened through, the kind that names what hangs off it, and the index of a
// child — exactly `<session> <kind>[:<index>]`, the same grammar as a channel.
func TestPeerAddressIsTheTreeAddress(t *testing.T) {
	if got := peerAddress("a1f3", "cdp", "1"); got != "a1f3 cdp:1" {
		t.Errorf("peerAddress = %q, want a1f3 cdp:1", got)
	}
	h := &jrpcHandle{sessionID: "a1f3", kind: "cdp", index: "2"}
	if got := pageAddress(h, "7D87AB"); got != "a1f3 cdp:2@7D87AB" {
		t.Errorf("pageAddress = %q, want a1f3 cdp:2@7D87AB", got)
	}
	// TWO CONNECTIONS ON ONE SESSION ARE TWO ADDRESSES. That is the whole reason the
	// index exists, so it is worth pinning: the old key could only ever hold one.
	if peerKey("a1f3", "cdp", "1") == peerKey("a1f3", "cdp", "2") {
		t.Error("two connections on one session share an address")
	}
}

// A page-level method sent to the browser process must be a FAILING reply with the
// sentence that says so. Chrome answers `wasn't found`, which reads like a typo.
func TestPeerErrorTurnsAProtocolErrorIntoAFailure(t *testing.T) {
	h := &jrpcHandle{kind: "cdp", cdp: newCDPState()}
	stmt := directive.Statement{Op: "call", Target: "a1f3", Kind: "cdp"}
	slot, isError := peerError(stmt, h, `{"id":1,"error":{"code":-32601,"message":"'Page.enable' wasn't found"}}`)
	if !isError {
		t.Fatal("a protocol error was reported as success")
	}
	if slot.Code < 400 {
		t.Errorf("status = %d, want a 4xx", slot.Code)
	}
	if !strings.Contains(slot.Body, "PAGE-level") {
		t.Errorf("the reply does not name the real cause:\n%s", slot.Body)
	}
}

// A vanished sessionId has its own sentence: the tab navigated or closed, and the
// caller has to attach again rather than retry.
func TestPeerErrorNamesAVanishedSession(t *testing.T) {
	h := &jrpcHandle{kind: "cdp", cdp: newCDPState()}
	stmt := directive.Statement{Op: "call", Target: "a1f3", Kind: "cdp", Key: "2"}
	slot, isError := peerError(stmt, h, `{"id":1,"error":{"code":-32001,"message":"No session with given id"}}`)
	if !isError {
		t.Fatal("a vanished session was reported as success")
	}
	if !strings.Contains(slot.Body, "re-attach") {
		t.Errorf("the reply does not say how to recover:\n%s", slot.Body)
	}
}

// A normal answer is NOT rewritten into an error.
func TestPeerErrorLeavesAHealthyFrameAlone(t *testing.T) {
	h := &jrpcHandle{kind: "cdp", cdp: newCDPState()}
	if _, isError := peerError(directive.Statement{Target: "a1f3", Kind: "cdp"}, h, `{"id":1,"result":{}}`); isError {
		t.Fatal("a successful frame was reported as an error")
	}
}

// TestFrameBelongsToReadsTheSessionTag pins the rule that makes one socket carry
// several pages: the frame's own sessionId is what tells them apart, and a frame
// with no tag came from the browser process, so it belongs to no page.
func TestFrameBelongsToReadsTheSessionTag(t *testing.T) {
	const sid = "ABC123"
	cases := []struct {
		frame string
		want  bool
	}{
		{`{"sessionId":"ABC123","method":"Page.loadEventFired"}`, true},
		{`{"sessionId":"OTHER","method":"Page.loadEventFired"}`, false},
		{`{"method":"Target.getTargets"}`, false},
		{`{"result":{"targetInfos":[]}}`, false},
		{`not json`, false},
	}
	for _, tc := range cases {
		if got := frameBelongsTo(tc.frame, sid); got != tc.want {
			t.Errorf("frameBelongsTo(%s) = %v, want %v", tc.frame, got, tc.want)
		}
	}
}

// TestCdpOpenReplyTeachesTheFlagThatWorks pins the one instruction the open reply gives.
//
// It said `open <handle> target <targetId>` — no dashes — so a caller following the
// hint verbatim had `target` parsed as the URL and got a discovery error about "1".
// The hint was wrong in the same reply that explains why the next call needs a target,
// which is the worst place for it to be wrong.
func TestCdpOpenReplyTeachesTheFlagThatWorks(t *testing.T) {
	src := readPackageSource(t)

	// The attach step must be taught with its dashes; without them the parser reads the
	// word as the URL to open.
	if !strings.Contains(src, "--target <targetId>") {
		t.Errorf("no reply teaches `--target <targetId>`; the attach step is unlearnable from a reply")
	}
	// The bare spelling is what shipped. Match it only where it follows a space and a
	// backtick-less context, so `--target <targetId>` does not trip it.
	if strings.Contains(src, " target <targetId>") && !strings.Contains(src, "--target <targetId>") {
		t.Errorf("a reply teaches `target <targetId>` without the dashes, which the parser reads as a URL")
	}
	for _, line := range strings.Split(src, "\n") {
		idx := strings.Index(line, " target <targetId>")
		if idx > 0 && line[idx-1] != '-' && line[idx-2] != '-' {
			t.Errorf("a reply teaches the bare `target <targetId>`:\n%s", strings.TrimSpace(line))
		}
	}
}
