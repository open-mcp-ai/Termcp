package mcp

import (
	"errors"
	"os"
	"testing"
)

// TestFileErrClassifiesFailures pins the fix for the "every file failure is a 500"
// problem: a missing path is a 404, a permission refusal is a 403, and only a real
// fault stays operation_failed.
func TestFileErrClassifiesFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"os not exist", os.ErrNotExist, CodeFileNotFound},
		{"wrapped not exist", &os.PathError{Op: "stat", Path: "/nope", Err: os.ErrNotExist}, CodeFileNotFound},
		{"permission", os.ErrPermission, CodeForbidden},
		{"wrapped permission", &os.PathError{Op: "open", Path: "/root/x", Err: os.ErrPermission}, CodeForbidden},
		{"generic failure", errors.New("connection reset"), CodeOperationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := fileErr(tc.err)
			if !res.IsError {
				t.Fatal("fileErr must return an error result")
			}
			fail := failureFromResult(res)
			if fail == nil {
				t.Fatal("failureFromResult must not be nil")
			}
			if fail.code != tc.want {
				t.Errorf("fail.code = %q, want %q (msg: %s)", fail.code, tc.want, fail.msg)
			}
		})
	}
}

// TestFileNotFoundMapsTo404 pins the HTTP mapping.
func TestFileNotFoundMapsTo404(t *testing.T) {
	if got := statusFor(CodeFileNotFound); got != 404 {
		t.Errorf("statusFor(%q) = %d, want 404", CodeFileNotFound, got)
	}
}
