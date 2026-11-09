package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestPeersGoWithTheirSession pins the cascade.
//
// A peer's transport IS the session's SSH channel. When the session dies the peer is a
// socket to nowhere, but its handle stayed addressable and kept its frame ring — up to
// 2048 frames or 8 MiB each — for the life of the process. Forwards already cascaded
// (forwardMgr.CloseBySession); peers did not, so `ls cdp` hid the dead peer while
// `read <handle>` still answered from it.
func TestPeersGoWithTheirSession(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]

	// A CDP door against a URL that cannot resolve still mints a handle only on
	// success, so drive the registry directly: what is under test is the cascade, not
	// discovery.
	jrpcChannels.Store(peerKey(sid, "cdp", "1"), &jrpcHandle{kind: "cdp", index: "1", sessionID: sid})
	jrpcChannels.Store(peerKey("other-session", "cdp", "1"), &jrpcHandle{kind: "cdp", index: "1", sessionID: "other-session"})

	closePeersBySession(sid)

	_, mine := jrpcChannels.Load(peerKey(sid, "cdp", "1"))
	_, theirs := jrpcChannels.Load(peerKey("other-session", "cdp", "1"))
	if mine {
		t.Errorf("the peer of the dead session is still registered")
	}
	if !theirs {
		t.Errorf("a peer of ANOTHER session was closed by the cascade")
	}

	// Drive the same path through the session manager, so the hook is proven wired and
	// not merely available.
	jrpcChannels.Store(peerKey(sid, "mcp", "2"), &jrpcHandle{kind: "mcp", index: "2", sessionID: sid})
	// The SESSION, not the channel: a peer hangs off the session's transport, so
	// closing one channel must leave it alone (and does — that is a different test).
	termText(t, s, map[string]any{"cmd": "close " + sid})
	waitForPeerGone(t, sid, "mcp", "2")

	jrpcChannels.Delete(peerKey("other-session", "cdp", "1"))
}

func waitForPeerGone(t *testing.T, sid, kind, index string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, ok := jrpcChannels.Load(peerKey(sid, kind, index))
		if !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer %s %s:%s survived its session", sid, kind, index)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
