package sshserver

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestAQuotedCommandLineIsNamedAsOne pins issue #208.2: the failure text for
// `--argv "cmd /c echo A"` pointed at PATH, where the answer was never going to be.
func TestAQuotedCommandLineIsNamedAsOne(t *testing.T) {
	cmd := exec.Command("definitely not a program anywhere 12345")
	err := cmd.Run()
	if err == nil {
		t.Skip("the bogus program unexpectedly exists")
	}
	got := describeStartError(cmd, err)
	if !strings.Contains(got, "argv") || !strings.Contains(got, "sh -c $1") {
		t.Errorf("the failure does not name the mistake or the fix:\n%s", got)
	}
}

// A program that exists is left alone, spaces or not: the check is a bound, not a
// rewrite of every exec failure.
func TestARealProgramPathIsNotSecondGuessed(t *testing.T) {
	plain := errors.New("exit status 3")
	cmd := exec.Command("echo")
	if got := describeStartError(cmd, plain); got != plain.Error() {
		t.Errorf("a plain failure was annotated:\n%s", got)
	}
}
