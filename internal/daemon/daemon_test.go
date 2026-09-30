package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
)

// The test binary doubles as a helper process: TestMain intercepts the marker
// environment variable before any test machinery runs.
const helperEnv = "TERMCP_DAEMON_TEST_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "exit":
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startSleeper launches a helper process that idles until terminated.
func startSleeper(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), helperEnv+"=sleep")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = terminate(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// deadPID returns the pid of a helper process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), helperEnv+"=exit")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run helper: %v", err)
	}
	return cmd.Process.Pid
}

func hostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), port
}

// newDocsServer answers /api.md with the termcp docs banner — the public
// fingerprint of an instance that predates the daemon probe.
func newDocsServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# Termcp HTTP API\n\nTermcp is a terminal-session platform.\n")
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newInstanceServer answers like a real termcp instance: the public /api.md
// fingerprint plus the daemon probe at /api/daemon, optionally guarded by a
// bearer token. A non-nil stop is served at POST /api/daemon/stop under the
// same token check.
func newInstanceServer(t *testing.T, info InstanceInfo, token string, stop http.HandlerFunc) *httptest.Server {
	t.Helper()
	authed := func(r *http.Request) bool {
		return token == "" || r.Header.Get("Authorization") == "Bearer "+token
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api.md", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# Termcp HTTP API\n\nTermcp is a terminal-session platform.\n")
	})
	mux.HandleFunc(apiDaemonPath, func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data, _ := json.Marshal(info)
		w.Write(data)
	})
	if stop != nil {
		mux.HandleFunc(apiDaemonStopPath, func(w http.ResponseWriter, r *http.Request) {
			if !authed(r) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			stop(w, r)
		})
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestChildArgs(t *testing.T) {
	base := &config.Config{Host: "127.0.0.1", Port: 18765, DataDir: "D:/data", LogLevel: "info"}
	got := ChildArgs(base, "")
	want := []string{"--host", "127.0.0.1", "--port", "18765", "--data-dir", "D:/data", "--log-level", "info"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default args = %v, want %v", got, want)
	}

	full := &config.Config{
		Host: "0.0.0.0", Port: 19000, DataDir: "/d", AssetsDir: "/a", LogLevel: "debug",
		NoInternal: true, MCPManageSSHConfigs: true, MCPDeferTools: true,
		AuthToken: "sekret-token", AuthHash: "deadbeef",
	}
	got = ChildArgs(full, "30s")
	joined := " " + strings.Join(got, " ") + " "
	for _, want := range []string{
		" --host 0.0.0.0 ", " --port 19000 ", " --data-dir /d ", " --assets /a ",
		" --log-level debug ", " --no-internal ", " --mcp-manage-ssh-configs ",
		" --mcp-defer-tools ", " --idle-timeout 30s ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %v lack %q", got, strings.TrimSpace(want))
		}
	}
	for _, forbidden := range []string{"sekret-token", "deadbeef", "--auth-token", "--auth-hash"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("args carry %q: %v", forbidden, got)
		}
	}
}

func TestChildEnv(t *testing.T) {
	t.Setenv(config.EnvAuthToken, "inherited-old")
	env := childEnv(&config.Config{AuthToken: "resolved-new", DisableAuth: false})
	var tokenEntries []string
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), config.EnvAuthToken+"=") {
			tokenEntries = append(tokenEntries, kv)
		}
	}
	if len(tokenEntries) != 1 || tokenEntries[0] != config.EnvAuthToken+"=resolved-new" {
		t.Errorf("token entries = %v, want exactly the resolved value", tokenEntries)
	}
	if !slices.Contains(env, EnvChild+"=1") {
		t.Errorf("child marker missing from %v", env)
	}
	if slices.Contains(env, config.EnvDisableAuth+"=1") {
		t.Errorf("disable-auth set although the config does not ask for it")
	}
}

func TestConnectHost(t *testing.T) {
	cases := map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "127.0.0.1",
		"[::]":      "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"localhost": "localhost",
		"10.1.2.3":  "10.1.2.3",
	}
	for in, want := range cases {
		if got := ConnectHost(in); got != want {
			t.Errorf("ConnectHost(%q) = %q, want %q", in, got, want)
		}
	}
	if got := BaseURL("0.0.0.0", 18765); got != "http://127.0.0.1:18765" {
		t.Errorf("BaseURL wildcard = %q", got)
	}
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive(self) = false")
	}
	pid := startSleeper(t)
	if !ProcessAlive(pid) {
		t.Errorf("ProcessAlive(sleeper %d) = false", pid)
	}
	if ProcessAlive(deadPID(t)) {
		t.Error("ProcessAlive(exited helper) = true")
	}
}

func TestPortReachable(t *testing.T) {
	ts := newDocsServer(t)
	host, port := hostPort(t, ts.URL)
	if !PortReachable(host, port) {
		t.Error("PortReachable(live server) = false")
	}
	ts.Close()
	if PortReachable(host, port) {
		t.Error("PortReachable(closed server) = true")
	}
}

func TestIsServing(t *testing.T) {
	ctx := context.Background()
	ts := newDocsServer(t)
	host, port := hostPort(t, ts.URL)
	if !IsServing(ctx, host, port) {
		t.Error("IsServing(termcp docs) = false")
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "just a plain web server")
	}))
	defer other.Close()
	host, port = hostPort(t, other.URL)
	if IsServing(ctx, host, port) {
		t.Error("IsServing(non-termcp server) = true")
	}

	closed := newDocsServer(t)
	host, port = hostPort(t, closed.URL)
	closed.Close()
	if IsServing(ctx, host, port) {
		t.Error("IsServing(closed server) = true")
	}
}

func TestInspectEndpoint(t *testing.T) {
	ctx := context.Background()

	daemonTS := newInstanceServer(t, InstanceInfo{Daemon: true, PID: 4242, Version: "v9.9.9", StartedAt: "2026-09-30T10:00:00Z", Log: "/d/termcp.log"}, "", nil)
	host, port := hostPort(t, daemonTS.URL)
	rep := InspectEndpoint(ctx, host, port, "")
	if !rep.Termcp || rep.Unauthorized || rep.Info.PID != 4242 || !rep.Info.Daemon || rep.Info.Version != "v9.9.9" || rep.Info.Log != "/d/termcp.log" {
		t.Errorf("daemon probe = %+v", rep)
	}

	// A manually started instance answers too — that is the point: discovery
	// never depends on how the instance was started.
	manualTS := newInstanceServer(t, InstanceInfo{Daemon: false, PID: 7}, "", nil)
	host, port = hostPort(t, manualTS.URL)
	rep = InspectEndpoint(ctx, host, port, "")
	if !rep.Termcp || rep.Info.Daemon || rep.Info.PID != 7 {
		t.Errorf("manual instance probe = %+v", rep)
	}

	// Guarded: rejected without the token (details unknown, identity via the
	// public fingerprint), answered with it.
	guardedTS := newInstanceServer(t, InstanceInfo{Daemon: true, PID: 11}, "sekret", nil)
	host, port = hostPort(t, guardedTS.URL)
	rep = InspectEndpoint(ctx, host, port, "")
	if !rep.Termcp || !rep.Unauthorized || rep.Info.PID != 0 {
		t.Errorf("guarded probe without token = %+v", rep)
	}
	rep = InspectEndpoint(ctx, host, port, "sekret")
	if !rep.Termcp || rep.Unauthorized || rep.Info.PID != 11 {
		t.Errorf("guarded probe with token = %+v", rep)
	}

	// An instance that predates the probe still identifies through /api.md.
	oldTS := newDocsServer(t)
	host, port = hostPort(t, oldTS.URL)
	rep = InspectEndpoint(ctx, host, port, "")
	if !rep.Termcp || rep.Unauthorized || rep.Info.PID != 0 {
		t.Errorf("fingerprint-only probe = %+v", rep)
	}

	// A plain web server and a dead port both report nothing.
	plainTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	host, port = hostPort(t, plainTS.URL)
	if rep = InspectEndpoint(ctx, host, port, ""); rep.Termcp || rep.Unauthorized {
		t.Errorf("plain server probe = %+v", rep)
	}
	plainTS.Close()
	if rep = InspectEndpoint(ctx, host, port, ""); rep.Termcp {
		t.Errorf("closed port probe = %+v", rep)
	}
}

func TestStopEndpoint(t *testing.T) {
	ctx := context.Background()

	// Nothing answers: nothing to stop.
	goneTS := newInstanceServer(t, InstanceInfo{Daemon: true, PID: 1}, "", nil)
	host, port := hostPort(t, goneTS.URL)
	goneTS.Close()
	if res, err := StopEndpoint(ctx, host, port, ""); err != nil || res.Stopped || res.NotDaemon {
		t.Errorf("stop of a dead port = (%+v, %v), want nothing running", res, err)
	}

	// A manually started instance has no daemon lifecycle: it is reported, not
	// stopped, and nothing is hunted down as a process.
	manualTS := newInstanceServer(t, InstanceInfo{Daemon: false, PID: 7}, "", nil)
	host, port = hostPort(t, manualTS.URL)
	res, err := StopEndpoint(ctx, host, port, "")
	if err != nil || res.Stopped || !res.NotDaemon || res.PID != 7 {
		t.Errorf("stop of a manual instance = (%+v, %v)", res, err)
	}
	if !PortReachable(host, port) {
		t.Error("stop took the manual instance down")
	}

	// The stop goes over HTTP: the route answers, then the endpoint goes away.
	var daemonTS *httptest.Server
	daemonTS = newInstanceServer(t, InstanceInfo{Daemon: true, PID: 21}, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(50 * time.Millisecond)
			daemonTS.Close()
		}()
	})
	host, port = hostPort(t, daemonTS.URL)
	res, err = StopEndpoint(ctx, host, port, "")
	if err != nil || !res.Stopped || res.PID != 21 {
		t.Fatalf("stop of a daemon = (%+v, %v)", res, err)
	}
	if PortReachable(host, port) {
		t.Error("endpoint still reachable after the stop")
	}

	// The token guards both the probe and the stop itself.
	var guardedTS *httptest.Server
	guardedTS = newInstanceServer(t, InstanceInfo{Daemon: true, PID: 33}, "sekret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(50 * time.Millisecond)
			guardedTS.Close()
		}()
	})
	ghost, gport := hostPort(t, guardedTS.URL)
	if _, err := StopEndpoint(ctx, ghost, gport, ""); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Errorf("stop without token err = %v, want an authentication error", err)
	}
	res, err = StopEndpoint(ctx, ghost, gport, "sekret")
	if err != nil || !res.Stopped || res.PID != 33 {
		t.Errorf("stop with token = (%+v, %v)", res, err)
	}
}

// Ensure never spawns in these tests: every path is either a reuse or a
// refusal decided before any child would launch.

func TestEnsureReusesManualInstance(t *testing.T) {
	ts := newInstanceServer(t, InstanceInfo{Daemon: false, PID: os.Getpid(), Version: "test"}, "", nil)
	host, port := hostPort(t, ts.URL)
	cfg := &config.Config{Host: host, Port: port, DataDir: t.TempDir(), LogLevel: "info"}

	res, err := Ensure(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Spawned || res.Unauthorized || res.Info.PID != os.Getpid() || res.Info.Daemon {
		t.Errorf("Ensure = %+v, want reuse of the manual instance", res)
	}
}

func TestEnsureReusesDaemonInstance(t *testing.T) {
	ts := newInstanceServer(t, InstanceInfo{Daemon: true, PID: 4242, Version: "v9"}, "", nil)
	host, port := hostPort(t, ts.URL)
	cfg := &config.Config{Host: host, Port: port, DataDir: t.TempDir(), LogLevel: "info"}

	res, err := Ensure(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Spawned || !res.Info.Daemon || res.Info.PID != 4242 {
		t.Errorf("Ensure = %+v, want reuse of the daemon instance", res)
	}
}

func TestEnsureReusesRejectedEndpoint(t *testing.T) {
	ts := newInstanceServer(t, InstanceInfo{Daemon: true, PID: 11}, "sekret", nil)
	host, port := hostPort(t, ts.URL)
	cfg := &config.Config{Host: host, Port: port, DataDir: t.TempDir(), LogLevel: "info"}

	res, err := Ensure(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Spawned || !res.Unauthorized || res.Info.PID != 0 {
		t.Errorf("Ensure = %+v, want a rejected-but-reused endpoint", res)
	}
}

// With only a hash configured, Ensure presents the hash string itself — a
// hash-configured instance accepts it, so management works with the hash
// alone at hand.
func TestEnsureUsesHashCredential(t *testing.T) {
	secret := "sha256-aabbccdd-" + strings.Repeat("00", 32)
	ts := newInstanceServer(t, InstanceInfo{Daemon: true, PID: 77}, secret, nil)
	host, port := hostPort(t, ts.URL)
	cfg := &config.Config{Host: host, Port: port, DataDir: t.TempDir(), LogLevel: "info", AuthHash: secret}

	res, err := Ensure(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Spawned || res.Unauthorized || res.Info.PID != 77 {
		t.Errorf("Ensure = %+v, want reuse with the hash credential", res)
	}
}

func TestEnsureRefusesForeignPort(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not termcp")
	}))
	defer ts.Close()
	host, port := hostPort(t, ts.URL)
	cfg := &config.Config{Host: host, Port: port, DataDir: t.TempDir(), LogLevel: "info"}

	_, err := Ensure(context.Background(), cfg, "")
	if err == nil || !strings.Contains(err.Error(), "holds") {
		t.Fatalf("Ensure on a foreign port = %v, want a refusal", err)
	}
}

// Every request a management probe makes must identify itself as probe traffic:
// the daemon probe, and the /api.md fingerprint it falls back to when the probe
// is rejected. An instance's idle countdown exempts probe traffic (see IsProbe
// and internal/daemon/idle_test.go), so a status query on a guarded instance
// must not keep alive the instance it is reporting on -- which is exactly what
// happened while only /api/daemon was exempt and this fallback was not.
func TestProbeRequestsAreTagged(t *testing.T) {
	seen := make(chan *http.Request, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r:
		default:
		}
		if r.URL.Path == apiDaemonPath {
			// Rejected, so InspectEndpoint must fall back to /api.md.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, "# Termcp HTTP API\n\nTermcp is a terminal-session platform.\n")
	}))
	defer ts.Close()
	host, port := hostPort(t, ts.URL)

	if !IsServing(context.Background(), host, port) {
		t.Fatal("IsServing = false against the docs fingerprint")
	}
	rep := InspectEndpoint(context.Background(), host, port, "")
	if !rep.Termcp || !rep.Unauthorized {
		t.Fatalf("InspectEndpoint = %+v, want Termcp+Unauthorized", rep)
	}

	// Both requests the two calls made must carry the marker.
	paths := map[string]bool{}
	for {
		select {
		case r := <-seen:
			paths[r.URL.Path] = true
			if !IsProbe(r) {
				t.Errorf("%s %s is not marked as probe traffic; a status query would feed the idle countdown", r.Method, r.URL.Path)
			}
			continue
		default:
		}
		break
	}
	for _, want := range []string{"/api.md", apiDaemonPath} {
		if !paths[want] {
			t.Fatalf("no probe request reached %s (saw %v)", want, paths)
		}
	}
}

// The predicate also keeps the guarantee for a caller that asks the probe route
// directly without the header (an older binary, or a hand-written curl).
func TestIsProbeRecognizesDaemonPathWithoutHeader(t *testing.T) {
	direct := httptest.NewRequest(http.MethodGet, apiDaemonPath, nil)
	if !IsProbe(direct) {
		t.Error("GET /api/daemon must count as probe traffic even unmarked")
	}
	// Everything else is real activity: an ordinary request must never be
	// silently exempted from the countdown.
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, apiDaemonPath, nil),
		httptest.NewRequest(http.MethodGet, "/api/sessions", nil),
		httptest.NewRequest(http.MethodGet, "/api/version", nil),
	} {
		if IsProbe(r) {
			t.Errorf("%s %s counts as a probe; only the daemon probe may be exempt", r.Method, r.URL.Path)
		}
	}
}

// The reported countdown is what a client needs to ping faster than it. Reading
// it back through the probe is the whole point of the field.
func TestInstanceInfoIdleTimeout(t *testing.T) {
	for _, c := range []struct {
		ms   int64
		want time.Duration
	}{
		{0, 0},                    // never auto-exits
		{6000, 6 * time.Second},   // --idle-timeout 6s
		{30000, 30 * time.Second}, // the default
	} {
		if got := (InstanceInfo{IdleTimeoutMS: c.ms}).IdleTimeout(); got != c.want {
			t.Errorf("IdleTimeout(%d ms) = %v, want %v", c.ms, got, c.want)
		}
	}
	// And it must survive the round trip a real client makes.
	ts := newInstanceServer(t, InstanceInfo{Daemon: true, PID: 5, IdleTimeoutMS: 6000}, "", nil)
	host, port := hostPort(t, ts.URL)
	rep := InspectEndpoint(context.Background(), host, port, "")
	if got := rep.Info.IdleTimeout(); got != 6*time.Second {
		t.Errorf("probed countdown = %v, want 6s", got)
	}
}
