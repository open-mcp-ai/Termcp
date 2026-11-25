package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRESTForwardAddressMustNameTheListeningSide pins that POST
// /api/sessions/{id}/forwards takes the address field of the side that listens,
// and refuses the other one instead of dropping it.
//
// The Web UI's forward dialog has ONE "Listen host" box, and it used to post it as
// `local_host` for every direction. The local branch reads `bind_host`, so the box
// did nothing at all for `ssh -L`: the request was accepted, the field was ignored,
// and the tunnel came up on 127.0.0.1. A caller cannot tell that from a working
// forward — the reply carries a listening address, it is just not the one asked
// for — which is why this is a 400 and not a silent default.
func TestRESTForwardAddressMustNameTheListeningSide(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	p := newRestProbe(t, []string{"forward.open"}, nil)
	sid := p.sess.ID

	post := func(body string) (int, string) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/forwards", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		p.mux.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "local listens here",
			body: `{"direction":"local","remote_host":"localhost","remote_port":80,"local_host":"0.0.0.0"}`,
			want: "bind_host",
		},
		{
			name: "remote listens on the target",
			body: `{"direction":"remote","local_host":"127.0.0.1","local_port":19090,"remote_host":"localhost","remote_port":80,"bind_host":"0.0.0.0"}`,
			want: "local_host",
		},
		{
			name: "dynamic has no target side",
			body: `{"direction":"dynamic","local_port":0,"local_host":"127.0.0.1"}`,
			want: "this host only",
		},
		{
			name: "dynamic has no fixed target",
			body: `{"direction":"dynamic","local_port":0,"remote_host":"db","remote_port":5432}`,
			want: "no fixed target",
		},
		{
			// The unauthenticated SOCKS5 proxy is loopback-only by construction, and
			// the caller wrote the address — so this is a bad request, not a 500.
			name: "dynamic refuses a public bind",
			body: `{"direction":"dynamic","local_port":0,"bind_host":"0.0.0.0"}`,
			want: "open proxy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := post(tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("POST %s = %d (%s), want 400", tc.body, code, body)
			}
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("the refusal is not the JSON error shape: %v (%s)", err, body)
			}
			if !strings.Contains(got.Error, tc.want) {
				t.Errorf("the refusal does not name %q, so it cannot be acted on: %q", tc.want, got.Error)
			}
		})
	}

	// Nothing above bound anything: a refused request must not leave a listener
	// on the address it was told to use.
	if got := len(p.forwardMgr.List()); got != 0 {
		t.Fatalf("%d forward(s) registered by refused requests, want 0", got)
	}

	// The control: the field that does apply still creates the forward.
	code, body := post(`{"direction":"local","remote_host":"localhost","remote_port":80,"bind_host":"127.0.0.1","local_port":0}`)
	if code != http.StatusCreated {
		t.Fatalf("a local forward with bind_host = %d (%s), want 201", code, body)
	}
	if !strings.Contains(body, `"listen_addr":"127.0.0.1:`) {
		t.Errorf("the reply does not carry the address it listened on: %s", body)
	}
}
