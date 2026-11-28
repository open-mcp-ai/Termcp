package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE STREAM MUST END WHEN THE EXTENSION DOES.
//
// A subscriber's GET stream is not a socket to the peer: it is a stream from this
// instance, so the peer exiting, the channel being closed, or the idle sweep reaping the
// extension reached the pump and stopped there. The stream stayed open, the keep-alive
// kept it warm, and nothing ever arrived again — a quiet subscription and a dead one were
// the same bytes. This pins the observable half of the fix: the stream ends, and the
// session it named is gone.
const dyingExtPeerStub = `
# ONE ANSWER, THEN GONE. The exit is triggered by the client's notifications/initialized
# rather than by a sleep, so the death happens at a moment the test chose: after the GET
# stream is attached, which is the ordering the assertion depends on.
while IFS= read -r line; do
  printf '%s\n' "$line" >> __PEERLOG__
  case "$line" in
    *notifications/initialized*)
      echo "PEER-EXITING" >> __PEERLOG__
      exit 0 ;;
    *initialize*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([^,}]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"resources":{"subscribe":true}},"serverInfo":{"name":"dying","version":"1"}}}\n' "$id" ;;
  esac
done
`

func TestAttachedExtStreamEndsWhenTheExtensionChannelDies(t *testing.T) {
	s := newExtensionTestServer(t)
	logf := filepath.ToSlash(filepath.Join(t.TempDir(), "peer.log"))
	script := writeScript(t, strings.ReplaceAll(dyingExtPeerStub, "__PEERLOG__", logf))
	t.Cleanup(func() {
		b, _ := os.ReadFile(logf)
		t.Logf("PEER SAW: %s", b)
	})
	callExtension(t, s, map[string]any{
		"action": "create", "name": "dying", "target": "internal",
		"command": "sh", "args": []any{script}, "enabled": true,
	})
	useExtension(t, s, "dying")

	mux := http.NewServeMux()
	mux.Handle("/ext/{name}/stream", s.ExtMCPHandler())
	ts := httptest.NewServer(mux)
	defer ts.Close()
	url := ts.URL + "/ext/dying/stream"

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"plugin","version":"1"}}}`
	resp, err := http.Post(url, "application/json", strings.NewReader(init))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize through the extension endpoint answered %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("the extension endpoint handed out no session id")
	}

	// THE STREAM, THEN THE KILL: Do returning means the handler got past openStream, so
	// the subscription the real client would wait on is in place before anything ends.
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Mcp-Session-Id", sid)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("GET on the extension endpoint answered %d, want the stream", stream.StatusCode)
	}

	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, stream.Body)
		close(ended)
	}()

	if code := extPost(t, url, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, sid); code != http.StatusAccepted {
		t.Fatalf("notifications/initialized answered %d, want 202", code)
	}

	// THE DEADLINE IS A HANG DETECTOR, not a budget: the stream ends on its own, and this
	// only bounds how long a broken build takes to say so.
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the GET stream is still open after the extension's channel ended")
	}

	if code := extPost(t, url, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sid); code != http.StatusNotFound {
		t.Errorf("a request naming the ended session answered %d, want 404 (the client must initialize again)", code)
	}
}

// extPost posts one frame to an extension endpoint and reports the status.
func extPost(t *testing.T, url, body, sessionID string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
