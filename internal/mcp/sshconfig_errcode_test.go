package mcp

import (
	"errors"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// A profile the caller wrote badly is their argument, not a server fault. These
// checks already refused the write, but returned bare fmt.Errorf values, so every
// one of them surfaced as 500 operation_failed — which reads like an outage and
// invites a retry that cannot succeed.
func TestInvalidProfileContentIsABadRequest(t *testing.T) {
	cases := []string{
		`kind = "remote"
host = ""
user = "u"
password = "p"`,
		`kind = "remote"
host = "h"
user = ""
password = "p"`,
		`kind = "remote"
host = "h"
user = "u"`,
	}
	for _, body := range cases {
		_, err := sshconfig.ParseAndValidate([]byte(body))
		if err == nil {
			t.Fatalf("expected validation to fail for:\n%s", body)
		}
		if !errors.Is(err, sshconfig.ErrInvalid) {
			t.Errorf("err = %v, want it to wrap ErrInvalid", err)
		}
		if got := sshConfigErrCode(err); got != CodeInvalidArgument {
			t.Errorf("code = %q, want %q (err: %v)", got, CodeInvalidArgument, err)
		}
	}
}

// A missing profile is still its own answer: 404-ish, not a bad request.
func TestMissingProfileKeepsItsOwnCode(t *testing.T) {
	err := sshconfig.ErrNotFound
	if got := sshConfigErrCode(err); got == CodeInvalidArgument {
		t.Fatalf("a missing profile was reported as a bad request")
	}
}
