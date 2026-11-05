// Package daemon manages the detached background termcp instance behind the
// `termcp daemon` subcommands: spawning it, finding the instance that answers
// a given HTTP endpoint, asking it to stop, and the idle countdown that ends a
// daemon on its own. Discovery is deliberately HTTP-only — an instance is
// found because it answers at its endpoint, whichever data directory or
// platform started it, and whether or not it was started as a daemon.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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

// ManagementURL validates and normalizes the origin used for daemon
// management. Cleartext HTTP is accepted only for literal loopback endpoints;
// an explicitly supplied URL may use HTTPS for a remote endpoint.
func ManagementURL(host string, port int, explicit string) (string, error) {
	raw := strings.TrimSpace(explicit)
	if raw == "" {
		raw = BaseURL(host, port)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("%w: expected an http(s) origin", ErrUnsafeManagementURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: expected an http(s) origin", ErrUnsafeManagementURL)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%w: daemon management requires an origin without a path, query, or fragment", ErrUnsafeManagementURL)
	}
	if u.Scheme == "http" && !loopbackManagementHost(u.Hostname()) {
		return "", fmt.Errorf("%w: refusing to send local Admin credentials over cleartext HTTP to %s; use an explicit https:// origin for a remote endpoint", ErrUnsafeManagementURL, u.Host)
	}
	return u.Scheme + "://" + u.Host, nil
}

func loopbackManagementHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
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

// ErrAuthenticationRequired reports that the daemon management endpoint
// rejected the supplied credential.
var ErrAuthenticationRequired = errors.New("daemon authentication required")

// ErrUnsafeManagementURL reports an endpoint that cannot safely receive local
// Admin credentials.
var ErrUnsafeManagementURL = errors.New("unsafe daemon management URL")

// ManagementCredential is the legacy shared credential used to inspect or stop
// the daemon: the plaintext token, or — when only a hash is configured — the
// hash string itself. Managed MCP tokens are deliberately excluded because
// they only grant MCP access.
func ManagementCredential(cfg *config.Config) string {
	if cfg.AuthToken != "" {
		return cfg.AuthToken
	}
	return cfg.AuthHash
}

// BridgeCredential selects the credential a stdio MCP bridge presents. A
// managed MCP token takes precedence there; daemon management never does.
func BridgeCredential(cfg *config.Config) string {
	if cfg.MCPToken != "" {
		return cfg.MCPToken
	}
	return ManagementCredential(cfg)
}

type endpointAuthorizer func(*http.Request)

var errManagementRedirect = errors.New("daemon management redirect refused")

func managementHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errManagementRedirect
		},
	}
}

func managementHostPort(baseURL string) (string, int, bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return "", 0, false
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if raw := u.Port(); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return "", 0, false
		}
		port = parsed
	}
	return u.Hostname(), port, true
}

func bearerAuthorizer(credential string) endpointAuthorizer {
	return func(req *http.Request) {
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
	}
}

func sessionAuthorizer(session ManagedAdminSession) endpointAuthorizer {
	return func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: managedSessionCookie, Value: session.cookie})
		req.Header.Set("X-Termcp-CSRF", session.csrf)
	}
}

// InspectEndpoint asks the endpoint what instance answers there, purely over
// HTTP: GET /api/daemon, with the configured management credential when there
// is one — never through process tables. When the probe is rejected, or the
// instance predates it, the credential-free /api.md fingerprint still
// identifies a Termcp instance; without a usable credential its details simply
// stay unknown.
func InspectEndpoint(ctx context.Context, host string, port int, credential string) EndpointReport {
	return InspectEndpointURL(ctx, BaseURL(host, port), credential)
}

// InspectEndpointURL inspects an already-validated management origin.
func InspectEndpointURL(ctx context.Context, baseURL, credential string) EndpointReport {
	baseURL, err := ManagementURL("", 0, baseURL)
	if err != nil {
		return EndpointReport{}
	}
	return inspectEndpoint(ctx, baseURL, bearerAuthorizer(credential))
}

// InspectEndpointWithSession inspects a managed instance with an Admin session.
func InspectEndpointWithSession(ctx context.Context, host string, port int, session ManagedAdminSession) EndpointReport {
	return InspectEndpointURLWithSession(ctx, BaseURL(host, port), session)
}

// InspectEndpointURLWithSession inspects a managed instance at an
// already-validated management origin.
func InspectEndpointURLWithSession(ctx context.Context, baseURL string, session ManagedAdminSession) EndpointReport {
	baseURL, err := ManagementURL("", 0, baseURL)
	if err != nil {
		return EndpointReport{}
	}
	return inspectEndpoint(ctx, baseURL, sessionAuthorizer(session))
}

func inspectEndpoint(ctx context.Context, baseURL string, authorize endpointAuthorizer) EndpointReport {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+apiDaemonPath, nil)
	if err != nil {
		return EndpointReport{}
	}
	markProbe(req)
	if authorize != nil {
		authorize(req)
	}
	resp, err := managementHTTPClient().Do(req)
	if err != nil {
		return EndpointReport{}
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusLocked:
		resp.Body.Close()
		if IsServingURL(ctx, baseURL) {
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
	if IsServingURL(ctx, baseURL) {
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
	return StopEndpointURL(ctx, BaseURL(host, port), credential)
}

// StopEndpointURL stops an instance at an already-validated management origin.
func StopEndpointURL(ctx context.Context, baseURL, credential string) (StopResult, error) {
	baseURL, err := ManagementURL("", 0, baseURL)
	if err != nil {
		return StopResult{}, err
	}
	return stopEndpoint(ctx, baseURL, bearerAuthorizer(credential))
}

// StopEndpointWithSession stops a managed instance using an authenticated
// Admin browser session.
func StopEndpointWithSession(ctx context.Context, host string, port int, session ManagedAdminSession) (StopResult, error) {
	return StopEndpointURLWithSession(ctx, BaseURL(host, port), session)
}

// StopEndpointURLWithSession stops an instance at an already-validated
// management origin using an authenticated Admin browser session.
func StopEndpointURLWithSession(ctx context.Context, baseURL string, session ManagedAdminSession) (StopResult, error) {
	baseURL, err := ManagementURL("", 0, baseURL)
	if err != nil {
		return StopResult{}, err
	}
	return stopEndpoint(ctx, baseURL, sessionAuthorizer(session))
}

func stopEndpoint(ctx context.Context, baseURL string, authorize endpointAuthorizer) (StopResult, error) {
	rep := inspectEndpoint(ctx, baseURL, authorize)
	if rep.Unauthorized {
		return StopResult{}, fmt.Errorf("%w: the instance at %s requires authentication", ErrAuthenticationRequired, baseURL)
	}
	if !rep.Termcp {
		return StopResult{}, nil
	}
	if !rep.Info.Daemon {
		return StopResult{NotDaemon: true, PID: rep.Info.PID}, nil
	}
	stopURL := baseURL + apiDaemonStopPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stopURL, nil)
	if err != nil {
		return StopResult{}, err
	}
	if authorize != nil {
		authorize(req)
	}
	resp, err := managementHTTPClient().Do(req)
	if err != nil {
		return StopResult{}, fmt.Errorf("ask the instance to stop: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return StopResult{}, fmt.Errorf("%s answered %s", stopURL, resp.Status)
	}
	host, port, ok := managementHostPort(baseURL)
	if !ok {
		return StopResult{}, fmt.Errorf("%w: invalid management origin", ErrUnsafeManagementURL)
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
	return StopResult{}, fmt.Errorf("the instance at %s did not stop within %s", baseURL, stopTimeout)
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
	if rep := InspectEndpoint(ctx, cfg.Host, cfg.Port, ManagementCredential(cfg)); rep.Termcp {
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
		rep := InspectEndpoint(ctx, cfg.Host, cfg.Port, ManagementCredential(cfg))
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
