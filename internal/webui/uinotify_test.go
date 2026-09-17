package webui

import (
	"reflect"
	"testing"
	"time"
)

func TestUINotifyHub_BroadcastDeliversToRegisteredChannels(t *testing.T) {
	h := newUINotifyHub()
	ch1 := make(chan []byte, 4)
	ch2 := make(chan []byte, 4)
	unreg1 := h.register(ch1)
	unreg2 := h.register(ch2)
	defer unreg1()
	defer unreg2()

	got := h.broadcast(map[string]any{"type": "ui_notify", "n": 1})
	if got != 2 {
		t.Fatalf("delivered = %d, want 2", got)
	}
	for i, ch := range []chan []byte{ch1, ch2} {
		select {
		case b := <-ch:
			if string(b) == "" {
				t.Fatalf("channel %d got empty payload", i)
			}
		default:
			t.Fatalf("channel %d received nothing", i)
		}
	}
}

func TestUINotifyHub_BroadcastWithoutClientsReturnsZero(t *testing.T) {
	h := newUINotifyHub()
	if got := h.broadcast(map[string]any{"type": "ui_notify"}); got != 0 {
		t.Fatalf("delivered = %d, want 0", got)
	}
}

func TestUINotifyHub_UnregisteredChannelIsSkipped(t *testing.T) {
	h := newUINotifyHub()
	ch := make(chan []byte, 1)
	unreg := h.register(ch)
	unreg()
	if got := h.broadcast(map[string]any{"type": "ui_notify"}); got != 0 {
		t.Fatalf("delivered = %d, want 0 after unregister", got)
	}
}

func TestUINotifyHub_SlowClientDoesNotBlockOthers(t *testing.T) {
	h := newUINotifyHub()
	slow := make(chan []byte, 1) // will fill up and be skipped
	fast := make(chan []byte, 4)
	defer h.register(slow)()
	defer h.register(fast)()

	// Fill the slow channel so the broadcast's default branch is taken.
	slow <- []byte("stale")

	done := make(chan struct{})
	go func() {
		if got := h.broadcast(map[string]any{"type": "ui_notify"}); got != 1 {
			t.Errorf("delivered = %d, want 1", got)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full channel")
	}
}

func TestBroadcastUINotify_BareHandlerIsSafe(t *testing.T) {
	var h Handler // zero value: no hub, no sessions
	if got := h.BroadcastUINotify("", "t", "m", "", 5); got != 0 {
		t.Fatalf("delivered = %d, want 0", got)
	}
}

func TestBroadcastUINotify_PayloadShape(t *testing.T) {
	hub := newUINotifyHub()
	h := &Handler{notifyHub: hub}
	ch := make(chan []byte, 1)
	defer hub.register(ch)()

	if got := h.BroadcastUINotify("", "hello", "world", "sess-1", 7); got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}

	select {
	case b := <-ch:
		want := []byte(`{"duration_seconds":7,"level":"info","message":"world","session_id":"sess-1","title":"hello","type":"ui_notify"}`)
		if !reflect.DeepEqual(b, want) {
			t.Fatalf("payload = %s, want %s", b, want)
		}
	default:
		t.Fatal("received nothing")
	}
}
