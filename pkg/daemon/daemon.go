// Package daemon manages the lifecycle of a background process: starting it detached,
// writing its streams to a log file, waiting for its readiness probe, checking its
// status, and stopping it cleanly across Unix and Windows (issue #309).
//
// NOTHING HERE KNOWS WHAT THE SERVICE DOES. The caller supplies the flags (`BuildArgs`),
// the environment (`BuildEnv`), the readiness question (`Probe`), and optionally a
// graceful stop (`Stop` — `HTTPStop` is the one that works on Windows too, where a
// detached process has no signal it can receive).
//
// There are two ways to use this package, matching the two ways services discover their
// background instances:
//
//   - A caller with a pidfile uses Manager (`New`, `Start`, `Stop`, `Status`). The
//     pidfile lives under DataDir as `<Name>.pid`, and stale pidfiles left by a crashed
//     process are cleaned up automatically.
//   - A caller that discovers its instance by HTTP endpoint instead of by pidfile (like
//     termcp itself) uses the building blocks directly: `Spawn`, `IdleWatcher`,
//     `PortReachable`, `IsServing`, and `github.com/open-mcp-ai/termcp/pkg/detach`.
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
	"time"

	"github.com/open-mcp-ai/termcp/pkg/detach"
)

const (
	defaultReadyWait = 15 * time.Second
	defaultStopWait  = 5 * time.Second
	pollInterval     = 50 * time.Millisecond
)

// Config describes one background service.
//
// Name and DataDir are the only fields with no sensible default when using Manager: Name
// picks the pidfile and default log file (`<Name>.pid`, `<Name>.log`), and DataDir is
// where both live.
type Config struct {
	// Name identifies the service (used for `<Name>.pid` and the default log file name).
	Name string
	// DataDir holds the pidfile and the default log file. Created on Start if absent.
	DataDir string
	// LogFile overrides `<DataDir>/<Name>.log` for stdout and stderr.
	LogFile string
	// Executable overrides os.Executable() when spawning the child.
	Executable string
	// BuildArgs builds the child process arguments. Nil runs with no arguments.
	BuildArgs func() []string
	// BuildEnv builds the child environment. Nil inherits os.Environ().
	BuildEnv func() []string
	// Probe reports whether the started child is ready (nil = ready). When nil, Start
	// returns as soon as the child process is alive after spawning.
	Probe func(ctx context.Context) error
	// Stop asks a running instance to exit gracefully (for example HTTPStop). On
	// Windows a detached process cannot receive SIGTERM, so setting Stop is what makes
	// Manager.Stop graceful across platforms; when Stop is nil or fails, Manager.Stop
	// falls back to detach.Terminate (SIGTERM on Unix, TerminateProcess on Windows).
	Stop func(ctx context.Context) error
	// ReadyWait bounds how long Start waits for Probe after spawning. Default 15s.
	ReadyWait time.Duration
	// StopWait bounds how long Stop waits for the pid to exit after Stop/Terminate.
	// Default 5s.
	StopWait time.Duration
}

// Instance describes a started or already-running background process.
type Instance struct {
	PID     int
	LogFile string
	Reused  bool
}

// Status is what Manager.Status reports about a managed instance.
type Status struct {
	Running bool
	PID     int
	Ready   bool
	LogFile string
	// ProbeErr carries why Probe failed when Running is true and Ready is false.
	ProbeErr error
}

// Manager drives a pidfile-backed daemon described by Config.
type Manager struct {
	cfg Config
	// startTok serialises Start/Stop without blocking callers that cannot wait: a
	// one-slot channel token rather than a mutex so ctx cancellation exits immediately
	// (AGENTS.md concurrency rule — never hold a mutex across process spawn or network
	// probes).
	tok chan struct{}
}

// New constructs a Manager for cfg.
func New(cfg Config) *Manager {
	if cfg.Name == "" {
		cfg.Name = "daemon"
	}
	if cfg.ReadyWait <= 0 {
		cfg.ReadyWait = defaultReadyWait
	}
	if cfg.StopWait <= 0 {
		cfg.StopWait = defaultStopWait
	}
	tok := make(chan struct{}, 1)
	tok <- struct{}{}
	return &Manager{cfg: cfg, tok: tok}
}

func (m *Manager) acquire(ctx context.Context) error {
	select {
	case <-m.tok:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release() {
	m.tok <- struct{}{}
}

func (m *Manager) pidPath() string {
	return filepath.Join(m.cfg.DataDir, m.cfg.Name+".pid")
}

func (m *Manager) logPath() string {
	if m.cfg.LogFile != "" {
		return m.cfg.LogFile
	}
	return filepath.Join(m.cfg.DataDir, m.cfg.Name+".log")
}

// readLivePID reads the pidfile and verifies the process is actually alive. A stale
// pidfile (process gone or unparseable) is removed so the caller sees "not running".
func (m *Manager) readLivePID() int {
	raw, err := os.ReadFile(m.pidPath())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 || !detach.ProcessAlive(pid) {
		_ = os.Remove(m.pidPath())
		return 0
	}
	return pid
}

// Start ensures an instance is running and ready.
//
// ALREADY RUNNING IS REUSED, NOT REFUSED: if the pidfile names a live process whose
// Probe succeeds (or Probe is nil), Start returns it with Reused=true. A child spawned
// here that fails Probe before ReadyWait expires is terminated before Start returns, so
// a failed Start leaves no orphan behind.
func (m *Manager) Start(ctx context.Context) (*Instance, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer m.release()

	if pid := m.readLivePID(); pid > 0 {
		if m.cfg.Probe == nil || m.cfg.Probe(ctx) == nil {
			return &Instance{PID: pid, LogFile: m.logPath(), Reused: true}, nil
		}
		return nil, fmt.Errorf("%s: process %d is running but readiness probe failed", m.cfg.Name, pid)
	}

	if err := os.MkdirAll(m.cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", m.cfg.DataDir, err)
	}
	exe := m.cfg.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate executable: %w", err)
		}
	}
	var args []string
	if m.cfg.BuildArgs != nil {
		args = m.cfg.BuildArgs()
	}
	var env []string
	if m.cfg.BuildEnv != nil {
		env = m.cfg.BuildEnv()
	}

	logPath := m.logPath()
	pid, err := Spawn(exe, args, env, logPath)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(m.pidPath(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = detach.Terminate(pid)
		return nil, fmt.Errorf("write pidfile: %w", err)
	}

	if m.cfg.Probe != nil {
		if err := m.waitReady(ctx, pid); err != nil {
			// A child that never answered its own probe is cleaned up rather than left
			// holding the port or data dir.
			_ = detach.Terminate(pid)
			_ = os.Remove(m.pidPath())
			return nil, fmt.Errorf("%s (pid %d) did not become ready: %w (see %s)", m.cfg.Name, pid, err, logPath)
		}
	}
	return &Instance{PID: pid, LogFile: logPath}, nil
}

func (m *Manager) waitReady(ctx context.Context, pid int) error {
	deadline := time.Now().Add(m.cfg.ReadyWait)
	var lastErr error
	for {
		if !detach.ProcessAlive(pid) {
			if lastErr != nil {
				return fmt.Errorf("exited early (last probe: %w)", lastErr)
			}
			return errors.New("exited before probe succeeded")
		}
		if err := m.cfg.Probe(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return errors.New("ready timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Stop stops the running instance (if any) and removes the pidfile.
//
// GRACEFUL FIRST, SIGNAL SECOND: if Config.Stop is set, Stop calls it and waits up to
// StopWait for the process to exit; only if Stop is unset, fails, or the process stays
// alive past the window does it fall back to detach.Terminate. On Windows that makes an
// HTTP shutdown the default rather than a special case.
func (m *Manager) Stop(ctx context.Context) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()

	pid := m.readLivePID()
	if pid == 0 {
		return nil
	}

	if m.cfg.Stop != nil {
		if err := m.cfg.Stop(ctx); err == nil {
			if m.waitExit(ctx, pid, m.cfg.StopWait) {
				_ = os.Remove(m.pidPath())
				return nil
			}
		}
	}

	if err := detach.Terminate(pid); err != nil && detach.ProcessAlive(pid) {
		return err
	}
	if !m.waitExit(ctx, pid, m.cfg.StopWait) {
		_ = detach.Kill(pid)
		_ = m.waitExit(ctx, pid, time.Second)
	}
	_ = os.Remove(m.pidPath())
	return nil
}

func (m *Manager) waitExit(ctx context.Context, pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for detach.ProcessAlive(pid) {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return !detach.ProcessAlive(pid)
		}
		select {
		case <-ctx.Done():
			return !detach.ProcessAlive(pid)
		case <-time.After(pollInterval):
		}
	}
	return true
}

// Status reports whether the managed instance is running and passes Probe.
func (m *Manager) Status(ctx context.Context) Status {
	pid := m.readLivePID()
	if pid == 0 {
		return Status{LogFile: m.logPath()}
	}
	st := Status{Running: true, PID: pid, LogFile: m.logPath(), Ready: true}
	if m.cfg.Probe != nil {
		if err := m.cfg.Probe(ctx); err != nil {
			st.Ready = false
			st.ProbeErr = err
		}
	}
	return st
}

// Spawn starts exe detached with args and env, redirecting stdin from the null device and
// stdout/stderr appended to logPath.
//
// It never waits on the child: the process handle is released immediately so the child
// outlives the caller.
func Spawn(exe string, args, env []string, logPath string) (int, error) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devnull.Close()
	if dir := filepath.Dir(logPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return 0, fmt.Errorf("create log dir %s: %w", dir, err)
		}
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", exe, err)
	}
	pid := cmd.Process.Pid
	// REAPED IN THE BACKGROUND, NOT RELEASED (found by this package's own test).
	//
	// A child that exits while this process is still running stays a ZOMBIE until it is
	// waited for, and `kill(pid, 0)` — the liveness question every function in this
	// package asks — SUCCEEDS for a zombie. So a released child made Stop report "still
	// running" forever, and the next Start refuse to reuse a process that had already
	// exited. Waiting in a goroutine reaps it the moment it ends, without making the
	// caller wait: the pid stays correct while the child lives, and turns correctly dead
	// when it does not.
	//
	// A caller that exits immediately (a CLI spawning its own daemon) loses nothing: the
	// goroutine dies with the process, and the orphaned child is reaped by init.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}
