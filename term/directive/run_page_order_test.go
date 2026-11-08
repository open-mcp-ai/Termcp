package directive

import (
	"strings"
	"testing"
)

// TestTheRunPageAnswersBlockingFirst.
//
// A first-time caller read the whole `help run` page looking for "will it block?" and
// found the answer in the last paragraph, after two sections about `$1` substitution
// they did not need yet. The question every caller has BEFORE their first `run` is
// whether the call comes back; the substitution rules are what you read once you are
// already running.
func TestTheRunPageAnswersBlockingFirst(t *testing.T) {
	when := strings.Index(runDetail, "WHEN THIS CALL RETURNS")
	if when < 0 {
		t.Fatal("the run page no longer opens with the completion answer")
	}
	for _, later := range []string{"SENDING SEVERAL SHELL COMMANDS", "$0 is refused"} {
		if i := strings.Index(runDetail, later); i >= 0 && i < when {
			t.Errorf("%q comes before the answer to \"does this call return?\"", later)
		}
	}
	// The range on a run reply is where the caller sees it, so the explanation has to
	// be on this page and not only in the protocol reference.
	if !strings.Contains(runDetail, "numbers the CHANNEL's stream") {
		t.Error("the run page does not say what Content-Range measures")
	}
}
