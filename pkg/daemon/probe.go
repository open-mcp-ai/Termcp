package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Do not use environment proxies: a refused proxy connection says nothing
// about whether the actual daemon endpoint is absent.
var healthClient = &http.Client{
	Transport:     &http.Transport{DisableKeepAlives: true},
	Timeout:       3 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// HTTPProbe returns a direct readiness check for a service-specific health URL.
// A 2xx response at that exact URL means ready. Redirects are not followed.
// A refused connection wraps ErrNotRunning. Other transport failures and
// non-2xx responses are inconclusive. This is not authentication or proof
// of instance identity; use a custom probe for a shared endpoint.
func HTTPProbe(url string) func(context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := healthClient.Do(req)
		if err != nil {
			if errors.Is(err, connRefused) && ctx.Err() == nil {
				return fmt.Errorf("%w: %w", ErrNotRunning, err)
			}
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("readiness probe: HTTP %d", resp.StatusCode)
		}
		return nil
	}
}

// PortReachable reports whether a TCP connection to address (host:port)
// can be established. A listening port alone does not identify a service.
func PortReachable(ctx context.Context, address string) bool {
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
