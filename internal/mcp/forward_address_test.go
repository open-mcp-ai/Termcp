package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/forward"
)

// TestTheListenAddressFlagNamesTheSideThatListens pins that the two address flags
// are not interchangeable, and that the wrong one is refused instead of dropped.
//
// `open <session> forward --local-port 8080 --local-host 0.0.0.0` used to answer
// 200: `--local-host` is declared on `open` (it is the address a REMOTE forward
// binds on the target), it was forwarded to the tool, and the local branch never
// read it. The listener stayed on 127.0.0.1 and the reply named a listening
// address, so the caller had a successful-looking answer about an address it had
// not asked for — the failure mode with no symptom.
func TestTheListenAddressFlagNamesTheSideThatListens(t *testing.T) {
	s := newTestServer(t)
	// `open … forward` talks to the manager, and newTestServer leaves it nil
	// because most tests never need one (see TestEveryForwardDirectionIsReachable).
	s.forwardMgr = forward.NewForwardManager()
	s.sessMgr.AddOnDeadHook(s.forwardMgr.CloseBySession)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, tc := range []struct {
		name string
		cmd  string
		// want are the words the refusal must carry: the flag that was wrong and
		// the one that works. A message naming neither leaves the caller to guess
		// again, which is how the flag was written wrong in the first place.
		want []string
	}{
		{
			name: "local does not take --local-host",
			cmd:  " --local-port 0 --remote-host 127.0.0.1 --remote-port 22 --local-host 0.0.0.0",
			want: []string{"local-host", "bind-host"},
		},
		{
			name: "remote does not take --bind-host",
			cmd:  " --remote --local-port 0 --local-host 127.0.0.1 --remote-host 127.0.0.1 --remote-port 22 --bind-host 0.0.0.0",
			want: []string{"bind-host", "local-host"},
		},
		{
			name: "dynamic does not take --local-host",
			cmd:  " --dynamic --local-port 0 --local-host 127.0.0.1",
			want: []string{"local-host", "bind-host"},
		},
		{
			name: "dynamic has no fixed target",
			cmd:  " --dynamic --local-port 0 --remote-host 127.0.0.1 --remote-port 22",
			want: []string{"remote-host", "local forward"},
		},
		{
			// The loopback-only rule for the unauthenticated proxy is unchanged; what
			// is pinned here is that breaking it answers 400 (the caller wrote the
			// address) and not 500 (the server broke).
			name: "dynamic refuses a public bind as a bad request",
			cmd:  " --dynamic --local-port 0 --bind-host 0.0.0.0",
			want: []string{"open proxy", "local forward"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, map[string]any{"cmd": "open " + sid + " forward" + tc.cmd})
			if !strings.HasPrefix(got, "TERM/1 400") {
				t.Fatalf("refused call did not answer 400:\n%s", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("the refusal does not name %q, so the caller cannot see which flag works:\n%s", w, got)
				}
			}
		})
	}

	// THE REFUSAL MUST NOT BE A COATING OVER A WORKING CALL: nothing above may
	// have left a listener behind under the address it was told to use.
	for _, fw := range s.forwardMgr.List() {
		if strings.HasPrefix(fw.ListenAddr, "0.0.0.0:") {
			t.Errorf("a refused call bound a public address anyway: %s", fw.ListenAddr)
		}
	}

	// The control: the flag that does apply still works, so the guard above is a
	// check on the spelling and not a closed door.
	out := termText(t, s, map[string]any{"cmd": "open " + sid + " forward --local-port 0 --remote-host 127.0.0.1 --remote-port 22 --bind-host 127.0.0.1"})
	if !strings.HasPrefix(out, "TERM/1 200") {
		t.Fatalf("a local forward with --bind-host was refused:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:") {
		t.Errorf("the reply does not carry the address it listened on:\n%s", out)
	}
}
