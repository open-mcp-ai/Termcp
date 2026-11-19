package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/fsutil"
	"github.com/open-mcp-ai/termcp/pkg/detach"
)

// ErrNotRunning is returned by Probe only for a definitively absent endpoint.
// Timeouts, identity mismatches and authorization failures are not absence.
var ErrNotRunning = errors.New("daemon endpoint is absent")

// ErrBusy means another invocation is inspecting or changing this instance.
// No action was taken; the caller can retry.
var ErrBusy = errors.New("daemon lifecycle operation in progress")

// Config identifies one endpoint and one set of files in a private local
// DataDir. All invocations for this instance must use the same configuration.
type Config struct {
	Name    string // single filename component
	DataDir string // required, writable only by the trusted caller
	LogFile string // defaults to DataDir/Name.log

	Executable string          // defaults to the current executable
	BuildArgs  func() []string // arguments after argv[0]; nil means no arguments
	BuildEnv   func() []string // complete child environment; nil inherits

	// Probe checks readiness AND the configured instance identity. Return nil
	// for that instance only, ErrNotRunning for absence, another error for
	// any other result. RequestStop must independently validate identity at
	// the stop endpoint; a preceding Probe is not a credential. It must not
	// signal a PID loaded from disk. Both callbacks must honor ctx; builders
	// must return promptly. Children must not fork or start another daemon.
	Probe       func(ctx context.Context) error
	RequestStop func(ctx context.Context) error // optional; required to stop a live instance
	ReadyWait   time.Duration                   // operation timeout; zero defaults to 10s
}

// Instance is a snapshot of the recorded child; PID is informational only.
type Instance struct{ PID int }

// Status distinguishes a known stopped endpoint (Running=false, Err=nil)
// from an indeterminate state (Err!=nil). PID may be stale when Err!=nil.
type Status struct {
	PID     int
	Running bool
	Err     error
}

// Manager controls the instance named in Config, independently of the
// lifetime of this Go value or the CLI invocation that created it.
type Manager struct{ cfg Config }

// New constructs a manager. Configuration and filesystem errors are reported
// by its operations; constructing a manager has no side effects.
func New(cfg Config) *Manager {
	if cfg.ReadyWait == 0 {
		cfg.ReadyWait = 10 * time.Second
	}
	return &Manager{cfg}
}

func (m *Manager) path(suffix string) string {
	return filepath.Join(m.cfg.DataDir, m.cfg.Name+suffix)
}

// begin claims a non-blocking OS-backed lease. Never unlink this lock file:
// doing so would let another process lock a different inode at the same path.
// Competing calls (including reentrant callbacks) return ErrBusy immediately.
func (m *Manager) begin(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c := m.cfg
	if c.DataDir == "" || len(c.Name) > 200 || !filepath.IsLocal(c.Name) || strings.ContainsAny(c.Name, `/\:`) ||
		c.Name == "." || c.Probe == nil || c.ReadyWait <= 0 {
		return nil, nil, errors.New("daemon needs a private DataDir, a single-component Name, a Probe and a positive ReadyWait")
	}
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(m.path(".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err := tryManagerLock(f); err != nil {
		f.Close()
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.ReadyWait)
	return ctx, func() { cancel(); f.Close() }, nil
}

func (m *Manager) inspect(ctx context.Context) Status {
	var pid int
	b, err := os.ReadFile(m.path(".pid"))
	switch {
	case err == nil:
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 || uint64(pid) > math.MaxUint32 || runtime.GOOS != "windows" && uint64(pid) > math.MaxInt32 {
			return Status{Err: fmt.Errorf("invalid or interrupted launch in %s; verify the child and endpoint before removing this file", m.path(".pid"))}
		}
	case !errors.Is(err, os.ErrNotExist):
		return Status{Err: err}
	}
	err = m.cfg.Probe(ctx)
	switch {
	case ctx.Err() != nil:
		return Status{PID: pid, Err: ctx.Err()}
	case err == nil && pid != 0 && !processExited(pid):
		return Status{PID: pid, Running: true}
	case err == nil:
		return Status{PID: pid, Err: errors.New("endpoint is serving without a live recorded child")}
	case !errors.Is(err, ErrNotRunning):
		return Status{PID: pid, Err: err}
	case pid != 0 && !processExited(pid):
		return Status{PID: pid, Err: errors.New("recorded PID is live but its endpoint is absent")}
	default:
		return Status{}
	}
}

// Status inspects the pidfile and probes the instance without starting or
// stopping it. A transient operation conflict returns ErrBusy.
func (m *Manager) Status(ctx context.Context) Status {
	ctx, release, err := m.begin(ctx)
	if err != nil {
		return Status{Err: err}
	}
	defer release()
	return m.inspect(ctx)
}

// Start returns an already running managed instance, or launches a child and
// waits for readiness. Until readiness, cleanup uses only the process handle
// from this invocation. A parent crash between launch and saving the PID
// leaves a "starting" marker rather than risking a duplicate launch.
func (m *Manager) Start(ctx context.Context) (*Instance, error) {
	ctx, release, err := m.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	status := m.inspect(ctx)
	if status.Err != nil {
		return nil, status.Err
	}
	if status.Running {
		return &Instance{status.PID}, nil
	}
	exe := m.cfg.Executable
	if exe == "" {
		exe, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	var args []string
	if m.cfg.BuildArgs != nil {
		args = m.cfg.BuildArgs()
	}
	cmd := exec.Command(exe, args...) // the child must outlive the calling context
	if m.cfg.BuildEnv != nil {
		cmd.Env = m.cfg.BuildEnv()
	}
	logPath := m.cfg.LogFile
	if logPath == "" {
		logPath = m.path(".log")
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log // nil Stdin reads the null device
	detach.Detach(cmd)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fsutil.WriteFileAtomic(m.path(".pid"), []byte("starting\n")); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, m.clearPID())
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait() // reap children of long-lived embedders, including on Unix
		close(done)
	}()
	if err = fsutil.WriteFileAtomic(m.path(".pid"), []byte(strconv.Itoa(pid)+"\n")); err == nil {
		err = waitFor(ctx, func() (bool, error) {
			probeErr := m.cfg.Probe(ctx)
			select {
			case <-done:
				return false, fmt.Errorf("daemon exited before readiness; see %s", logPath)
			default:
			}
			if probeErr != nil && !errors.Is(probeErr, ErrNotRunning) {
				return false, probeErr
			}
			return probeErr == nil, nil
		})
	}
	if err != nil {
		// Never reopen or signal a PID read from disk. Keep the marker if the
		// owned child's exit cannot be confirmed, so later starts fail closed.
		killErr := cmd.Process.Kill()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return nil, errors.Join(err, killErr)
		}
		// Even when readiness expired, allow a short, separate budget to
		// reap the killed child and clean up. Never wait indefinitely.
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-done:
			return nil, errors.Join(err, m.clearPID())
		case <-timer.C:
			return nil, errors.Join(err, errors.New("child exit unconfirmed; retained launch marker"))
		}
	}
	return &Instance{pid}, nil
}

func (m *Manager) clearPID() error {
	err := os.Remove(m.path(".pid"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Stop requests shutdown through the caller's instance-specific endpoint
// and confirms that both the endpoint and process exited. A child can close
// a listener during shutdown and continue working. It never signals a saved PID.
// Missing stop support, probe failures and ambiguous state leave the pidfile
// untouched. An already stopped instance succeeds without a callback.
func (m *Manager) Stop(ctx context.Context) error {
	ctx, release, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	status := m.inspect(ctx)
	if status.Err != nil {
		return status.Err
	}
	if !status.Running {
		return m.clearPID()
	}
	if m.cfg.RequestStop == nil {
		return errors.New("running daemon needs an identity-checked RequestStop callback")
	}
	if err := m.cfg.RequestStop(ctx); err != nil {
		return err
	}
	err = waitFor(ctx, func() (bool, error) {
		err := m.cfg.Probe(ctx)
		if errors.Is(err, ErrNotRunning) {
			return processExited(status.PID), nil
		}
		return false, err
	})
	if err != nil {
		return err
	}
	return m.clearPID()
}

func waitFor(ctx context.Context, check func() (bool, error)) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := check()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ok || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
