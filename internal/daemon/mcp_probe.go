package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProbeMCP is a client admission check, separate from daemon management. An
// Agent may discover the keepalive interval without gaining stop/status access.
func ProbeMCP(ctx context.Context, host string, port int, credential string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", BaseURL(host, port)+"/auth/mcp", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("MCP authentication rejected (%d)", resp.StatusCode)
	}
	var info struct {
		OK   bool  `json:"ok"`
		Idle int64 `json:"idle_timeout_ms"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info); err != nil {
		return 0, err
	}
	if !info.OK || info.Idle < 0 || info.Idle > int64((365*24*time.Hour)/time.Millisecond) {
		return 0, fmt.Errorf("invalid MCP probe response")
	}
	return time.Duration(info.Idle) * time.Millisecond, nil
}
