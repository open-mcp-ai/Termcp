package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestExtAnswerReachesTheClientByteForByte pins issue #234's central promise: the
// client that connects to /ext/{name}/stream talks to the EXTENSION, so what it
// reads is the peer's own frame — its serverInfo, its tools, its answers.
//
// IT USED TO BE TESTED THROUGH extMCPWriteSlot, the hand-written writer that
// converted a directive slot into an HTTP response. That writer is gone: the
// transport writes the answer now, and what the endpoint owes is that the BYTES it
// hands the transport are the peer's. That is what this pins — every member the
// reply carried, in the order the peer wrote them, with only the id restored.
func TestExtAnswerReachesTheClientByteForByte(t *testing.T) {
	ep, ch := newTestEndpoint(t)
	sess := newExtTestSession("client")
	ep.RegisterSession(context.Background(), sess)
	defer ep.UnregisterSession(context.Background(), sess)
	if _, err := ep.ensureChannel(context.Background()); err != nil {
		t.Fatalf("ensureChannel: %v", err)
	}

	answerCh := make(chan string, 1)
	go func() {
		answerCh <- string(ep.Handle(context.Background(),
			[]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"calc"}}`), sess))
	}()

	var upstream string
	deadline := time.Now().Add(2 * time.Second)
	for upstream == "" && time.Now().Before(deadline) {
		if frames := ch.take(); len(frames) > 0 {
			upstream = frames[0]
		} else {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if upstream == "" {
		t.Fatal("the request never reached the peer")
	}

	// A reply with members this project does not model, in an order it would not
	// choose, and a nested object that holds a comma — re-encoding would change all
	// three.
	peerReply := `{"jsonrpc":"2.0","result":{"zeta":1,"alpha":[2,3],"content":[{"type":"text","text":"ok"}]},"id":` + frameID(upstream) + `}`
	ch.inbound <- []byte(peerReply + "\n")

	select {
	case got := <-answerCh:
		want := strings.Replace(peerReply, `"id":`+frameID(upstream), `"id":5`, 1)
		if got != want {
			t.Fatalf("the reply was changed on the way to the client.\n want: %s\n  got: %s", want, got)
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(got), &probe); err != nil {
			t.Fatalf("the answer is not JSON: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the client never got its answer")
	}
}

// TestExtNonFrameAnswerIsNeverSentAsOne: ordinary output from the peer (a crash
// message, a banner) is not a JSON-RPC message, and handing it to an MCP client
// would surface later as a mysterious protocol error instead of "the extension did
// not answer with a frame". The pump skips it, so the request times out with a
// sentence — which is the honest outcome.
func TestExtNonFrameAnswerIsNeverSentAsOne(t *testing.T) {
	ep, ch := newTestEndpoint(t)
	sess := newExtTestSession("client")
	ep.RegisterSession(context.Background(), sess)
	defer ep.UnregisterSession(context.Background(), sess)
	if _, err := ep.ensureChannel(context.Background()); err != nil {
		t.Fatalf("ensureChannel: %v", err)
	}

	ch.inbound <- []byte("python: command not found\n")

	answer := ep.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":6,"method":"tools/call"}`), sess)
	if !strings.Contains(string(answer), `"error"`) {
		t.Fatalf("a peer that printed a crash message must become an error, got: %s", answer)
	}
	for _, s := range sess.frames() {
		if strings.Contains(s, "command not found") {
			t.Fatalf("the peer's stdout reached the client as a frame: %s", s)
		}
	}
}
