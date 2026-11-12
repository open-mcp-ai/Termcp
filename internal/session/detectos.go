package session

import (
	"context"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// WHAT OPERATING SYSTEM IS ON THE OTHER SIDE (issue #318).
//
// A caller reading `ls` had no way to tell a Windows session from a Linux one, and the
// measured cost was three wasted turns: `ls /workspace` on a Windows node, a "path not
// found", a retry, and only then the switch to a container. The fact is one command away
// and belongs in the session's own record.
//
// THE PROBE IS A ONE-SHOT PIPE, NOT A PTY. It runs on a fresh exec channel, reads, and
// closes: nothing is typed into the interactive shell, nothing is echoed into the
// caller's transcript, and a foreground program in the pty cannot swallow it. That is
// what makes it affordable on every connect.

// osProbeCommand answers with the kernel name on every platform termcp can reach:
// `uname -s` where it exists, `ver` on a Windows that has no uname. Both are a builtin or
// a guaranteed binary of their own shell, so nothing has to be installed.
const osProbeCommand = "uname -s 2>/dev/null || ver"

// osProbeTimeout bounds the probe. The answer is worth having; it is never a reason a
// connect fails, so a host that cannot answer leaves the field empty and the caller reads
// "unknown".
const osProbeTimeout = 5 * time.Second

// DetectOS records the target's operating system. Called once per connection: a reconnect
// probes again, which is what "the fact expires with the connection" means.
func (s *Session) DetectOS() {
	// AN IN-PROCESS TARGET IS THIS MACHINE, so asking would be a round trip for something
	// already known. Everything else is measured.
	s.mu.RLock()
	endpoint := s.Session.SSHEndpoint
	s.mu.RUnlock()
	if endpoint == "internal" {
		s.SetOS(localOS(), "local")
		return
	}
	client := s.SSHClient()
	if client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), osProbeTimeout)
		defer cancel()
		if name := probeOS(ctx, client); name != "" {
			s.SetOS(name, "probe")
		}
	}()
}

// probeOS runs the probe over a fresh channel and returns the category, or "" when the
// host did not answer. A failure is silent on purpose: this is an annotation.
func probeOS(ctx context.Context, client *ssh.Client) string {
	sess, err := client.NewSession()
	if err != nil {
		return ""
	}
	defer sess.Close()

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := sess.CombinedOutput(osProbeCommand)
		done <- result{string(out), err}
	}()
	select {
	case <-ctx.Done():
		// Closing the session unblocks the goroutine holding it, the same way the
		// shutdown path does for a stuck read.
		_ = sess.Close()
		return ""
	case r := <-done:
		if r.out == "" {
			return ""
		}
		return classifyOS(r.out)
	}
}

// classifyOS maps the probe's raw text to a category a caller can branch on.
//
// THE RAW LINE IS NOT ECHOED BACK. A caller deciding between `ls` and `dir` needs four
// answers, not a kernel version; and a classifier here is the only place that has to know
// `uname -s` spells Windows as MINGW64_NT (which it does under Git Bash).
func classifyOS(out string) string {
	low := strings.ToLower(strings.TrimSpace(out))
	switch {
	case low == "":
		return ""
	case strings.Contains(low, "mingw"), strings.Contains(low, "msys"),
		strings.Contains(low, "cygwin"), strings.Contains(low, "windows"):
		return "windows"
	case strings.HasPrefix(low, "linux"):
		return "linux"
	case strings.HasPrefix(low, "darwin"):
		return "darwin"
	case strings.HasPrefix(low, "freebsd"), strings.HasPrefix(low, "openbsd"),
		strings.HasPrefix(low, "netbsd"):
		return "bsd"
	case strings.HasPrefix(low, "sunos"):
		return "solaris"
	}
	return "other"
}

// SetOS records an OS determined without a probe — an operator's configuration, or an
// embedder that already knows. The source says which, so a reader can tell a measurement
// from an inference.
func (s *Session) SetOS(name, source string) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return
	}
	s.mu.Lock()
	s.Session.OS = name
	s.Session.OSSource = source
	s.Session.UpdatedAt = clock.Now()
	s.mu.Unlock()
}

// localOS is what an in-process target answers without asking anybody.
func localOS() string {
	switch runtime.GOOS {
	case "windows", "darwin", "linux":
		return runtime.GOOS
	}
	return "other"
}
