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
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

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
