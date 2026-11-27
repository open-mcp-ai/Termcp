package extensions

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// This subprocess emits its diagnostic, then waits for a line and exits
// unsuccessfully. Its startup is independent of the clock.
func TestResidentRecoveryProcess(t *testing.T) {
	if os.Getenv("TERMCP_TEST_RESIDENT_RECOVERY") != "1" {
		return
	}
	fmt.Println("fixture diagnostic ready")
	if bufio.NewScanner(os.Stdin).Scan() {
		os.Exit(23)
	}
}

func TestOpenRecoversAnExitedResidentChannel(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMCP_TEST_RESIDENT_RECOVERY", "1")
	r := startReconciler(t)
	for _, name := range []string{"worker", "neighbor"} {
		// PTY mode execs argv directly. Pipe mode on Windows may select WSL's
		// bash, which cannot invoke os.Executable's native Windows path.
		ext := Extension{Mode: "pty", Resident: true}
		ext.Name, ext.Target, ext.Command = name, "internal", exe
		ext.Args = []string{"-test.run=^TestResidentRecoveryProcess$"}
		mustSave(t, r.store, &ext)
	}
	first := waitForStatus(t, r, "worker", StatusAvailable)
	neighbor := waitForStatus(t, r, "neighbor", StatusAvailable)
	sess := r.sessMgr.Get(first.SessionID)
	if sess == nil || first.SessionID != neighbor.SessionID {
		t.Fatal("expected both resident extensions on a shared session")
	}
	old := sess.GetChildShell(first.ShellID)
	if old == nil {
		t.Fatal("missing original worker")
	}
	// ConPTY can coalesce its final screen update with process exit. Observe
	// an actual output line before triggering exit so it is known to exist.
	for deadline := time.Now().Add(30 * time.Second); ; {
		b, _, err := old.OutputByteRange(0, 8192)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "fixture diagnostic ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture did not produce its diagnostic: %q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := old.SendTerminalBytesFrom([]byte("exit"), true, session.InputFromAI); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("fixture did not exit")
	}
	if code := old.Info().ExitCode; code == nil || *code != 23 {
		t.Fatalf("expected a process exit with code 23, got %v", code)
	}
	open := func() Status {
		t.Helper()
		got, err := r.Acquire("worker", 30*time.Second)
		if err != nil || got.Status != StatusAvailable {
			t.Fatalf("open after exit: %+v, %v", got, err)
		}
		return got
	}
	got := open()
	if got.SessionID != first.SessionID || got.ShellID == first.ShellID || liveShell(sess, got.ShellID) == nil {
		t.Fatalf("no replacement channel on the shared session: %+v", got)
	}
	if again := open(); again.ShellID != got.ShellID {
		t.Fatalf("opening the healthy resident restarted it: %+v", again)
	}
	if err := r.Retry("worker"); err != nil {
		t.Fatal(err)
	}
	if retried := waitForStatus(t, r, "worker", StatusAvailable); retried.ShellID != got.ShellID {
		t.Fatalf("retry restarted a healthy resident: %+v", retried)
	}
	// A channel already stopped and archived by the user needs no cleanup.
	// A later open must neither resurrect that handle nor erase its snapshot.
	previous := sess.GetChildShell(got.ShellID)
	if previous == nil {
		t.Fatal("missing recovered worker")
	}
	if _, err := r.sessMgr.ArchiveChildShell(previous.ID); err != nil {
		t.Fatal(err)
	}
	if reopened := open(); reopened.ShellID == previous.ID || liveShell(sess, reopened.ShellID) == nil {
		t.Fatalf("open after explicit archive did not make a fresh channel: %+v", reopened)
	}
	if snapshot, ok := sess.SnapshotShellByIndex(previous.Index); !ok || snapshot.ID != previous.ID {
		t.Fatalf("lost previously archived channel: %+v", snapshot)
	}
	if liveShell(sess, neighbor.ShellID) == nil {
		t.Fatal("recovery interrupted the other extension")
	}
	if snapshot, ok := sess.SnapshotShellByIndex(old.Index); !ok || snapshot.ID != old.ID || snapshot.Status != api.SessionExited {
		t.Fatalf("lost archived channel: %+v", snapshot)
	}
	b, _, err := r.msgMgr.OutputByteRange(sess.ID, old.ID, 0, 8192)
	if err != nil || !strings.Contains(string(b), "fixture diagnostic ready") {
		t.Fatalf("lost archived output: %q, %v", b, err)
	}
	// Load the same manifests a new manager would restore, independent of
	// the live session's in-memory shell history.
	records, err := storage.New(r.store.dataDir).LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.ID == sess.ID {
			for _, sh := range record.Shells {
				if sh.ID == old.ID && sh.Status == api.SessionExited && sh.ExitCode != nil && *sh.ExitCode == 23 {
					return
				}
			}
		}
	}
	t.Fatal("exited shell not retained on disk for restore")
}
