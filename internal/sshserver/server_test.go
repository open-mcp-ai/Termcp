package sshserver

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "/bin/bash"
}

// ptyShellLine returns the shell command line for PTY tests (suppress Windows profile / slow startup).
func ptyShellLine() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe -NoLogo -NoProfile"
	}
	return testShell()
}

func testShellInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func testShellEchoCommand(s string) string {
	if runtime.GOOS == "windows" {
		return "powershell.exe -NoProfile -Command Write-Output " + s
	}
	return "echo " + s
}

func mintCfg(t *testing.T, srv *Server) *ssh.ClientConfig {
	t.Helper()
	c, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// dialServer creates an in-memory SSH client connected to the test server.
func dialServer(t *testing.T, srv *Server, config *ssh.ClientConfig) *ssh.Client {
	t.Helper()
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, "inmem", config)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return ssh.NewClient(c, chans, reqs)
}

func TestMint_OneTimePassword(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	cfg, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	c1 := dialServer(t, srv, cfg)
	_ = c1.Close()

	// Second dial with same one-time config should fail at SSH handshake.
	conn2, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = ssh.NewClientConn(conn2, "inmem", cfg)
	if err == nil {
		t.Fatal("expected second dial with same one-time config to fail")
	}
	// ssh.NewClientConn closes conn2 on handshake failure, so no explicit Close needed.
}

// A minted credential that never authenticates (dial refused, handshake error)
// must not stay in the pending map: failed session starts would otherwise leak
// one entry per attempt for the lifetime of the process.
func TestRevokeClientConfig(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	cfg, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	_, pendingBefore := srv.pending[cfg.User]
	srv.mu.Unlock()
	if !pendingBefore {
		t.Fatal("minted credential should be pending before revoke")
	}

	srv.RevokeClientConfig(cfg.User)
	srv.mu.Lock()
	_, pendingAfter := srv.pending[cfg.User]
	srv.mu.Unlock()
	if pendingAfter {
		t.Fatal("RevokeClientConfig left the credential in the pending map")
	}

	// Revoking an unknown/already-consumed credential must be a no-op.
	srv.RevokeClientConfig(cfg.User)
	srv.RevokeClientConfig("")
}

func TestServer_StartAndStop(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	// Verify we can connect
	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	client.Close()

	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestServer_PipeSession(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	out, err := session.Output(testShellEchoCommand("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(string(out), "\r\n") != "hello" {
		t.Fatalf("expected hello output, got %q", string(out))
	}
}

// TestServer_PipeSession_StdinNeverClosed guards the pipe-mode deadlock: when
// the client opens StdinPipe and never writes/closes it (the termcp MCP/handler
// path), the server must still finish Wait once the command exits for commands
// that do not read stdin (echo, ls, …). Regression: unrelated to whether the
// client provides stdin − the session must not hang forever.
func TestServer_PipeSession_StdinNeverClosed(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately never write to or close stdin — replicates the termcp SSH
	// client which keeps stdin open for the life of the session.
	_ = stdin
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := session.Start("echo deadlock-guard"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		b, _ := io.ReadAll(stdout)
		if strings.TrimRight(string(b), "\r\n") != "deadlock-guard" {
			t.Errorf("unexpected output: %q", string(b))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish the pipe session: stdin-copy deadlock")
	}
	if err := session.Wait(); err != nil {
		t.Errorf("Wait returned error: %v", err)
	}
}

func TestServer_SignalTerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on Windows")
	}
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := session.Start("sleep 30"); err != nil {
		t.Fatal(err)
	}

	// Give the process time to start
	time.Sleep(200 * time.Millisecond)

	if err := session.Signal(ssh.SIGTERM); err != nil {
		t.Fatalf("signal failed: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error from SIGTERM")
		}
		if exitErr, ok := err.(*ssh.ExitError); ok {
			// SIGTERM → exit 143 (128+15) on Unix
			if exitErr.ExitStatus() == 0 {
				t.Fatal("expected non-zero exit status from SIGTERM")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for signal kill")
	}
}

func TestServer_SignalInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on Windows")
	}
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := session.Start("sleep 30"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	if err := session.Signal(ssh.SIGINT); err != nil {
		t.Fatalf("signal failed: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error from SIGINT")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for signal interrupt")
	}
}

// A signal that arrives before the session handler registers its channel is
// buffered by the library and replayed from a goroutine that reads the slot
// WITHOUT the session lock, so teardown must never write that slot again. The
// order here makes the collision deterministic: signalling before exec leaves the
// signals buffered, and registering the channel starts the replay.
func TestServer_BufferedSignalReplayDoesNotRaceTeardown(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for range 8 {
		if err := session.Signal(ssh.SIGTERM); err != nil {
			t.Fatalf("signal before exec: %v", err)
		}
	}
	if err := session.Start(testShellEchoCommand("buffered-signal")); err != nil {
		t.Fatal(err)
	}
	_ = session.Wait() // let the handler reach the teardown that used to write the slot
}

func TestServer_ProcessStateNil(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// Run a nonexistent command — ProcessState will be nil
	out, _ := session.CombinedOutput("this_command_does_not_exist_12345")
	_ = out // server should not panic
}

func TestServer_PtySession(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", 24, 80, modes); err != nil {
		t.Fatal(err)
	}

	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()

	if err := session.Start(ptyShellLine()); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	stdin.Write([]byte(testShellInput("echo test_pty")))

	// Read in a loop until we see "test_pty" or timeout
	deadline := time.Now().Add(5 * time.Second)
	var allOutput string
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) && !strings.Contains(allOutput, "test_pty") {
		n, _ := stdout.Read(buf)
		allOutput += string(buf[:n])
	}
	if !strings.Contains(allOutput, "test_pty") {
		t.Fatalf("expected output containing 'test_pty', got %q", allOutput)
	}

	stdin.Write([]byte(testShellInput("exit")))
	session.Wait()
}

// ptyExitCommand returns a command line that exits with the given status, and
// the status it should report.
func ptyExitCommand(code int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("cmd.exe /c exit %d", code)
	}
	return fmt.Sprintf("sh -c 'exit %d'", code)
}

// A PTY session must report the child's real exit code. This is the half of the
// Windows reap guard that fails loudly (see waitChild in server_windows.go): the
// fix waits on its own handle to the process instead of calling exec.Cmd.Wait,
// which works because ConPTY never closes the process handle it spawns with. If
// a future x/conpty starts closing it, waitChild falls back to 127 and this test
// fails instead of the exit code silently rotting.
//
// It also covers the pre-fix double reap, where the library's own goroutine and
// the handler both called Wait on the same exec.Cmd: the loser saw a nil state,
// so the session reported the 127 fallback rather than the child's code.
func TestServer_PtyExitCode(t *testing.T) {
	const want = 42

	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	if err := session.Start(ptyExitCommand(want)); err != nil {
		t.Fatal(err)
	}

	err = session.Wait()
	exitErr, ok := err.(*ssh.ExitError)
	if !ok {
		t.Fatalf("expected an *ssh.ExitError, got %v", err)
	}
	if got := exitErr.ExitStatus(); got != want {
		t.Fatalf("pty session reported exit code %d, want %d", got, want)
	}
}

func TestServer_PtyEnviron(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TERM env var is a Unix concept, not meaningful on Windows")
	}
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		t.Fatal(err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := session.Start(ptyShellLine()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	stdin.Write([]byte(testShellInput("echo TERM=$TERM")))

	deadline := time.Now().Add(5 * time.Second)
	var allOutput string
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) && !strings.Contains(allOutput, "TERM=") {
		n, _ := stdout.Read(buf)
		allOutput += string(buf[:n])
	}

	if !strings.Contains(allOutput, "TERM=xterm-256color") {
		t.Fatalf("expected TERM=xterm-256color in output, got %q", allOutput)
	}

	stdin.Write([]byte(testShellInput("exit")))
	session.Wait()
}

// countGoroutines returns the current goroutine count after a settle period.
func countGoroutines() int {
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// Every handled session used to leak one signal-forwarding goroutine: it ranged
// over a channel the library never closes, so it blocked forever after the shell
// exited. Delete a handful of sessions and the count must come back down.
func TestServer_SessionGoroutinesReleased(t *testing.T) {
	if testing.Short() {
		t.Skip("goroutine accounting is timing sensitive")
	}
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	// Warm up: the first session spins up library-internal goroutines that are
	// intentionally long-lived, so measure the baseline only after one round.
	runOneSession := func() {
		cfg := mintCfg(t, srv)
		c := dialServer(t, srv, cfg)
		session, err := c.NewSession()
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		stdin, err := session.StdinPipe()
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		stdout, err := session.StdoutPipe()
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		if err := session.Start(testShellEchoCommand("leak-check")); err != nil {
			c.Close()
			t.Fatal(err)
		}
		_, _ = io.ReadAll(stdout)
		_, _ = stdin.Write([]byte("\n"))
		_ = session.Wait()
		_ = session.Close()
		_ = c.Close()
	}

	runOneSession()
	baseline := countGoroutines()

	const rounds = 6
	for i := 0; i < rounds; i++ {
		runOneSession()
	}

	after := countGoroutines()
	// Allow a little slack for runtime-internal goroutines that linger briefly.
	if after > baseline+3 {
		t.Fatalf("goroutine leak: baseline=%d after %d sessions=%d (leaked ~%d)", baseline, rounds, after, after-baseline)
	}
}
