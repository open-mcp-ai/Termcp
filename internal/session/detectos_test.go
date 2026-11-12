package session

// Issue #318: a session says what operating system is on the other side.
//
// The classifier is the part that can be wrong in a way a caller notices — a Windows node
// reported as "other" puts the caller back to guessing — so it is pinned per platform
// spelling. The wiring is pinned by starting a real loopback session.

import (
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

func TestClassifyOSKnowsTheSpellings(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		// `uname -s` on the platforms termcp is built for.
		{"Linux\n", "linux"},
		{"Darwin\n", "darwin"},
		{"FreeBSD\n", "bsd"},
		{"OpenBSD\n", "bsd"},
		{"SunOS\n", "solaris"},
		// Windows, as each shell spells it: `ver`, and Git Bash's uname.
		{"Microsoft Windows [Version 10.0.26200.8037]\r\n", "windows"},
		{"MINGW64_NT-10.0-26200\n", "windows"},
		{"MSYS_NT-10.0\n", "windows"},
		{"CYGWIN_NT-10.0\n", "windows"},
		// A host that answered with something new is "other", not a guess.
		{"Plan9\n", "other"},
		{"", ""},
		{"\n", ""},
	} {
		if got := classifyOS(tc.out); got != tc.want {
			t.Errorf("classifyOS(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
}

// An internal session IS this machine, so the answer is recorded without a round trip —
// and it is recorded at all, which is the half a caller notices.
func TestLocalSessionRecordsItsOS(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "os-probe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	info := s.Info()
	if info.OS != localOS() {
		t.Fatalf("fresh internal session OS=%q, want %q", info.OS, localOS())
	}
	if info.OSSource != "local" {
		t.Fatalf("OSSource=%q, want local", info.OSSource)
	}
}

// A session with no transport must not invent a platform, and SetOS is how an operator's
// answer arrives without a probe.
func TestSetOSAndNoTransport(t *testing.T) {
	s := &Session{}
	s.DetectOS()
	if s.Info().OS != "" {
		t.Fatalf("a session with no transport claimed OS=%q", s.Info().OS)
	}

	s.SetOS(" Linux ", "config")
	info := s.Info()
	if info.OS != "linux" || info.OSSource != "config" {
		t.Fatalf("SetOS recorded OS=%q source=%q, want linux/config", info.OS, info.OSSource)
	}

	// An empty answer is not an answer: the field keeps what it had rather than
	// overwriting a fact with nothing.
	s.SetOS("", "probe")
	if got := s.Info().OS; got != "linux" {
		t.Fatalf("an empty SetOS cleared OS to %q", got)
	}
}
