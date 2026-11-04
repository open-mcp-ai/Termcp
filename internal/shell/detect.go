package shell

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Detector detects the available shell on the target system.
// Fields are injectable for testing; use NewDetector() for production.
type Detector struct {
	LookPath func(string) (string, error)
	Getenv   func(string) string
	Stat     func(string) error
	GOOS     string
	ComSpec  string
}

// NewDetector returns a Detector backed by real OS calls.
func NewDetector() *Detector {
	return &Detector{
		LookPath: exec.LookPath,
		Getenv:   os.Getenv,
		Stat: func(path string) error {
			_, err := os.Stat(path)
			return err
		},
		GOOS:    runtime.GOOS,
		ComSpec: os.Getenv("COMSPEC"),
	}
}

// Detect returns the shell path, family, and a hint for agents.
func (d *Detector) Detect() (path, family, hint string) {
	if d.GOOS == "windows" {
		return d.detectWindows()
	}
	path, _, hint = d.detectUnix()
	if path == "" {
		return "", "", hint
	}
	return path, "unix", hint
}

// CommandLine wraps a raw exec-request command line into the invocation a
// login shell on this platform uses, so loopback answers the exact same
// question as a remote host: `login_shell -c "<payload>"` — pipes, redirects
// and variables expand, exactly as OpenSSH specifies.
//
// The returned env belongs to THIS shell choice and is for the spawned child
// ONLY: on Windows, sh.exe (Git/MSYS2) rewrites arguments that look like Unix
// paths ("/c" → "C:\"), which corrupts the client's words; the MSYS_* vars
// switch that conversion off for the child alone. Nothing here mutates the
// termcp process or any other process. Callers with no POSIX shell on PATH get
// the cmd.exe fallback and a nil env.
func (d *Detector) CommandLine(command string) (path string, args []string, env []string) {
	if d.GOOS == "windows" {
		for _, sh := range []string{"sh.exe", "bash.exe"} {
			if p, err := d.LookPath(sh); err == nil {
				return p, []string{"-c", command}, []string{
					"MSYS_NO_PATHCONV=1", "MSYS2_ARG_CONV_EXCL=*",
				}
			}
		}
		p := d.ComSpec
		if p == "" {
			p = "cmd.exe"
		}
		return p, []string{"/c", command}, nil
	}
	p, shArgs, _ := d.detectUnix()
	if p == "" {
		return "/bin/sh", []string{"-c", command}, nil
	}
	return p, append(append([]string{}, shArgs...), "-c", command), nil
}

// Argv returns the default shell argv (path + optional args) using the same

// priority as Detect. Use this when spawning a shell, so that the actual
// process matches what Detect advertises to agents.
func (d *Detector) Argv() (path string, args []string) {
	if d.GOOS == "windows" {
		p, _, _ := d.detectWindows()
		if p == "" {
			return "cmd.exe", nil
		}
		return p, nil
	}
	p, args, _ := d.detectUnix()
	if p == "" {
		return "/bin/sh", nil
	}
	return p, args
}

func (d *Detector) detectWindows() (path, family, hint string) {
	for _, sh := range []string{"pwsh.exe", "powershell.exe", "cmd.exe"} {
		if p, err := d.LookPath(sh); err == nil {
			family := "powershell"
			if sh == "cmd.exe" {
				family = "cmd"
			}
			return p, family, fmt.Sprintf("Found: %s", sh)
		}
	}
	return "", "", "No shell found on Windows"
}

func (d *Detector) detectUnix() (path string, args []string, hint string) {
	if shell := d.Getenv("SHELL"); shell != "" {
		if fields := strings.Fields(shell); len(fields) > 0 {
			if p, err := d.LookPath(fields[0]); err == nil {
				return p, fields[1:], fmt.Sprintf("$SHELL: %s", shell)
			}
		}
	}
	for _, sh := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if err := d.Stat(sh); err == nil {
			return sh, nil, fmt.Sprintf("Default: %s", sh)
		}
	}
	return "", nil, "No shell found"
}
