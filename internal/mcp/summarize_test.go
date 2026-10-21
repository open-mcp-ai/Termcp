package mcp

import "testing"

// TestForwardReviewShowsTheBindingAddress pins the prompt half of bind_host.
//
// Review mode is the only thing standing between "--bind-host 0.0.0.0" and a
// tunnel on the network, and a prompt that renders both bindings as ":8080" asks
// the reviewer to approve a decision it does not show them. The default is
// spelled out rather than left blank for the same reason: the empty argument is
// not missing information, it is the loopback default.
func TestForwardReviewShowsTheBindingAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "default bind is spelled out",
			args: map[string]any{"action": "local", "local_port": 8080, "remote_host": "db", "remote_port": 5432},
			want: "open local forward 127.0.0.1:8080 -> db:5432",
		},
		{
			name: "explicit wildcard is shown and marked",
			args: map[string]any{"action": "local", "bind_host": "0.0.0.0", "local_port": 8080, "remote_host": "db", "remote_port": 5432},
			want: "open local forward 0.0.0.0:8080 -> db:5432  (reachable from the network, not just this machine)",
		},
		{
			name: "loopback needs no warning",
			args: map[string]any{"action": "local", "bind_host": "127.0.0.1", "local_port": 80, "remote_host": "db", "remote_port": 80},
			want: "open local forward 127.0.0.1:80 -> db:80",
		},
		{
			name: "dynamic says it is local only",
			args: map[string]any{"action": "dynamic", "local_port": 1080},
			want: "open SOCKS5 proxy on port 1080 (this host only)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeOperation("forward", tc.args); got != tc.want {
				t.Errorf("summarizeOperation(forward) = %q, want %q", got, tc.want)
			}
		})
	}
}
