package mcp

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/term/directive"
)

// TestNamedKindNeverFallsThroughToTheContainer pins the difference between "close
// this peer" and "close this session".
//
// `close <session> mcp 1` used to parse the kind away, find no branch for it, and
// take the default path — closing the SESSION and every channel on it. A caller
// naming a peer destroyed the whole link and got a clean 204 for it. The same shape
// as #132: a qualifier treated as decoration instead of as the object.
func TestNamedKindNeverFallsThroughToTheContainer(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]

	// A kind this verb does not handle must be refused, not silently downgraded.
	for _, cmd := range []string{"close " + sid + " mcp 1", "close " + sid + " file /tmp/x"} {
		got := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("%q was accepted; a named kind must never fall through:\n%s", cmd, got)
		}
	}

	// And the session is still there, which is the whole point: the refusals above
	// must not have closed anything.
	after := termText(t, s, map[string]any{"cmd": "ls"})
	if !strings.Contains(after, sid) {
		t.Fatalf("the session was closed by a refused command:\n%s", after)
	}
	termText(t, s, map[string]any{"cmd": "close " + h})
}

// TestADeclaredKindIsEitherHandledOrRefused is the general form of the test above:
// for every verb and every kind it declares, `verb <session> <kind> <key>` must
// either reach a branch for that kind or be refused outright. What it must never do
// is answer 2xx from a path that ignored the kind, because that is a command that
// did something other than what was asked.
//
// The session is a live pipe and the key is deliberately nonsense, so a branch that
// does run will fail on its own terms — a 4xx here means "the kind was not silently
// discarded", which is exactly the property under test.
func TestADeclaredKindIsEitherHandledOrRefused(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, op := range directive.Ops() {
		for _, kind := range op.Kinds {
			cmd := op.Name + " " + sid + " " + kind + " zz-nonsense"
			res := termCall(t, s, map[string]any{"cmd": cmd})
			body := toolResultText(res)
			// A 2xx is FINE when the branch really handled the kind: `ls forward` on a
			// session with no tunnels is an empty list, and `rm alias nosuch` is a no-op
			// that succeeds. What is not fine is a 2xx that ignored the kind and acted on
			// the session — and that failure shows up as the session disappearing, which
			// the check below catches.
			if res.IsError && strings.Contains(body, "does not take a") {
				continue // refused by name, which is the other acceptable answer
			}
			if !res.IsError && !strings.Contains(body, "TERM/1 2") {
				t.Errorf("%q answered neither a refusal nor a result:\n%s", cmd, body)
			}
		}
	}

	// The session survived every one of them, which also proves none of the refusals
	// took the default "act on the target" path.
	if after := termText(t, s, map[string]any{"cmd": "ls"}); !strings.Contains(after, sid) {
		t.Fatalf("a declared-kind probe closed the session:\n%s", after)
	}
}

// TestEveryFileOpReachesItsGroup pins the two-level SFTP surface from the caller's
// side: `--op chmod` must reach the perm group, `--op realpath` the fs group, and so
// on. The group is an implementation detail; a caller names the operation.
//
// It exists because the first version of `get` hardcoded the fs group, so `--op stat`
// answered "action must be truncate, realpath, or statvfs" — the operation was real,
// the group was wrong, and the message described a layer the caller never mentioned.
func TestEveryFileOpReachesItsGroup(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// The ops that belong on get/write. symlink and link do not: creating a link is
	// a CREATE operation, on `create … file --target`, because a link needs a target
	// and neither get nor write has that flag.
	for _, op := range []string{
		"stat", "getwd", "urls", "realpath", "readlink",
		"chmod", "chown", "chtimes", "truncate", "statvfs",
	} {
		for _, verb := range []string{"get", "write"} {
			cmd := verb + " " + sid + " file /tmp/x --op " + op
			got := termText(t, s, map[string]any{"cmd": cmd})
			// "does not know" means the table is missing the op; any other answer —
			// success, or a failure about the path — means the group was found.
			if strings.Contains(got, "does not know") {
				t.Errorf("%q: --op %s is not mapped to a group:\n%s", cmd, op, got)
			}
			// And the group's own complaint must never be what the caller sees.
			if strings.Contains(got, "action must be truncate, realpath, or statvfs") {
				t.Errorf("%q: reached the fs group with an op it does not hold:\n%s", cmd, got)
			}
		}
	}
}

// TestEveryKindTheDispatcherServesIsParsable is the REVERSE of
// TestADeclaredKindIsEitherHandledOrRefused, and it is the one that would have
// caught `create <session> file <path>`.
//
// That test starts from the declared kinds and asks whether each is handled. It
// cannot see a branch that exists in the dispatcher but whose kind was never
// declared — and `create`'s file branch was exactly that: unreachable, because the
// parser had no reason to read `file` as a kind and left it in Args. The verb
// answered "create needs a kind" for a shape its own usage line advertised.
//
// So this walks the OTHER direction: for every verb-and-kind pair the dispatcher
// serves, the statement must parse into that kind and key. A new branch with no
// declaration fails here.
func TestEveryKindTheDispatcherServesIsParsable(t *testing.T) {
	cases := []struct{ cmd, wantKind, wantKey string }{
		// open
		{"open s1 forward --local-port 0", "forward", ""},
		{"open s1 mcp http://h/mcp", "mcp", "http://h/mcp"},
		{"open s1 cdp ws://h/d", "cdp", "ws://h/d"},
		{"open s1 jrpc ws://h/s", "jrpc", "ws://h/s"},
		// ls
		{"ls host", "host", ""},
		{"ls ext", "ext", ""},
		{"ls alias", "alias", ""},
		{"ls s1 forward", "forward", ""},
		{"ls s1 file /tmp", "file", "/tmp"},
		// rm
		{"rm s1 file /tmp/x", "file", "/tmp/x"},
		{"rm host prod", "host", "prod"},
		{"rm ext wire", "ext", "wire"},
		{"rm alias ll", "alias", "ll"},
		// close
		{"close s1 forward fw-1", "forward", "fw-1"},
		// read / write / get
		{"read s1 file /tmp/x", "file", "/tmp/x"},
		{"write s1 file /tmp/x --data y", "file", "/tmp/x"},
		{"get s1 file /tmp/x", "file", "/tmp/x"},
		// create / edit
		{"create host prod", "host", "prod"},
		{"create ext wire", "ext", "wire"},
		{"create alias ll ls", "alias", "ll"},
		{"create s1 file /tmp/new", "file", "/tmp/new"},
		{"edit host prod", "host", "prod"},
		{"edit ext wire", "ext", "wire"},
		{"edit alias ll", "alias", "ll"},
	}
	for _, tc := range cases {
		f, err := directive.Parse(tc.cmd)
		if err != nil {
			t.Errorf("%q does not parse: %v", tc.cmd, err)
			continue
		}
		st := f.Statements[0]
		if st.Kind != tc.wantKind {
			t.Errorf("%q parsed kind = %q, want %q (args=%v)", tc.cmd, st.Kind, tc.wantKind, st.Args)
		}
		if st.Key != tc.wantKey {
			t.Errorf("%q parsed key = %q, want %q", tc.cmd, st.Key, tc.wantKey)
		}
	}
}

// TestCreateFileMakesADirectoryOrASymlink pins both shapes of `create <session>
// file <path>`, including which argument is which.
//
// The node's key is the path being created and --target is what a symlink points at,
// while the handler names those link_path and target — the opposite order. Getting it
// backwards makes a symlink at the wrong end of the path, which still answers 200.
func TestCreateFileMakesADirectoryOrASymlink(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	dir := filepath.Join(t.TempDir(), "made-by-create")
	if got := termText(t, s, map[string]any{"cmd": "create " + sid + " file " + dir}); !strings.Contains(got, "success") {
		t.Fatalf("create file did not make a directory:\n%s", got)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the directory was not created on disk: %v", err)
	}

	file := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "made-by-create-link")
	got := termText(t, s, map[string]any{"cmd": "create " + sid + " file " + link + " --target " + file})
	if strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("create file --target was refused:\n%s", got)
	}
	// The link exists AT THE KEY and points AT --target; readlink proves the direction.
	back, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("no symlink at the key path: %v", err)
	}
	if back != file {
		t.Fatalf("the symlink points at %q, want %q", back, file)
	}
}

// TestRemoveFileDeletesTheNodeAndNotTheSession is the pair to the create test above,
// and it exists because `rm <session>` and `rm <session> file <path>` differ by one
// word while destroying very different things. A kind that fails to parse turns the
// second into the first.
func TestRemoveFileDeletesTheNodeAndNotTheSession(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := termText(t, s, map[string]any{"cmd": "rm " + sid + " file " + victim}); strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("rm file was refused:\n%s", got)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("the file is still there: %v", err)
	}

	// The session outlived it — that is the whole point of the distinction.
	if after := termText(t, s, map[string]any{"cmd": "ls"}); !strings.Contains(after, sid) {
		t.Fatalf("rm file took the session with it:\n%s", after)
	}
}

// TestEveryForwardDirectionIsReachable pins that `open <session> forward` can express
// all three directions.
//
// The rewrite hardcoded --action local, so `--dynamic` and `--remote` were not flags
// of `open` at all: SOCKS5 and remote forwards became unreachable while the detail
// page went on describing all three. Nothing failed, because "a forward was created"
// is true for every direction.
func TestEveryForwardDirectionIsReachable(t *testing.T) {
	s := newTestServer(t)
	// The forward manager is what `open … forward` talks to, and newTestServer leaves it
	// nil because most tests never need one — so a refusal here would be about the
	// fixture, not about the direction being reachable.
	s.forwardMgr = forward.NewForwardManager()
	s.sessMgr.AddOnDeadHook(s.forwardMgr.CloseBySession)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, tc := range []struct{ flag, body string }{
		{"", "--local-port 0 --remote-host 127.0.0.1 --remote-port 22"},
		{"--dynamic", "--local-port 0"},
		// A remote forward binds the port on the TARGET, so 0 ("any free port") is
		// rejected there — the caller has to pick the number it wants published. That
		// number cannot be a constant: the suite runs shuffled, in parallel, on a runner
		// that may already have it. %PORT% is filled per attempt below.
		{"--remote", "--local-port %PORT% --remote-host 127.0.0.1 --remote-port 22"},
	} {
		cmd := "open " + sid + " forward " + tc.flag + " " + tc.body
		if strings.Contains(cmd, "%PORT%") {
			cmd = strings.ReplaceAll(cmd, "%PORT%", fmt.Sprint(freePort(t)))
		}
		res := termCall(t, s, map[string]any{"cmd": cmd})
		body := toolResultText(res)
		if res.IsError {
			t.Errorf("%q was refused:\n%s", cmd, body)
			continue
		}
		if !strings.Contains(body, "forward_id") {
			t.Errorf("%q created no forward:\n%s", cmd, body)
		}
	}
}

// TestForwardIsAddressedTheSameWayEverywhere pins one spelling per object.
//
// The tree says `close <session> forward <id>` — the id is the node, so it is the key.
// The dispatcher passed `--action close` and dropped the key, so the handler asked for
// `--forward-id` and the documented form was refused with "forward_id required". The
// same object was addressed positionally when opening and by flag when closing.
func TestForwardIsAddressedTheSameWayEverywhere(t *testing.T) {
	s := newTestServer(t)
	s.forwardMgr = forward.NewForwardManager()
	s.sessMgr.AddOnDeadHook(s.forwardMgr.CloseBySession)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	out := termText(t, s, map[string]any{"cmd": "open " + sid + " forward --local-port 0 --remote-host 127.0.0.1 --remote-port 22"})
	id := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "forward_id:"); ok {
			id = strings.TrimSpace(v)
		}
	}
	if id == "" {
		t.Fatalf("no forward_id in the open reply:\n%s", out)
	}

	// The id from that reply, closed the way the usage line spells it.
	if got := termText(t, s, map[string]any{"cmd": "close " + sid + " forward " + id}); strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("close <session> forward <id> was refused:\n%s", got)
	}
	if list := termText(t, s, map[string]any{"cmd": "ls " + sid + " forward"}); strings.Contains(list, id) {
		t.Errorf("the forward %s is still listed after close:\n%s", id, list)
	}

	// A key that would be ignored is refused rather than dropped: `ls` lists a
	// collection and has nothing to scope it with.
	if got := termText(t, s, map[string]any{"cmd": "ls " + sid + " forward " + id}); !strings.HasPrefix(got, "TERM/1 4") {
		t.Errorf("ls with a forward id was accepted:\n%s", got)
	}
}

// freePort asks the kernel for a port nobody is using and hands the number back.
//
// A test that needs a SPECIFIC port (a remote forward cannot take 0) has to name one,
// and a constant is a shared resource: the suite runs shuffled, in parallel, and the
// runner may already have it. The release-then-use window is inherent to asking for a
// number instead of a listener; it is small, and retrying is what the forward handler
// already does for the same reason.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

// TestRepliesDoNotNameADeletedDirective pins that the advice in a reply is advice that
// can be followed.
//
// Twice now a user-facing message kept naming a directive this branch deleted:
// `mcp tools <handle>` after a failed MCP handshake, and `mcp recv <handle>` after a
// frame was sent to a stdio peer. Neither breaks a test — the reply is well formed and
// the handle is right — and both send the caller to "unknown directive", which reads
// like the capability is missing rather than like the sentence is stale.
//
// The check is on the words a caller would type, not on comments: every directive name
// the reply suggests must still resolve.
func TestRepliesDoNotNameADeletedDirective(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	replies := map[string]string{
		// A frame sent to a stdio peer: the reply says how to read the answer.
		"call": termText(t, s, map[string]any{"cmd": `call ` + h + ` {"jsonrpc":"2.0","id":1,"method":"ping"}`}),
		// A peer that cannot be reached: the reply explains what failed.
		"open mcp": termText(t, s, map[string]any{"cmd": "open " + strings.SplitN(h, ":", 2)[0] + " mcp http://127.0.0.1:9/mcp"}),
	}
	for what, body := range replies {
		for _, deleted := range []string{"mcp recv", "mcp send", "mcp tools", "mcp call", "mcp close"} {
			if strings.Contains(body, deleted) {
				t.Errorf("%s: the reply tells the caller to run %q, which no longer exists:\n%s", what, deleted, body)
			}
		}
	}
}

// TestCloseWithAForwardIDDoesNotTerminateTheSession pins the destructive half of
// the same class of bug: `close <session> --forward-id fw-…` used to answer 204 and
// terminate the WHOLE session, because the dispatcher declared the flag, let the
// parser accept it, and then closed the container. A caller asking to stop one
// tunnel silently lost the connection (found by dogfooding #159).
func TestCloseWithAForwardIDDoesNotTerminateTheSession(t *testing.T) {
	s := newTestServer(t)
	s.forwardMgr = forward.NewForwardManager()
	s.sessMgr.AddOnDeadHook(s.forwardMgr.CloseBySession)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	out := termText(t, s, map[string]any{"cmd": "open " + sid + " forward --local-port 0 --remote-host 127.0.0.1 --remote-port 22"})
	id := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "forward_id:"); ok {
			id = strings.TrimSpace(v)
		}
	}
	if id == "" {
		t.Fatalf("no forward_id in the open reply:\n%s", out)
	}

	if got := termText(t, s, map[string]any{"cmd": "close " + sid + " --forward-id " + id}); strings.HasPrefix(got, "TERM/1 4") || strings.HasPrefix(got, "TERM/1 5") {
		t.Fatalf("close --forward-id was refused:\n%s", got)
	}

	// The tunnel is gone...
	if list := termText(t, s, map[string]any{"cmd": "ls " + sid + " forward"}); strings.Contains(list, id) {
		t.Errorf("the forward %s is still listed after close:\n%s", id, list)
	}
	// ...and the session it belonged to is NOT.
	if got := termText(t, s, map[string]any{"cmd": "get " + sid}); strings.Contains(got, "status: exited") {
		t.Fatalf("closing a forward terminated the session:\n%s", got)
	}
	// The shell still answers, which is the only proof that survives a status field.
	// The line travels as the VALUE of `$1` — a literal payload is refused now — so a
	// reply that merely quotes the refusal cannot pass for the command's output.
	if got := termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo alive"}}); !strings.Contains(got, "alive") {
		t.Errorf("the channel stopped working after closing a forward:\n%s", got)
	}

	// And the two deadlines together are refused instead of one being dropped.
	if got := termText(t, s, map[string]any{"cmd": "close " + sid + " --forward-id " + id + " --grace 3"}); !strings.HasPrefix(got, "TERM/1 4") {
		t.Errorf("--grace beside --forward-id was accepted:\n%s", got)
	}
}
