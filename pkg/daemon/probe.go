package daemon

// LIVENESS, ASKED TWO WAYS, FROM ONE PLACE (issue #309).
//
// A background instance is found by asking it, never by reading a process table: the
// process may be another user's, on another mount namespace, or started by a previous
// boot. So there are exactly two probes here, and the difference between them is the
// whole point:
//
//	PortReachable  something accepts TCP. Cheap, HTTP-free, and it must not count as
//	               activity — a poll that keeps an idle instance alive is a poll that
//	               defeats the feature it is checking.
//	IsServing      the thing on that port is the program we are looking for, proved by
//	               a marker in a public document rather than by a credential, so the
//	               probe works before any token exists.
//
// Both tag their requests (ProbeHeader) so the instance can exempt them from its own
// idle countdown. The tagging lives with the code that sends the requests, because the
// server side can only guess from a path what a probe is, and a guess would eventually
// exempt a real request.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ProbeHeader marks a request as management traffic.
//
// THE VALUE IS DELIBERATELY OPAQUE: the server asks "is this header present", never what
// it says, so a future probe version does not have to negotiate with an older server.
const ProbeHeader = "X-Any-Probe"

// MarkProbe tags a request as management traffic.
func MarkProbe(req *http.Request) *http.Request {
	req.Header.Set(ProbeHeader, "1")
	return req
}

// IsProbe reports whether a request is management traffic that must not count as
// activity for an idle countdown.
//
// extraPaths covers probes that predate the header, or a caller that asks the status
// route directly: those are management questions too, and counting them would let a
// monitor keep an instance alive forever.
func IsProbe(r *http.Request, extraPaths ...string) bool {
	if r.Header.Get(ProbeHeader) != "" {
		return true
	}
	// A PATH IS EXEMPT ONLY AS A QUESTION. An unmarked GET on the status route is a
	// monitor waking the instance to ask whether it is idle — counting it would keep the
	// answer permanently "no". The same path with another method is an ACTION (a stop
	// request, a config change), and an action is activity by definition.
	if r.Method != http.MethodGet {
		return false
	}
	for _, p := range extraPaths {
		if p != "" && r.URL.Path == p {
			return true
		}
	}
	return false
}

// PortReachable reports whether something accepts TCP connections on host:port.
//
// IT DOES NOT SPEAK HTTP, on purpose: a liveness poll must be free of side effects, and
// an HTTP request to a short-idle instance is activity unless it is tagged. A TCP dial
// cannot be mistaken for one.
func PortReachable(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// IsServing reports whether a program serving `mark` answers on baseURL.
//
// THE MARKER IS THE IDENTITY CHECK. A port that answers is not enough: the port may
// belong to an unrelated program, and stopping "whatever holds 1729" is how a daemon
// command kills somebody's database. The document at docPath is read without a
// credential, so the check works on an instance that requires a token.
func IsServing(ctx context.Context, baseURL, docPath, mark string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+docPath, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(MarkProbe(req))
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
	return bytes.Contains(head, []byte(mark))
}

// HTTPProbe turns an endpoint into a readiness check: nil means ready.
//
// A probe that returns nil on a 401/403 would be a probe that reports ready for a
// program which is merely refusing: the question here is "is the service answering the
// way it should", so only a 2xx is ready and everything else is an error carrying the
// status.
func HTTPProbe(url string) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(MarkProbe(req))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return &StatusError{Code: resp.StatusCode, URL: url}
		}
		return nil
	}
}

// HTTPStop turns an endpoint into a graceful stop.
//
// IT EXISTS BECAUSE WINDOWS HAS NO SIGNAL TO SEND to a detached process: the only way to
// ask such a process to finish is over a socket it is listening on. A caller that sets
// this gets the same shutdown path on every platform — and pkg/detach.Terminate stays the
// fallback for a child that never became reachable.
func HTTPStop(url string, headers map[string]string) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(MarkProbe(req))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return &StatusError{Code: resp.StatusCode, URL: url}
		}
		return nil
	}
}

// StatusError is a probe answer that was not 2xx. It carries the code because the caller
// distinguishes "not up yet" (connection refused) from "up and refusing" (401/403), and
// only the second is worth reporting as a misconfiguration instead of a wait.
type StatusError struct {
	Code int
	URL  string
}

func (e *StatusError) Error() string {
	return e.URL + " answered " + strconv.Itoa(e.Code)
}
