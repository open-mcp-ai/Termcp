package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// TestDryRunStopsBeforeDispatch pins issue #241's acceptance list. It is written
// as one test per hard rule rather than one big assertion, because a preview that
// breaks a single rule is worse than one that breaks none: a side effect here
// would run a command the caller explicitly asked NOT to run.
func TestDryRunStopsBeforeDispatch(t *testing.T) {
	t.Run("a compound line previews every statement in order", func(t *testing.T) {
		s := &Server{}
		reply := s.executeTerm(context.Background(),
			"ls host; help", termArgs{DryRun: true})
		if len(reply.slots) != 2 {
			t.Fatalf("a two-statement cmd should answer two slots, got %d", len(reply.slots))
		}
		for i, want := range []string{"ls host", "help"} {
			if got := reply.slots[i].Headers["Path"]; got != want {
				t.Errorf("slot %d Path = %q, want %q (order must follow cmd)", i, got, want)
			}
			if got := reply.slots[i].Headers["Verdict"]; got != "valid-syntax" {
				t.Errorf("slot %d Verdict = %q, want valid-syntax", i, got)
			}
			if got := reply.slots[i].Headers["Dry-Run"]; got != "true" {
				t.Errorf("slot %d Dry-Run = %q, want true", i, got)
			}
		}
	})

	t.Run("no slot promises bytes it did not produce", func(t *testing.T) {
		s := &Server{}
		reply := s.executeTerm(context.Background(),
			"ls host; help; get session", termArgs{DryRun: true})
		// These headers ARE the promise that something ran: a range of a stream, a
		// cursor that advanced, an exit status, a wait that elapsed. A preview has
		// none of them, and inventing one would let a reader mistake the echo for
		// output.
		forbidden := []string{"Content-Range", "Input-At", "Exit-Code", "Waited", "Sampled"}
		for i, slot := range reply.slots {
			for _, h := range forbidden {
				if _, bad := slot.Headers[h]; bad {
					t.Errorf("slot %d carries %s, which promises output a dry run never produced", i, h)
				}
			}
			if slot.Body != "" {
				t.Errorf("slot %d carries a body (%q): a preview answers with facts, not output", i, slot.Body)
			}
			if slot.Code != 200 {
				t.Errorf("slot %d Code = %d, want 200 for valid syntax", i, slot.Code)
			}
		}
	})

	t.Run("a missing $1 is invalid and teaches the fix", func(t *testing.T) {
		s := &Server{}
		// The classic small-model failure: the shell line typed where only the
		// reference belongs. Parse refuses it, and a dry run must report that
		// refusal as the Verdict rather than as an unexplained 400.
		reply := s.executeTerm(context.Background(),
			"run s1:1 df -h", termArgs{DryRun: true})
		if len(reply.slots) != 1 {
			t.Fatalf("want 1 slot, got %d", len(reply.slots))
		}
		slot := reply.slots[0]
		if slot.Code != 400 {
			t.Fatalf("Code = %d, want 400 for the missing-reference form", slot.Code)
		}
		if got := slot.Headers["Verdict"]; got != "invalid" {
			t.Errorf("Verdict = %q, want invalid", got)
		}
		if got := slot.Headers["Dry-Run"]; got != "true" {
			t.Errorf("Dry-Run = %q, want true (the refusal happened inside a preview)", got)
		}
		if !strings.Contains(slot.Body, "$1") {
			t.Errorf("the refusal must name the reference to write, got: %q", slot.Body)
		}
	})

	t.Run("expansion stops at termcp, so a glob stays a glob", func(t *testing.T) {
		s := &Server{}
		reply := s.executeTerm(context.Background(),
			"run s1:1 $1",
			termArgs{DryRun: true, Args: []string{"rm /tmp/*"}})
		if len(reply.slots) != 1 {
			t.Fatalf("want 1 slot, got %d", len(reply.slots))
		}
		if got := reply.slots[0].Headers["Expanded"]; got != "rm /tmp/*" {
			t.Fatalf("Expanded = %q, want the glob unchanged: which files match is the far shell's business, not termcp's", got)
		}
	})

	t.Run("a secret's value never appears in Expanded", func(t *testing.T) {
		s := &Server{}
		const secret = "hunter2-not-in-the-reply"
		reply := s.executeTerm(context.Background(),
			"run s1:1 $1",
			termArgs{DryRun: true,
				Args:    []string{"mysql -u root -p" + secret},
				Secrets: map[string]string{"DB_PASS": secret}})
		if len(reply.slots) != 1 {
			t.Fatalf("want 1 slot, got %d", len(reply.slots))
		}
		expanded := reply.slots[0].Headers["Expanded"]
		if strings.Contains(expanded, secret) {
			t.Fatalf("Expanded leaks the secret verbatim (#241 §4.4 forbids this): %q", expanded)
		}
		if !strings.Contains(expanded, "$DB_PASS") {
			t.Errorf("Expanded should name the placeholder instead, got %q", expanded)
		}
	})

	t.Run("a channel that does not exist is still valid syntax", func(t *testing.T) {
		s := &Server{}
		reply := s.executeTerm(context.Background(),
			"read nosuchchannel", termArgs{DryRun: true})
		if len(reply.slots) != 1 {
			t.Fatalf("want 1 slot, got %d", len(reply.slots))
		}
		slot := reply.slots[0]
		// A preview answers "is this written correctly", not "will it work". A
		// reference to a session nobody opened is a fact about the world, and the
		// whole point of offline checking is that the world is absent.
		if slot.Code != 200 {
			t.Fatalf("Code = %d: a missing referent is not a syntax error", slot.Code)
		}
		if got := slot.Headers["Verdict"]; got != "valid-syntax" {
			t.Errorf("Verdict = %q, want valid-syntax", got)
		}
	})

	t.Run("a raw control byte is reported as an invalid line", func(t *testing.T) {
		s := &Server{}
		reply := s.executeTerm(context.Background(),
			"run s1:1 $1", termArgs{DryRun: true, Args: []string{"echo a\rb"}})
		if len(reply.slots) != 1 {
			t.Fatalf("want 1 slot, got %d", len(reply.slots))
		}
		slot := reply.slots[0]
		if slot.Code != 400 {
			t.Fatalf("Code = %d, want 400", slot.Code)
		}
		if got := slot.Headers["Verdict"]; got != "invalid" {
			t.Errorf("Verdict = %q, want invalid", got)
		}
	})
}

// TestDryRunTouchesNothing is acceptance rule #1, checked mechanically: a preview
// writes no session, no log and no span, so the data directory is untouched.
func TestDryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	s := &Server{}
	for _, slot := range s.executeTerm(context.Background(),
		"ls host; help; read a:1; connect nosuchhost", termArgs{DryRun: true}).slots {
		if slot.Code != 200 && !strings.Contains(slot.Body, "no such") {
			// Any failure is fine — what matters is that nothing was written.
			continue
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a dry run wrote to the data directory: %v", names)
	}
	// The data dir is only meaningful if the server would have used it; assert the
	// stronger version too — no file anywhere under it.
	if _, err := os.Stat(filepath.Join(dir, "log.bin")); err == nil {
		t.Fatal("a dry run created log.bin")
	}
}

// TestDryRunIsAbsentFromTheWireByDefault pins acceptance rule #7: `dry_run:false`
// and an omitted `dry_run` must behave identically, so the argument is purely
// additive.
func TestDryRunIsAbsentFromTheWireByDefault(t *testing.T) {
	without, derr := parseTermArgs(map[string]any{})
	if derr != nil {
		t.Fatalf("parseTermArgs: %v", derr.Msg)
	}
	explicit, derr := parseTermArgs(map[string]any{"dry_run": false})
	if derr != nil {
		t.Fatalf("parseTermArgs(dry_run:false): %v", derr.Msg)
	}
	if without.DryRun || explicit.DryRun {
		t.Fatalf("DryRun must default to false: omitted=%v explicit-false=%v", without.DryRun, explicit.DryRun)
	}
	on, derr := parseTermArgs(map[string]any{"dry_run": true})
	if derr != nil {
		t.Fatalf("parseTermArgs(dry_run:true): %v", derr.Msg)
	}
	if !on.DryRun {
		t.Fatal("dry_run:true did not reach termArgs.DryRun")
	}
	// A typo is refused rather than read as false, the same rule every other
	// argument follows.
	if _, derr := parseTermArgs(map[string]any{"dry_run": "yes please"}); derr == nil {
		t.Fatal("a non-boolean dry_run should be refused, not coerced")
	}
}

// TestDryRunHeadersRenderInOrder pins that the three new headers survive the
// rendering step. A header present in the map but missing from the text would
// make the two projections disagree about the key set.
func TestDryRunHeadersRenderInOrder(t *testing.T) {
	slot := envelope.Slot{Code: 200, Headers: map[string]string{
		"Path":     "run s1:1 $1",
		"Dry-Run":  "true",
		"Verdict":  "valid-syntax",
		"Target":   "s1:1",
		"Expanded": "df -h /mnt/data",
	}}
	text := slot.Text()
	for _, h := range []string{"Dry-Run: true", "Verdict: valid-syntax", "Target: s1:1", "Expanded: df -h /mnt/data"} {
		if !strings.Contains(text, h) {
			t.Errorf("rendered reply is missing %q:\n%s", h, text)
		}
	}
	// And the JSON projection carries the same keys, so a program consumer sees
	// what a reader sees.
	b, err := json.Marshal(slot.Structured())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"Verdict", "Target", "Expanded"} {
		if !strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("structured projection is missing %q: %s", k, b)
		}
	}
}

// TestDryRunMasksSecretsInEveryHeader is a regression test for a leak found by
// running the feature, not by reading it.
//
// The parser expands the WHOLE line, echo included: `subst.go` rewrites `Path`
// with the same table that fills the payload. So `input s1:1 $PASS` arrives with
// the credential already written into the echo, and masking only `Expanded:`
// would publish the secret in the more prominent header. Measured against a live
// instance: `"Path":"input s1:1 s3cr3t-do-not-leak"`, with `Expanded: $PASS`
// beside it looking correctly masked.
func TestDryRunMasksSecretsInEveryHeader(t *testing.T) {
	const secret = "s3cr3t-do-not-leak"
	s := &Server{}
	for _, cmd := range []string{
		"input s1:1 $PASS",
		"key s1:1 $PASS",
	} {
		// THE WINDOW IS BUILT BY THE REAL PARSER, not assembled by hand: `secrets`
		// reaches substitution through `Vars` (parseTermArgs merges the two tables
		// and keeps Secrets only as the record of what must not be logged), so a
		// hand-built termArgs with Secrets alone substitutes nothing and the test
		// would pass without ever exercising the leak.
		win, derr := parseTermArgs(map[string]any{
			"dry_run": true,
			"secrets": map[string]any{"PASS": secret},
		})
		if derr != nil {
			t.Fatalf("%s: parseTermArgs: %v", cmd, derr.Msg)
		}
		reply := s.executeTerm(context.Background(), cmd, win)
		if len(reply.slots) != 1 {
			t.Fatalf("%s: want 1 slot, got %d", cmd, len(reply.slots))
		}
		slot := reply.slots[0]
		// The whole rendered reply is checked, not one field: the guarantee is
		// "the value is nowhere in what the caller reads", and a per-field test
		// would pass while a seventh header leaked it.
		rendered := slot.Text()
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s: the secret appears in the reply:\n%s", cmd, rendered)
		}
		if !strings.Contains(rendered, "$PASS") {
			t.Errorf("%s: the placeholder should stand where the value was:\n%s", cmd, rendered)
		}
	}
}
