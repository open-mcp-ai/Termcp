package session

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// testSizeCommand returns the command that makes the child report its terminal
// geometry as "<rows> <cols>", plus the marker the size is wrapped in.
//
// Windows has no stty: PowerShell reads the ConPTY geometry off the console
// API instead. The marker wraps the two numbers and the numbers are written as
// separate output items, so the shell's own echoing of the command line can
// never be mistaken for the answer — the tests assert on the marker, not on a
// bare "24 80" that appears verbatim in the command it just echoed.
func testSizeCommand() (cmd, marker string) {
	if runtime.GOOS == "windows" {
		return `Write-Output "SIZE=$([console]::WindowHeight) $([console]::WindowWidth)"`, "SIZE="
	}
	// stty prints "<rows> <cols>"; wrap it in the same marker shape.
	return `echo "SIZE=$(stty size)"`, "SIZE="
}

// TestSession_PtyResizeReachesChild locks the window-change contract: a resize
// requested by the client must reach the child's terminal, so the child's own
// geometry read reports the new size. The internal SSH server hands every
// window-change to charmbracelet/ssh's own resize handling; draining the
// library's window channel itself silently discarded ~half of all resizes.
//
// On Windows this also guards the conpty cache race fix: the window is applied
// with ResizePseudoConsole directly rather than through Pty.Resize, whose
// geometry cache is written without a lock by both window consumers (see
// applyWindow in internal/sshserver).
func TestSession_PtyResizeReachesChild(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	time.Sleep(300 * time.Millisecond)

	sizeCmd, marker := testSizeCommand()

	// askSize runs the size command in the shell and returns until the child
	// reports exactly rows x cols (empty when it never does).
	askSize := func(rows, cols int) string {
		t.Helper()
		want := marker + fmt.Sprintf("%d %d", rows, cols)
		var out string
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := s.PrimaryShell().SendTerminalBytes([]byte(testShellInput(sizeCmd)), false); err != nil {
				t.Fatalf("send size command: %v", err)
			}
			chunk, _ := s.ReadOutput(context.Background(), 300*time.Millisecond, true, 0, 0)
			out += chunk
			if strings.Contains(out, want) {
				return out
			}
		}
		return out
	}

	// Initial geometry is the one the session was created with.
	if out := askSize(24, 80); !strings.Contains(out, marker+"24 80") {
		t.Fatalf("expected initial tty size 24 80, got %q", out)
	}

	// Each resize must be observable by the child, not just tracked client-side.
	for _, size := range []struct{ rows, cols int }{{30, 100}, {40, 110}, {50, 120}} {
		if err := s.PrimaryShell().ResizePty(size.rows, size.cols); err != nil {
			t.Fatalf("resize %dx%d: %v", size.rows, size.cols, err)
		}
		if out := askSize(size.rows, size.cols); !strings.Contains(out, marker+fmt.Sprintf("%d %d", size.rows, size.cols)) {
			t.Fatalf("window-change %dx%d did not reach the child tty, got %q", size.rows, size.cols, out)
		}
	}
}
