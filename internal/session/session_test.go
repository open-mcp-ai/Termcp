package session

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func testShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "bash"
}

func testInteractiveShellArgs() []string {
	if runtime.GOOS == "windows" {
		return testShellArgs("-NoLogo", "-NoProfile")
	}
	return nil
}

func testShellInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func testInteractiveOutputCommand(s string) string {
	if runtime.GOOS == "windows" {
		return "Write-Output " + s
	}
	return "echo " + s
}

func testShellArgs(args ...string) []string {
	return args
}

func testShellEchoArgs(s string) []string {
	if runtime.GOOS == "windows" {
		return testShellArgs("-NoLogo", "-NoProfile", "-Command", "Write-Output "+s)
	}
	return testShellArgs("-c", "echo "+s)
}

func testSleepCommand(seconds string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "Start-Sleep -Seconds " + seconds}
	}
	return "sleep", []string{seconds}
}

func testPipeCommand() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "cat"
}

func TestInfo_DeepCopyExitCode(t *testing.T) {
	t.Skip("Session.done() no longer tracks ExitCode; exit code is per-shell")
}

func startTestServer(t *testing.T) *sshserver.Server {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}

func testConfig(command string, args []string, mode api.SessionMode, name string) Config {
	return Config{
		Command: command,
		Args:    args,
		Mode:    mode,
		Name:    name,
		Rows:    24,
		Cols:    80,
	}
}

func TestSession_CreateAndInfo(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "test-session"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	info := s.Info()
	if info.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if info.Name != "test-session" {
		t.Fatalf("expected name 'test-session', got %q", info.Name)
	}
	if info.Status != api.SessionRunning {
		t.Fatalf("expected status 'running', got %q", info.Status)
	}
	if info.Mode != "" {
		t.Fatalf("session records must carry no mode (it is per shell), got %q", info.Mode)
	}
	if ps := s.PrimaryShell(); ps == nil {
		t.Fatal("expected a live primary shell")
	} else if got := ps.Info().Mode; got != api.ModePTY {
		t.Fatalf("expected primary shell mode 'pty', got %q", got)
	}
}

func TestSession_SendInputReadOutput(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	// The shell may still be cold-starting on a slow CI runner (PowerShell under
	// ConPTY can take seconds), so input typed too early is dropped by the TTY.
	// Retry the line until the marker shows up rather than asserting startup
	// speed — the same approach as the MCP press-key test.
	var output string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.PrimaryShell().SendTerminalBytes([]byte(testShellInput(testInteractiveOutputCommand("session_test"))), false); err != nil {
			t.Fatal(err)
		}
		chunk, _ := s.ReadOutput(context.Background(), 1*time.Second, true, 0, 0)
		output += chunk
		if strings.Contains(output, "session_test") {
			break
		}
	}
	if !strings.Contains(output, "session_test") {
		t.Fatalf("expected output containing 'session_test', got %q", output)
	}
}

func TestSession_Terminate(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)
	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	info := s.Info()
	if info.Status != api.SessionRunning {
		t.Fatalf("expected 'running', got %q", info.Status)
	}

	m.Terminate(id, false, 2*time.Second)

	if m.Get(id) == nil {
		t.Fatal("expected session to be retained (DEAD) after terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after terminate, got %q", got)
	}
}

func TestSession_ForceTerminate(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)
	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	m.Terminate(id, true, 0)

	if m.Get(id) == nil {
		t.Fatal("expected session to be retained (DEAD) after force terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after force terminate, got %q", got)
	}
}

func TestManager_TerminateKeepsDeadThenDeleteReleases(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)

	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	var (
		mu    sync.Mutex
		count int
	)
	// Attached at creation time (as forwards and notification rules are): the
	// cleanup belongs to the session, not to a manager-wide listener list.
	s.AttachCleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	m.Terminate(id, true, 0)
	// Repeating the DEAD path (e.g. Disconnect after Terminate) must not release
	// attached resources or remove the registry entry.
	s.Disconnect()

	mu.Lock()
	if count != 0 {
		t.Fatalf("terminate/disconnect must not release attached resources, got %d", count)
	}
	mu.Unlock()

	if m.Get(id) == nil {
		t.Fatal("expected session retained (DEAD) after terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited', got %q", got)
	}

	// Delete is the only release: runs cleanup once and removes the registry entry.
	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if count != 1 {
		t.Fatalf("expected resource cleanup once on delete, got %d", count)
	}
	mu.Unlock()
	if m.Get(id) != nil {
		t.Fatal("expected session removed after delete")
	}
}

func TestManager_DisconnectKeepsDead(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)

	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	var (
		mu    sync.Mutex
		count int
	)
	s.AttachCleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	// SSH abort/Disconnect only DEADs the session; it never releases resources.
	s.Disconnect()

	if m.Get(id) == nil {
		t.Fatal("expected session retained (DEAD) after disconnect")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after disconnect, got %q", got)
	}
	mu.Lock()
	if count != 0 {
		t.Fatalf("disconnect must not release attached resources, got %d", count)
	}
	mu.Unlock()

	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if count != 1 {
		t.Fatalf("expected resource cleanup once on delete, got %d", count)
	}
	mu.Unlock()
	if m.Get(id) != nil {
		t.Fatal("expected session removed after delete")
	}
}

func TestSession_ResizePty(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	if err := s.PrimaryShell().ResizePty(50, 120); err != nil {
		t.Fatalf("ResizePty failed: %v", err)
	}

	info := s.PrimaryShell().Info()
	if info.Rows != 50 || info.Cols != 120 {
		t.Fatalf("expected 50x120, got %dx%d", info.Rows, info.Cols)
	}
}

// A pipe shell has no TTY, so resizing it must fail on the shell, not silently
// on the session. Mode is per shell: the check lives in ChildShell.ResizePty.
func TestSession_ResizePtyPipeMode(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testPipeCommand(), nil, api.ModePipe, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	if err := s.PrimaryShell().ResizePty(50, 120); err == nil {
		t.Fatal("expected error when resizing a pipe shell")
	}
}

func TestSession_SendInputAfterExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell exit under ConPTY is not deterministic enough for this assertion")
	}
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}

	s.PrimaryShell().SendTerminalBytes([]byte(testShellInput("exit")), false)

	time.Sleep(1 * time.Second)

	err = s.PrimaryShell().SendTerminalBytes([]byte("should fail"), true)
	if err == nil {
		t.Fatal("expected error sending input to exited process")
	}
}

func TestSession_NaturalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell -Command under ConPTY stays interactive after command completion")
	}
	srv := startTestServer(t)

	s, err := New(srv, Config{Command: testShell(), Args: testShellEchoArgs("hello"), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)

	// Shell exit does not tear down the session — the session container
	// stays alive for potential new shells (applies to both internal and remote).
	info := s.Info()
	if info.Status != api.SessionRunning {
		t.Fatalf("session should still be running after shell exit, got %q", info.Status)
	}
}

// TestManager_PipeShellExitKeepsSessionRunning locks the container contract: a
// shell is a channel, and its lifetime never decides the session's. A run-to-exit
// pipe command finishing cleanly must leave the container running (retained,
// reusable) so forwards, SFTP and new shells still work — only terminate,
// disconnect, or shutdown flip it DEAD. The exited shell stays in the map so its
// output can still be drained.
func TestManager_PipeShellExitKeepsSessionRunning(t *testing.T) {
	srv := startTestServer(t)
	mgr := NewManager(nil, nil, srv)

	// A short pipe command (echo) that exits cleanly on its own.
	s, err := mgr.Create(Config{Command: testShell(), Args: testShellEchoArgs("done"), Mode: api.ModePipe, Name: "pipe", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID
	defer mgr.Delete(id)

	// Wait for the shell to reach a terminal status.
	shellDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(shellDeadline) {
		shells := s.ListChildShells()
		if len(shells) == 1 && shells[0].Status != api.SessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	got := mgr.Get(id)
	if got == nil {
		t.Fatal("session must stay registered after its last shell exits")
	}
	if got.Info().Status != api.SessionRunning {
		t.Fatalf("session must stay running after a pipe shell exits, got %q", got.Info().Status)
	}
	// Output must remain readable from the retained shell.
	if len(got.ListChildShells()) == 0 {
		t.Fatal("expected exited shell retained in map for reading output")
	}
}

func TestManager_CreateAndGet(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	// Use a long-running command so auto-delete doesn't fire before we inspect.
	command, args := testSleepCommand("60")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "test", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	got := mgr.Get(s.ID)
	if got == nil {
		t.Fatal("expected to find session")
	}
	if got.ID != s.ID {
		t.Fatalf("expected ID %q, got %q", s.ID, got.ID)
	}
}

func TestManager_ListAll(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("60")
	s1, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s1", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Terminate(true, 0)
	s2, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s2", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Terminate(true, 0)

	all := mgr.ListAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(all))
	}
}

func TestManager_MarkAllDead(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("60")
	mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s1", Rows: 24, Cols: 80})
	mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s2", Rows: 24, Cols: 80})

	// Shutdown semantics: DEAD every running session in place; nothing is
	// removed from the registry (disconnect ≠ delete).
	mgr.MarkAllDead()

	time.Sleep(500 * time.Millisecond)

	all := mgr.ListAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions retained after MarkAllDead, got %d", len(all))
	}
	for _, s := range all {
		if s.Status != api.SessionExited {
			t.Fatalf("expected session %q exited after MarkAllDead, got %q", s.ID, s.Status)
		}
	}
}

func TestManager_Delete(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("0.1")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "del-me", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}

	sid := s.ID

	// Terminate only turns the session DEAD; it stays registered.
	s.Terminate(true, 0)
	if mgr.Get(sid) == nil {
		t.Fatal("expected session retained (DEAD) after terminate")
	}
	if got := mgr.Get(sid).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited', got %q", got)
	}

	// Only Delete removes it from the registry.
	if err := mgr.Delete(sid); err != nil {
		t.Fatal(err)
	}
	if mgr.Get(sid) != nil {
		t.Fatal("expected session removed from registry after delete")
	}

	all := mgr.ListAll()
	if len(all) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(all))
	}
}

func TestManager_DeleteRunningSession(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("60")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	sid := s.ID

	if err := mgr.Delete(sid); err != nil {
		t.Fatal(err)
	}
	if mgr.Get(sid) != nil {
		t.Fatal("expected running session to be force-removed by Delete")
	}
}

// goroutineCountStable returns the process goroutine count once it has stopped
// changing for two consecutive samples, so a baseline is not taken in the middle
// of an earlier test's teardown. Falls back to the last sample at the deadline,
// since a busy process may never be perfectly still.
func goroutineCountStable(timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	last := runtime.NumGoroutine()
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		now := runtime.NumGoroutine()
		if now == last {
			stable++
			if stable >= 2 {
				return now
			}
			continue
		}
		stable = 0
		last = now
	}
	return last
}

// waitGoroutinesAtMost polls until the count falls to at most want and stays
// there for a short settle window. Goroutine counting is process-wide, so this
// waits for a release instead of sampling once after a fixed sleep: cleanup
// takes longer on a loaded CI runner, and a fixed sleep turns that latency into
// a flake. A real leak never settles, so it still fails.
func waitGoroutinesAtMost(want int, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	settled := 0
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		last = runtime.NumGoroutine()
		if last <= want {
			settled++
			// Stay below the bar across a few samples, so a concurrent dip in some
			// other test's teardown cannot be mistaken for our own cleanup.
			if settled >= 3 {
				return last, true
			}
		} else {
			settled = 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last, false
}

// Terminating a session must release the goroutines it started. The count is
// process-wide, so the assertion is on a SETTLED baseline versus a settled end
// state rather than on one sample: other tests in this package leave sessions
// and servers winding down, and their churn must not read as our leak.
func TestSession_GoroutinesCleanedUp(t *testing.T) {
	srv := startTestServer(t)

	before := goroutineCountStable(2 * time.Second)

	s, err := New(srv, Config{Command: testShell(), Args: testInteractiveShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	s.Terminate(true, 0)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Info().Status != api.SessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if after, ok := waitGoroutinesAtMost(before+2, 15*time.Second); !ok {
		t.Fatalf("goroutines did not settle after terminate: before=%d after=%d (a released session must not keep its goroutines)", before, after)
	}
}

func TestSession_ReadOutputWithMaxBytes(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	time.Sleep(200 * time.Millisecond)

	// Generate predictable output longer than maxBytes
	longText := strings.Repeat("ABCDEFGHIJ", 100) // 1000 bytes
	cmd := testInteractiveOutputCommand(longText)
	if err := s.PrimaryShell().SendTerminalBytes([]byte(testShellInput(cmd)), false); err != nil {
		t.Fatal(err)
	}

	// Wait for output to accumulate
	time.Sleep(500 * time.Millisecond)

	maxBytes := 100
	output, err := s.ReadOutput(context.Background(), 500*time.Millisecond, true, 0, maxBytes)
	if err != nil {
		t.Fatal(err)
	}

	if len(output) > maxBytes {
		t.Fatalf("expected output <= %d bytes, got %d bytes", maxBytes, len(output))
	}

	// Should still have more data available
	if !s.HasMoreOutput(s.DefaultOutputReaderID()) {
		t.Fatal("expected HasMoreOutput=true after partial read")
	}
}

func TestSession_ReadOutputWithMaxLinesPreservesUnreadData(t *testing.T) {
	b := buffer.New(1024)
	r, _ := b.NewReader()
	s := &Session{
		Session:  api.Session{ID: "test-session"},
		buf:      b,
		readerID: r,
	}

	if err := b.Write([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}

	output, err := s.ReadOutput(context.Background(), 0, false, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "one\ntwo\n" {
		t.Fatalf("expected first two lines, got %q", output)
	}
	if !s.HasMoreOutput(s.DefaultOutputReaderID()) {
		t.Fatal("expected unread output after max_lines read")
	}

	output, err = s.ReadOutput(context.Background(), 0, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "three\nfour\n" {
		t.Fatalf("expected remaining lines, got %q", output)
	}
	if s.HasMoreOutput(s.DefaultOutputReaderID()) {
		t.Fatal("expected no unread output after draining")
	}
}

func TestChildShell_ReadTerminalStreamWithMaxLinesPreservesUnreadData(t *testing.T) {
	b := buffer.New(1024)
	r, _ := b.NewReader()
	cs := &ChildShell{buf: b}

	if err := b.Write([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}

	output, err := cs.ReadTerminalStream(context.Background(), r, 0, false, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "one\ntwo\n" {
		t.Fatalf("expected first two lines, got %q", output)
	}
	if !cs.HasMoreOutput(r) {
		t.Fatal("expected unread output after max_lines read")
	}

	output, err = cs.ReadTerminalStream(context.Background(), r, 0, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "three\nfour\n" {
		t.Fatalf("expected remaining lines, got %q", output)
	}
	if cs.HasMoreOutput(r) {
		t.Fatal("expected no unread output after draining")
	}
}

func TestManager_CloseInternalChildShellKeepsParentSession(t *testing.T) {
	srv := startTestServer(t)
	mgr := NewManager(nil, nil, srv)

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "parent"))
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Delete(s.ID)

	child, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "child")
	if err != nil {
		t.Fatal(err)
	}

	found, err := mgr.CloseChildShell(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected child shell to be found")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.GetChildShell(child.ID) != nil {
		time.Sleep(20 * time.Millisecond)
	}

	if s.GetChildShell(child.ID) != nil {
		t.Fatal("expected child shell to be removed")
	}

	time.Sleep(200 * time.Millisecond)
	if mgr.Get(s.ID) == nil {
		t.Fatal("expected parent session to remain registered after closing child shell")
	}
	if s.IsBufferClosed() {
		t.Fatal("expected parent output buffer to remain open after closing child shell")
	}

	// Closed shell must vanish from the retained snapshot (the view list is
	// the live map for a running session, already asserted above).
	for _, sh := range s.SnapshotShells() {
		if sh.ID == child.ID {
			t.Fatal("closed child shell must not appear in SnapshotShells")
		}
	}
}

// Manual shell close must survive nothing: not the live map, not the retained
// snapshot, and not the persisted shell manifest. After a restart the closed
// shell must not reappear as a DEAD/"end" tab (the reported bug).
func TestManager_CloseChildShellNotResurrectedAfterRestart(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	m := NewManager(message.NewManager(store), store, srv)

	s, err := m.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "parent"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Delete(s.ID)

	child, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "child")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	found, err := m.CloseChildShell(child.ID)
	if err != nil || !found {
		t.Fatalf("close child shell: found=%v err=%v", found, err)
	}
	time.Sleep(200 * time.Millisecond)

	// Session stays running (PTY container is reusable); shell is gone locally.
	if got := m.Get(s.ID).Info().Status; got != api.SessionRunning {
		t.Fatalf("pty session must stay running after closing a child shell, got %q", got)
	}
	if s.GetChildShell(child.ID) != nil {
		t.Fatal("closed child shell still in live map")
	}
	for _, sh := range s.SnapshotShells() {
		if sh.ID == child.ID {
			t.Fatal("closed child shell retained in SnapshotShells — delete required")
		}
	}
	// The persisted snapshot must not contain the closed shell either.
	loaded, err := store.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range loaded {
		for _, sh := range meta.Shells {
			if sh.ID == child.ID {
				t.Fatal("closed child shell persisted in the shell manifest — would resurrect on restart")
			}
		}
	}

	// "Restart": a fresh manager over the same store restores the session as a
	// DEAD view; the closed shell must not come back as a tab.
	m2 := NewManager(message.NewManager(store), store, srv)
	if err := m2.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	restored := m2.Get(s.ID)
	if restored == nil {
		t.Fatal("expected restored session in registry")
	}
	for _, sh := range restored.ShellsForView() {
		if sh.ID == child.ID {
			t.Fatal("restored session resurrects a closed shell as a DEAD tab")
		}
	}
}

// Closing a shell is never a session event: a pipe container whose last shell is
// closed stays running and reusable, exactly like a PTY one. Forwards and SFTP
// ride the container's SSH transport, which no shell close may take away.
func TestManager_CloseLastPipeShellStaysRunning(t *testing.T) {
	srv := startTestServer(t)
	m := NewManager(nil, nil, srv)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "pipe"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Delete(s.ID)

	// Close the only (primary) shell directly. The web/MCP guards that no-op
	// internal-primary closes are a UI layer; the session contract is what's
	// under test.
	if err := s.CloseChildShell(s.PrimaryShellID()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := m.Get(s.ID).Info().Status; got != api.SessionRunning {
		t.Fatalf("pipe session must stay running after its last shell is closed, got %q", got)
	}
	// The shell is closed (deleted), so the live view has no retained tabs for it.
	if len(s.SnapshotShells()) != 0 {
		t.Fatal("snapshot should be empty: the closed shell is deleted, not retained")
	}
}

// Closing the last shell of a PTY container must NOT flip it DEAD; PTY
// containers remain reusable and stay running with an empty shell list.
func TestManager_CloseLastPTYShellStaysRunning(t *testing.T) {
	srv := startTestServer(t)
	m := NewManager(nil, nil, srv)

	s, err := m.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "pty"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Delete(s.ID)

	if err := s.CloseChildShell(s.PrimaryShellID()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := m.Get(s.ID).Info().Status; got != api.SessionRunning {
		t.Fatalf("pty session must stay running after its last shell is closed, got %q", got)
	}
	if len(s.ShellsForView()) != 0 {
		t.Fatal("pty session with zero shells must report an empty shell list")
	}
}

func TestAppendEnter(t *testing.T) {
	if got := appendEnter([]byte("ls"), false); string(got) != "ls\n" {
		t.Fatalf("unix: expected %q, got %q", "ls\n", got)
	}
	if got := appendEnter([]byte("dir"), true); string(got) != "dir\r\n" {
		t.Fatalf("windows: expected %q, got %q", "dir\r\n", got)
	}
	if got := appendEnter(nil, false); string(got) != "\n" {
		t.Fatalf("empty unix: expected %q, got %q", "\n", got)
	}
}

func TestSessionScopeReleasesIndependentSubsystems(t *testing.T) {
	srv := startTestServer(t)
	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)

	var mu sync.Mutex
	fired := map[string]int{}
	order := []string{}

	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}

	// Independent subsystems attach at their own creation points; Delete must
	// run every one of them exactly once, in LIFO order, with no central list.
	s.AttachCleanup(func() {
		mu.Lock()
		fired["forward"]++
		order = append(order, "forward")
		mu.Unlock()
	})
	s.AttachCleanup(func() {
		mu.Lock()
		fired["notify"]++
		order = append(order, "notify")
		mu.Unlock()
	})

	if err := m.Delete(s.ID); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if fired["forward"] != 1 || fired["notify"] != 1 {
		t.Fatalf("expected both attached resources once, got %v", fired)
	}
	if len(order) != 2 || order[0] != "notify" || order[1] != "forward" {
		t.Fatalf("expected LIFO release order [notify forward], got %v", order)
	}
}

// Shell resolution for a shell channel is: the caller's command, else the
// profile's default_shell, else — pty only — whatever the server picks as its
// login shell. Step three is deliberately not a client-side guess: the client's
// PATH describes the wrong machine for a remote target.
func TestResolveShellCommand_DefaultShellPriority(t *testing.T) {
	s := &Session{defaultShell: "my-shell -i -l"}

	if cmd, args := s.resolveShellCommand("", nil); cmd != "my-shell" || len(args) != 2 || args[0] != "-i" || args[1] != "-l" {
		t.Fatalf("empty command must fall back to default_shell, got %q %v", cmd, args)
	}
	if cmd, args := s.resolveShellCommand("explicit", []string{"x"}); cmd != "explicit" || len(args) != 1 || args[0] != "x" {
		t.Fatalf("explicit command must win, got %q %v", cmd, args)
	}
	// Args-only counts as explicit too: the caller named a program via args.
	if cmd, args := s.resolveShellCommand("  ", []string{"-c", "hi"}); cmd != "  " || len(args) != 2 {
		t.Fatalf("args-only input must not be replaced, got %q %v", cmd, args)
	}

	// No default_shell: both empty, so the transport decides.
	plain := &Session{}
	if cmd, args := plain.resolveShellCommand("", nil); cmd != "" || len(args) != 0 {
		t.Fatalf("without a default_shell the command must stay empty, got %q %v", cmd, args)
	}
}
