package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/task"
)

func TestDetachedTransferStopsAfterGroupRevocation(t *testing.T) {
	h := newV2Harness(t, v2Policy("file.write"))
	var principalList struct {
		Principals []struct {
			ID      string `json:"id"`
			GroupID string `json:"group_id"`
		} `json:"principals"`
	}
	h.adminRequest("GET", "/auth/principals", nil, &principalList)
	var groupID string
	for _, p := range principalList.Principals {
		if p.ID == h.agentID {
			groupID = p.GroupID
		}
	}
	if groupID == "" {
		t.Fatal("missing explicit compatibility group")
	}
	var transfer *task.Task
	ownerFinished := make(chan struct{})
	endpoint := h.account.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		created, ctx, release, err := h.server.startTransfer(r.Context(), task.Upload, "display-session", "display-path", "display-local")
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		transfer = created
		go func() {
			defer close(ownerFinished)
			defer release()
			<-ctx.Done()
			h.server.finishTransfer(ctx, created.ID(), ctx.Err())
		}()
		w.WriteHeader(204)
	}))
	req := httptest.NewRequest("POST", h.url+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	response := httptest.NewRecorder()
	endpoint.ServeHTTP(response, req)
	if response.Code != 204 || transfer == nil {
		t.Fatal("request did not create a transfer")
	}
	view, err := h.server.tasks.Get(transfer.ID())
	if err != nil || view.Status != task.Running {
		t.Fatal("response completion stopped the task", err)
	}
	h.adminRequest("PATCH", "/auth/groups/"+groupID, map[string]any{"revision": 1, "enabled": false}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	view, err = h.server.tasks.Await(ctx, transfer.ID(), 5*time.Second)
	if err != nil || view.Status != task.Cancelled {
		t.Fatal("revocation did not preserve a cancelled receipt", view.Status, err)
	}
	select {
	case <-ownerFinished:
	case <-ctx.Done():
		t.Fatal("transfer owner did not release authority")
	}
}
