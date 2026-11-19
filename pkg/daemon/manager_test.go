package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/detach"
)

// The exclusive ready file stands for a private local endpoint. Its owner
// removes it when shutting down. All endpoints, logs and sentinels are unique
// to this TempDir; no process outside the test is ever signalled.
func TestManagerChild(t *testing.T) {
	dir := os.Getenv("MANAGER_TEST_CHILD")
	if dir == "" {
		return
	}
	if os.Getenv("MANAGER_TEST_MODE") == "inspect" {
		m := New(testManagerConfig(dir))
		status := m.Status(context.Background())
		if os.Getenv("MANAGER_TEST_BUSY") == "1" {
			if !errors.Is(status.Err, ErrBusy) {
				t.Fatalf("expected busy in another invocation, got %+v", status)
			}
		} else if status.Err != nil || !status.Running {
			t.Fatalf("independent status = %+v", status)
		}
		return
	}
	fmt.Println("fixture started")
	ready := filepath.Join(dir, "ready")
	f, err := os.OpenFile(ready, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(ready)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitFor(ctx, func() (bool, error) {
		_, err := os.Stat(filepath.Join(dir, "stop"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return true, err
	}); err != nil {
		t.Fatal(err)
	}
}

func testManagerConfig(dir string) Config {
	return Config{
		Name: "fixture", DataDir: dir, ReadyWait: 15 * time.Second,
		BuildArgs: func() []string { return []string{"-test.run=^TestManagerChild$"} },
		BuildEnv: func() []string {
			return append(os.Environ(), "MANAGER_TEST_CHILD="+dir, "GORACE=atexit_sleep_ms=0")
		},
		Probe: func(context.Context) error {
			_, err := os.Stat(filepath.Join(dir, "ready"))
			if errors.Is(err, os.ErrNotExist) {
				return ErrNotRunning
			}
			return err
		},
		RequestStop: func(context.Context) error {
			return os.WriteFile(filepath.Join(dir, "stop"), nil, 0600)
		},
	}
}

func testCLI(t *testing.T, cfg Config, extra ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestManagerChild$")
	cmd.Env = append(cfg.BuildEnv(), "MANAGER_TEST_MODE=inspect")
	cmd.Env = append(cmd.Env, extra...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("independent CLI: %v: %s", err, out)
	}
}

func TestManagerLifecycle(t *testing.T) {
	cfg := testManagerConfig(t.TempDir())
	cfg.LogFile = filepath.Join(cfg.DataDir, "chosen.log")
	m := New(cfg)
	first, err := m.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cfg.RequestStop(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := waitFor(ctx, func() (bool, error) { return !detach.ProcessAlive(first.PID), nil }); err != nil {
			t.Error("child did not exit", err)
		}
	})
	testCLI(t, cfg)
	second, err := New(cfg).Start(t.Context())
	if err != nil || second.PID != first.PID {
		t.Fatalf("duplicate start: %v, %v; first = %v", second, err, first)
	}
	log, err := os.ReadFile(cfg.LogFile)
	if err != nil || !strings.Contains(string(log), "fixture started") {
		t.Fatalf("child stdout did not reach log: %v, %q", err, log)
	}
	noStop := cfg
	noStop.RequestStop = nil
	if err := New(noStop).Stop(t.Context()); err == nil {
		t.Fatal("live child was stopped without a callback")
	}
	noStop.RequestStop = func(context.Context) error { return errors.New("stop denied") }
	if err := New(noStop).Stop(t.Context()); err == nil {
		t.Fatal("ignored failed stop")
	}
	if err := New(cfg).Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := New(cfg).Stop(t.Context()); err != nil {
		t.Fatal("repeated stop:", err)
	}
	if s := New(cfg).Status(t.Context()); s.Err != nil || s.Running {
		t.Fatalf("stopped status: %+v", s)
	}
}

func TestManagerCrossProcessFence(t *testing.T) {
	cfg := testManagerConfig(t.TempDir())
	cfg.Probe = func(context.Context) error {
		testCLI(t, cfg, "MANAGER_TEST_BUSY=1")
		return ErrNotRunning
	}
	if err := New(cfg).Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker string
		probe  error
	}{
		{"identity mismatch", "", errors.New("wrong instance")},
		{"unowned endpoint", "", nil},
		{"unreachable live PID", strconv.Itoa(os.Getpid()), ErrNotRunning},
		{"half-started", "starting", ErrNotRunning},
		{"invalid PID", "-1", ErrNotRunning},
		{"truncated PID", "4294967300", ErrNotRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testManagerConfig(t.TempDir())
			cfg.Probe = func(context.Context) error { return tc.probe }
			cfg.BuildArgs = func() []string { t.Fatal("unexpected spawn"); return nil }
			cfg.RequestStop = func(context.Context) error { t.Fatal("unexpected shutdown"); return nil }
			m := New(cfg)
			if tc.marker != "" {
				if err := os.WriteFile(m.path(".pid"), []byte(tc.marker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if s := m.Status(t.Context()); s.Err == nil || s.Running {
				t.Fatalf("ambiguous status: %+v", s)
			}
			if _, err := m.Start(t.Context()); err == nil || m.Stop(t.Context()) == nil {
				t.Fatal("accepted ambiguous state")
			}
			if tc.marker != "" {
				b, err := os.ReadFile(m.path(".pid"))
				if string(b) != tc.marker || err != nil {
					t.Fatalf("marker changed: %q %v", b, err)
				}
			}
		})
	}
}

func TestManagerBoundedProbe(t *testing.T) {
	cfg := testManagerConfig(t.TempDir())
	cfg.ReadyWait = 100 * time.Millisecond
	cfg.Probe = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	if _, err := New(cfg).Start(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start deadline: %v", err)
	}
}

func TestManagerEarlyEndpointCloseKeepsFence(t *testing.T) {
	cfg := testManagerConfig(t.TempDir())
	instance, err := New(cfg).Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = testManagerConfig(cfg.DataDir).RequestStop(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := waitFor(ctx, func() (bool, error) { return processExited(instance.PID), nil }); err != nil {
			t.Fatal("child did not exit:", err)
		}
	})
	cfg.ReadyWait = 100 * time.Millisecond
	cfg.RequestStop = func(context.Context) error { return os.Remove(filepath.Join(cfg.DataDir, "ready")) }
	if err := New(cfg).Stop(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("process still alive but stop returned %v", err)
	}
	if _, err := New(cfg).Start(t.Context()); err == nil {
		t.Fatal("spawned while old child was still draining")
	}
}

func TestManagerCanceledStartupReapsChild(t *testing.T) {
	cfg := testManagerConfig(t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := cfg.Probe
	var observedPID int
	cfg.Probe = func(ctx context.Context) error {
		if err := probe(ctx); err != nil {
			return err
		}
		b, err := os.ReadFile(New(cfg).path(".pid"))
		if err != nil {
			return err
		}
		observedPID, err = strconv.Atoi(strings.TrimSpace(string(b)))
		cancel()
		return err
	}
	m := New(cfg)
	if _, err := m.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start: %v", err)
	}
	if observedPID == 0 || !processExited(observedPID) {
		t.Fatalf("launched child wasn't reaped: pid=%d", observedPID)
	}
	if _, err := os.Stat(m.path(".pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped child marker not cleaned up: %v", err)
	}
}
