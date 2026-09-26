package sshclient

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"golang.org/x/crypto/ssh"
)

func testShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "bash"
}

func ptyShellCmd() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile"}
	}
	return testShell(), nil
}

func testShellInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func pipeEchoCommand() (string, []string, string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "$input | Write-Output"}, "hello\r\n"
	}
	return "cat", nil, "hello\n"
}

func longRunningCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "Start-Sleep -Seconds 60"}
	}
	return "sleep", []string{"60"}
}

func failingCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "exit 1"}
	}
	return "false", nil
}

func startTestSSHServer(t *testing.T) *sshserver.Server {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}

// dialAndStart creates an in-memory connection to the test server and starts a command.
func dialAndStart(t *testing.T, srv *sshserver.Server, command string, args []string, pty bool, rows, cols int) *ExecSession {
	t.Helper()
	cfg := mintClientConfig(t, srv)
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	es, err := StartWithConn(conn, cfg, command, args, pty, rows, cols)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return es
}

func mintClientConfig(t *testing.T, srv *sshserver.Server) *ssh.ClientConfig {
	t.Helper()
	cfg, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestStart_PipeMode(t *testing.T) {
	srv := startTestSSHServer(t)

	command, args, input := pipeEchoCommand()
	es := dialAndStart(t, srv, command, args, false, 24, 80)

	es.Stdin.Write([]byte(input))
	es.Stdin.Close()

	<-es.Done()

	if es.ExitCode() != 0 {
		t.Fatalf("expected exit code 0, got %d", es.ExitCode())
	}
}

func TestStart_PtyMode(t *testing.T) {
	srv := startTestSSHServer(t)

	sh, shArgs := ptyShellCmd()
	es := dialAndStart(t, srv, sh, shArgs, true, 24, 80)

	time.Sleep(200 * time.Millisecond)
	es.Stdin.Write([]byte(testShellInput("echo pty_test")))

	// Loop-read until we see the expected output
	buf := make([]byte, 4096)
	var allOutput string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(allOutput, "pty_test") {
		n, _ := es.Stdout.Read(buf)
		allOutput += string(buf[:n])
	}
	if !strings.Contains(allOutput, "pty_test") {
		t.Fatalf("expected output containing 'pty_test', got %q", allOutput)
	}

	es.Stdin.Write([]byte(testShellInput("exit")))
	<-es.Done()
}

func TestStart_ResizePty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("interactive PTY tests not stable on Windows ConPTY")
	}
	srv := startTestSSHServer(t)

	sh, shArgs := ptyShellCmd()
	es := dialAndStart(t, srv, sh, shArgs, true, 24, 80)
	defer es.Close()

	if err := es.ResizePty(50, 120); err != nil {
		t.Fatalf("ResizePty failed: %v", err)
	}

	es.Stdin.Write([]byte(testShellInput("exit")))
	<-es.Done()
}

func TestStart_Signal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on Windows")
	}
	srv := startTestSSHServer(t)

	command, args := longRunningCommand()
	es := dialAndStart(t, srv, command, args, false, 24, 80)

	time.Sleep(200 * time.Millisecond) // let server process start
	if err := es.Signal("TERM"); err != nil {
		t.Fatalf("Signal failed: %v", err)
	}
	// Close stdin to unblock server's stdin copy goroutine in pipe mode
	es.Stdin.Close()

	select {
	case <-es.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after signal")
	}
}

func TestStart_Close(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("session close is reported as EOF on Windows ConPTY")
	}
	srv := startTestSSHServer(t)

	command, args := longRunningCommand()
	es := dialAndStart(t, srv, command, args, false, 24, 80)

	if err := es.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case <-es.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after close")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		cmd  string
		args []string
		want string
	}{
		{"echo", []string{"hello"}, `echo hello`},
		{"echo hello", nil, `"echo hello"`},
		{"echo", []string{"a b"}, `echo "a b"`},
		{"echo", []string{`a"b`}, `echo "a\"b"`},
		{"my command", []string{"arg"}, `"my command" arg`},
	}
	for _, tc := range tests {
		got := shellQuote(tc.cmd, tc.args)
		if got != tc.want {
			t.Fatalf("shellQuote(%q, %v) = %q, want %q", tc.cmd, tc.args, got, tc.want)
		}
	}
}

func TestStart_CommandFailure(t *testing.T) {
	srv := startTestSSHServer(t)

	command, args := failingCommand()
	es := dialAndStart(t, srv, command, args, false, 24, 80)

	// Close stdin to unblock server's stdin copy goroutine in pipe mode
	es.Stdin.Close()

	select {
	case <-es.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}
	if es.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %d", es.ExitCode())
	}
}

func TestDefaultPTYModes(t *testing.T) {
	modes := defaultPTYModes()

	required := map[string]uint8{
		"ECHO":   ssh.ECHO,
		"ICRNL":  ssh.ICRNL,
		"ONLCR":  ssh.ONLCR,
		"OPOST":  ssh.OPOST,
		"ISIG":   ssh.ISIG,
		"ICANON": ssh.ICANON,
	}

	for name, opcode := range required {
		v, ok := modes[opcode]
		if !ok {
			t.Errorf("missing required terminal mode %s (opcode %d)", name, opcode)
			continue
		}
		if v != 1 {
			t.Errorf("terminal mode %s = %d, want 1 (enabled)", name, v)
		}
	}
}

// An empty command in pipe mode must be refused outright. There is no login
// shell to ask for on a pipe channel, and the client must not invent one from
// its own PATH: that path describes the machine termcp runs on, which for a
// remote target is the wrong machine entirely. (Reported as a remote zsh host
// receiving `C:\...\pwsh.exe` as its exec command.)
func TestStart_PipeModeEmptyCommandRefused(t *testing.T) {
	srv := startTestSSHServer(t)
	cfg := mintClientConfig(t, srv)
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	es, err := StartWithConn(conn, cfg, "", nil, false, 24, 80)
	if err == nil {
		es.Close()
		t.Fatal("expected an error for an empty pipe command")
	}
	if !strings.Contains(err.Error(), "pipe") {
		t.Fatalf("error should name the pipe mode, got %v", err)
	}
	// The refusal must not leak this host's shell path into the message either.
	if strings.Contains(err.Error(), testShell()) {
		t.Fatalf("error must not suggest a local shell, got %v", err)
	}
}

// A pty shell with no command asks the SERVER for its login shell. The test
// server always has one, so this asserts the request is made rather than that a
// particular shell name came back.
func TestStart_PtyModeEmptyCommandUsesServerShell(t *testing.T) {
	srv := startTestSSHServer(t)
	es := dialAndStart(t, srv, "", nil, true, 24, 80)
	defer es.Close()
	time.Sleep(200 * time.Millisecond)
	es.Stdin.Write([]byte(testShellInput("echo server_shell_ok")))
	buf := make([]byte, 4096)
	var out string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out, "server_shell_ok") {
		n, _ := es.Stdout.Read(buf)
		out += string(buf[:n])
	}
	if !strings.Contains(out, "server_shell_ok") {
		t.Fatalf("pty shell with no command should run the server's login shell, got %q", out)
	}
}
