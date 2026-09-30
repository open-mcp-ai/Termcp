// Package daemon manages the detached background termcp instance behind the
// `termcp daemon` subcommands: spawning it, finding the instance that answers
// a given HTTP endpoint, asking it to stop, and the idle countdown that ends a
// daemon on its own. Discovery is deliberately HTTP-only — an instance is
// found because it answers at its endpoint, whichever data directory or
// platform started it, and whether or not it was started as a daemon.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/config"
)

// EnvChild marks a spawned process as a daemon child: it turns on the idle
// countdown and identifies the instance as a daemon at GET /api/daemon.
const EnvChild = "TERMCP_DAEMON_CHILD"

const (
	logFileName  = "termcp.log"
	probeTimeout = 1500 * time.Millisecond
	readyTimeout = 15 * time.Second
	stopTimeout  = 10 * time.Second

	apiDaemonPath     = "/api/daemon"
	apiDaemonStopPath = "/api/daemon/stop"
)

// LogPath returns the daemon log location inside a data directory.
func LogPath(dataDir string) string { return filepath.Join(dataDir, logFileName) }

// ConnectHost normalizes a configured bind host into an address clients can
// dial: wildcard binds answer on loopback.
func ConnectHost(host string) string {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return host
}

// BaseURL is the origin an instance bound to host:port is reachable at.
func BaseURL(host string, port int) string {
	return "http://" + net.JoinHostPort(ConnectHost(host), strconv.Itoa(port))
}

// Mark is the banner the public docs start with; it distinguishes a termcp
// instance from whatever else might hold the port.
const apiMark = "Termcp"

// ProbeHeader marks every request a management probe makes — the daemon probe
// and the /api.md fingerprint it falls back to. An instance counts its own
// probe traffic as no activity at all (see internal/daemon/idle.go), so the
// bookkeeping of "which requests are a probe" stays here, with the code that
// sends them, instead of being guessed from paths on the server side.
const ProbeHeader = "X-Termcp-Probe"

// markProbe tags a request as probe traffic. Both the daemon probe and the
// credential-free docs fingerprint are pure liveness questions whose answer
// must never extend the instance's life: a status query that keeps a
// short-countdown instance alive is the opposite of what it reports. Only the
// probe's own requests carry the tag — no other client does.
func markProbe(req *http.Request) *http.Request {
	req.Header.Set(ProbeHeader, "1")
	return req
}

// IsProbe reports whether a request came from a termcp management probe and
// must therefore not count as activity for an idle countdown. The /api/daemon
// route check keeps the guarantee for older binaries and for any caller that
// asks the probe path directly without the header.
func IsProbe(r *http.Request) bool {
	if r.Header.Get(ProbeHeader) != "" {
		return true
	}
	return r.Method == http.MethodGet && r.URL.Path == apiDaemonPath
}

// IsServing reports whether a termcp instance answers HTTP on host:port. It
// reads the credential-free /api.md, so no token is involved and nothing but
// documentation state is touched.
func IsServing(ctx context.Context, host string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL(host, port)+"/api.md", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(markProbe(req))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return false
	}
	return bytes.Contains(head, []byte(apiMark))
}

// PortReachable reports whether something accepts TCP connections on
// host:port. It is deliberately HTTP-free, so a liveness poll never counts as
// activity for the instance's idle timer.
func PortReachable(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ConnectHost(host), strconv.Itoa(port)), probeTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// InstanceInfo is what an instance reports about itself at GET /api/daemon.
// Every management action is built on this probe, so pid, version, daemon
// identity and the effective idle countdown all arrive over HTTP.
type InstanceInfo struct {
	Daemon    bool   `json:"daemon"`
	PID       int    `json:"pid"`
	Version   string `json:"version"`
	StartedAt string `json:"started_at"`
	Log       string `json:"log"`
	// IdleTimeoutMS is the instance's effective idle countdown in milliseconds,
	// 0 when it never exits on its own. It is reported so a client that wants to
	// count as activity (the stdio bridge) can ping faster than the *actual*
	// countdown instead of assuming the default: an instance started with
	// --idle-timeout 6s would otherwise die under a bridge that pings every 10s.
	IdleTimeoutMS int64 `json:"idle_timeout_ms"`
}

// IdleTimeout is the reported countdown as a duration; 0 means the instance
// never exits on its own.
func (i InstanceInfo) IdleTimeout() time.Duration {
	return time.Duration(i.IdleTimeoutMS) * time.Millisecond
}

// EndpointReport is the outcome of InspectEndpoint.
type EndpointReport struct {
	Termcp       bool         // a Termcp instance answers at the endpoint
	Unauthorized bool         // ...but the probe was rejected (HTTP 401/403)
	Info         InstanceInfo // zero when the probe was not answered
}

// Credential is the bearer value this client can present: the plaintext
// token, or — when only a hash is configured — the hash string itself, which
// a hash-configured instance accepts.
func Credential(cfg *config.Config) string {
	if cfg.AuthToken != "" {
		return cfg.AuthToken
	}
	return cfg.AuthHash
}

// InspectEndpoint asks the endpoint what instance answers there, purely over
// HTTP: GET /api/daemon, with the configured credential (see Credential) when
// there is one — never through process tables. When the probe is rejected, or
// the instance predates it, the credential-free /api.md fingerprint still
// identifies a Termcp instance; without a usable credential its details
// simply stay unknown.
func InspectEndpoint(ctx context.Context, host string, port int, credential string) EndpointReport {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL(host, port)+apiDaemonPath, nil)
	if err != nil {
		return EndpointReport{}
	}
	markProbe(req)
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return EndpointReport{}
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		resp.Body.Close()
		if IsServing(ctx, host, port) {
			return EndpointReport{Termcp: true, Unauthorized: true}
		}
		return EndpointReport{}
	case http.StatusOK:
		var info InstanceInfo
		err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info)
		resp.Body.Close()
		if err == nil && info.PID > 0 {
			return EndpointReport{Termcp: true, Info: info}
		}
	}
	resp.Body.Close()
	// Not the probe payload; the public docs fingerprint still proves a termcp
	// instance — one that predates the probe.
	if IsServing(ctx, host, port) {
		return EndpointReport{Termcp: true}
	}
	return EndpointReport{}
}

// StopResult reports what a stop request found and did.
type StopResult struct {
	Stopped   bool // the instance acknowledged and its endpoint stopped answering
	PID       int  // when the probe revealed it
	NotDaemon bool // an instance answered, but it has no daemon lifecycle to stop
}

// StopEndpoint stops the daemon instance serving host:port. The stop itself
// goes over HTTP — POST /api/daemon/stop, answered before the instance begins
// shutting down gracefully — and then the call waits for the endpoint to stop
// answering. An instance started manually (not as a daemon) has no stop route
// and is reported through NotDaemon instead of being hunted down as a process.
func StopEndpoint(ctx context.Context, host string, port int, credential string) (StopResult, error) {
	rep := InspectEndpoint(ctx, host, port, credential)
	if rep.Unauthorized {
		return StopResult{}, fmt.Errorf("the instance at %s requires authentication: pass its token with --auth-token (or --auth-hash), or stop it where it was started", BaseURL(host, port))
	}
	if !rep.Termcp {
		return StopResult{}, nil
	}
	if !rep.Info.Daemon {
		return StopResult{NotDaemon: true, PID: rep.Info.PID}, nil
	}
	url := BaseURL(host, port) + apiDaemonStopPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return StopResult{}, err
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return StopResult{}, fmt.Errorf("ask the instance to stop: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return StopResult{}, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if !PortReachable(host, port) {
			return StopResult{Stopped: true, PID: rep.Info.PID}, nil
		}
		select {
		case <-ctx.Done():
			return StopResult{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return StopResult{}, fmt.Errorf("the instance at %s did not stop within %s", BaseURL(host, port), stopTimeout)
}

// EnsureResult reports what Ensure found or started.
type EnsureResult struct {
	Info         InstanceInfo
	Spawned      bool // true when this call launched the instance
	Unauthorized bool // the endpoint answered but rejected the probe: details unknown
}

// Ensure makes sure an instance serves the configured endpoint. Whatever
// answers there is reused — including an instance started manually, or from a
// different data directory on another platform; nothing is looked up through
// the process table. Only when nothing answers is a detached child spawned,
// with the same resolved flags as the caller and the daemon marker that gives
// it the idle countdown. idleTimeout is the literal --idle-timeout value to
// hand the child: the caller picks it per action ("30s" for the stdio action,
// "0" — no countdown — for a plain start; "" would fall back to the child's
// daemon default).
func Ensure(ctx context.Context, cfg *config.Config, idleTimeout string) (EnsureResult, error) {
	url := BaseURL(cfg.Host, cfg.Port)
	if rep := InspectEndpoint(ctx, cfg.Host, cfg.Port, Credential(cfg)); rep.Termcp {
		return EnsureResult{Info: rep.Info, Unauthorized: rep.Unauthorized}, nil
	}
	// Something that is not termcp is holding the port; a spawned child would
	// just die on bind.
	if PortReachable(cfg.Host, cfg.Port) {
		return EnsureResult{}, fmt.Errorf("something other than termcp holds %s; stop it or choose another --host/--port", url)
	}

	exe, err := os.Executable()
	if err != nil {
		return EnsureResult{}, fmt.Errorf("locate executable: %w", err)
	}
	logPath := LogPath(cfg.DataDir)
	pid, err := spawn(exe, ChildArgs(cfg, idleTimeout), childEnv(cfg), logPath)
	if err != nil {
		return EnsureResult{}, err
	}

	deadline := time.Now().Add(readyTimeout)
	for {
		rep := InspectEndpoint(ctx, cfg.Host, cfg.Port, Credential(cfg))
		if rep.Termcp {
			// A concurrent front-end may have won the race to serve this
			// endpoint; its instance is just as good as ours.
			return EnsureResult{Info: rep.Info, Spawned: rep.Info.PID == 0 || rep.Info.PID == pid, Unauthorized: rep.Unauthorized}, nil
		}
		if !ProcessAlive(pid) {
			return EnsureResult{}, fmt.Errorf("the daemon exited before serving %s; see %s", url, logPath)
		}
		if time.Now().After(deadline) {
			stopQuietly(pid)
			return EnsureResult{}, fmt.Errorf("the daemon did not become ready at %s within %s; see %s", url, readyTimeout, logPath)
		}
		select {
		case <-ctx.Done():
			stopQuietly(pid)
			return EnsureResult{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ChildArgs builds the command-line flags for the detached instance. The
// parent rebuilds flags from its resolved configuration instead of replaying
// argv, so the child gets a complete, explicit setup. Secrets are NOT included:
// they travel via the environment (childEnv), never argv.
func ChildArgs(cfg *config.Config, idleTimeout string) []string {
	args := []string{
		"--host", cfg.Host,
		"--port", strconv.Itoa(cfg.Port),
		"--data-dir", cfg.DataDir,
	}
	if cfg.AssetsDir != "" {
		args = append(args, "--assets", cfg.AssetsDir)
	}
	if cfg.LogLevel != "" {
		args = append(args, "--log-level", cfg.LogLevel)
	}
	if cfg.NoInternal {
		args = append(args, "--no-internal")
	}
	if cfg.MCPManageSSHConfigs {
		args = append(args, "--mcp-manage-ssh-configs")
	}
	if cfg.MCPDeferTools {
		args = append(args, "--mcp-defer-tools")
	}
	if idleTimeout != "" {
		args = append(args, "--idle-timeout", idleTimeout)
	}
	return args
}

// childEnv is the environment for the detached instance: the inherited
// environment with daemon markers and credentials (re)appended, so that flag
// values resolved by the parent win over any inherited TERMCP_* variables.
func childEnv(cfg *config.Config) []string {
	env := filterEnv(os.Environ(), []string{
		EnvChild + "=",
		config.EnvAuthToken + "=",
		config.EnvAuthHash + "=",
		config.EnvDisableAuth + "=",
	})
	env = append(env, EnvChild+"=1")
	if cfg.AuthToken != "" {
		env = append(env, config.EnvAuthToken+"="+cfg.AuthToken)
	}
	if cfg.AuthHash != "" {
		env = append(env, config.EnvAuthHash+"="+cfg.AuthHash)
	}
	if cfg.DisableAuth {
		env = append(env, config.EnvDisableAuth+"=1")
	}
	return env
}

// filterEnv drops entries with any of the given name prefixes (matched
// case-insensitively: Windows environment blocks are case-insensitive).
func filterEnv(env, prefixes []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		skip := false
		for _, p := range prefixes {
			if len(kv) >= len(p) && strings.EqualFold(kv[:len(p)], p) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

// spawn starts the detached instance: stdin from the null device, stdout and
// stderr appended to the daemon log, and no wait — the child outlives this
// process.
func spawn(exe string, args, env []string, logPath string) (int, error) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devnull.Close()
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", exe, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // never wait: the child is detached
	return pid, nil
}

func stopQuietly(pid int) {
	_ = terminate(pid)
}
