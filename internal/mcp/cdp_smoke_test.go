package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func findChrome(t *testing.T) string {
	for _, n := range []string{"chrome", "google-chrome", "chromium", "msedge"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	for _, p := range []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestRealBrowserThroughTheCdpDoor is the check a fake peer cannot be: a real
// browser, reached through the real door, answering the calls a page probe
// actually starts with. The fakes pin the protocol's shape; this pins that the
// shape is Chrome's.
func TestRealBrowserThroughTheCdpDoor(t *testing.T) {
	chrome := findChrome(t)
	if chrome == "" {
		t.Skip("no Chrome or Edge installed")
	}
	// NOT t.TempDir(): Chrome's crashpad helper holds CrashpadMetrics-active.pma
	// for seconds after the browser exits, and TempDir's cleanup runs the instant
	// the test returns, so it fails the TEST with an "Access is denied" unlink
	// that says nothing about what was being tested. MkdirTemp plus a tolerant
	// cleanup is the same treatment internal/webui's browser probe uses.
	profile, err := os.MkdirTemp("", "termcp-cdp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for deadline := time.Now().Add(15 * time.Second); ; {
			if err := os.RemoveAll(profile); err == nil || time.Now().After(deadline) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	cmd := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-sandbox",
		"--no-first-run", "--remote-debugging-port=0", "--user-data-dir="+profile, "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// DevToolsActivePort: line 1 is the port the browser chose, which is what
	// makes --remote-debugging-port=0 usable without guessing a free one.
	var port string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort")); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) == 2 {
				port = strings.TrimSpace(lines[0])
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if port == "" {
		t.Fatal("browser never wrote DevToolsActivePort")
	}

	s := newTestServer(t)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "cdp open http://127.0.0.1:" + port}))
	defer termText(t, s, map[string]any{"cmd": "cdp close " + h})

	targets := termText(t, s, map[string]any{"cmd": `cdp call ` + h + ` {"method":"Target.getTargets"}`})
	if !strings.Contains(targets, "targetInfos") {
		t.Fatalf("Target.getTargets failed:\n%s", targets)
	}

	created := termText(t, s, map[string]any{"cmd": `cdp call ` + h + ` {"method":"Target.createTarget","params":{"url":"about:blank"}}`})
	if !strings.Contains(created, "targetId") {
		t.Fatalf("Target.createTarget failed:\n%s", created)
	}
}
