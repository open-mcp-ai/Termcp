package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBatchCreateFiles(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(target, manifest string, preview bool) []int {
		t.Helper()
		cmd := "write " + target + " file $1 --op batch-create --data $2"
		if preview {
			cmd += " --dry-run"
		}
		r := s.Term(t.Context(), cmd, map[string]any{"args": []any{filepath.ToSlash(dir), manifest}})
		codes := make([]int, len(r.Slots))
		for i, slot := range r.Slots {
			codes[i] = slot.Code
			t.Log(slot.Code, slot.Body)
		}
		return codes
	}
	check := func(got, want []int) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("codes %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("codes %v, want %v", got, want)
			}
		}
	}
	const manifest = `[["existing.txt","changed"],["new.txt","line\n2"],["empty",""]]`
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	check(run(sid, manifest, true), []int{200, 200, 200})
	if es, err := os.ReadDir(dir); err != nil || len(es) != 1 {
		t.Fatalf("preview changed directory: %v %v", es, err)
	}
	for _, m := range []string{`[["first","x"],["../outside","x"]]`, `[["first","x"],["FIRST","y"]]`, `[["CON.txt","x"]]`, `[["file",null]]`} {
		check(run(sid, m, false), []int{400})
	}
	if _, err := os.Stat(filepath.Join(dir, "first")); !os.IsNotExist(err) {
		t.Fatalf("invalid manifest partially executed: %v", err)
	}
	check(run(sid, manifest, false), []int{409, 200, 200})
	for name, want := range map[string]string{"existing.txt": "keep", "new.txt": "line\n2", "empty": ""} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	restricted := guardTestSession(t, s, []string{"file_write"}, []string{filepath.ToSlash(filepath.Join(dir, "allowed.txt"))})
	check(run(restricted, `[["allowed.txt","ok"],["denied.txt","no"]]`, false), []int{200, 403})
	if _, err := os.Stat(filepath.Join(dir, "denied.txt")); !os.IsNotExist(err) {
		t.Fatal("denied path was written")
	}

	gated, _ := startGatedSession(t, s, 1, time.Minute)
	check(run(gated, `[["held","x"]]`, true), []int{200})
	q := s.sessMgr.Get(gated).ApprovalQueue()
	if len(q.List()) != 0 {
		t.Fatal("preview queued an approval")
	}
	check(run(gated, `[["held","x"]]`, false), []int{202})
	if len(q.List()) != 1 {
		t.Fatal("expected an approval")
	}
	if _, err := os.Stat(filepath.Join(dir, "held")); !os.IsNotExist(err) {
		t.Fatal("held request wrote a file")
	}
	if !strings.Contains(string(q.List()[0].Payload), `"create_only":true`) {
		t.Fatal("approval lost the exclusive create operation")
	}
	// Simulate another writer winning while a human considers the request.
	if err := os.WriteFile(filepath.Join(dir, "held"), []byte("other writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ExecuteApprovedOperation(q.List()[0]); err == nil {
		t.Fatal("approval replay overwrote an existing target")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "held")); err != nil || string(b) != "other writer" {
		t.Fatalf("approval replay changed winner: %q %v", b, err)
	}

	start := make(chan struct{})
	done := make(chan int, 2)
	for _, text := range []string{"one", "two"} {
		go func() {
			<-start
			m, _ := json.Marshal([][]string{{"race", text}})
			res := s.Term(t.Context(), "write "+sid+" file $1 --op batch-create --data $2", map[string]any{"args": []any{filepath.ToSlash(dir), string(m)}})
			done <- res.Slots[0].Code
		}()
	}
	close(start)
	a, b := <-done, <-done
	if (a != 200 || b != 409) && (a != 409 || b != 200) {
		t.Fatalf("concurrent creates: %d %d", a, b)
	}
	if saved, err := os.ReadFile(filepath.Join(dir, "race")); err != nil || (string(saved) != "one" && string(saved) != "two") {
		t.Fatalf("invalid race winner: %q %v", saved, err)
	}
}

func TestBatchManifestLimits(t *testing.T) {
	if _, err := parseCreateManifest(`[["a","` + strings.Repeat("x", maxBatchManifest) + `"]]`); err == nil {
		t.Fatal("accepted oversize manifest")
	}
	var tooMany [][]string
	for i := 0; i < 33; i++ {
		tooMany = append(tooMany, []string{fmt.Sprintf("file-%d", i), ""})
	}
	raw, _ := json.Marshal(tooMany)
	if _, err := parseCreateManifest(string(raw)); err == nil {
		t.Fatal("accepted 33 children")
	}
}

func TestBatchCreateSymlinksAndPreviewMisuse(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source")
	final := filepath.Join(dir, "final")
	for name, data := range map[string]string{source: "input", final: "preserve"} {
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []string{"write " + sid + " file $1 --dry-run --data ignored", "read " + sid + " file $2 | write " + sid + " file $1 --dry-run"} {
		args := []any{final}
		if strings.Contains(cmd, "$2") {
			args = append(args, source)
		}
		reply := s.Term(t.Context(), cmd, map[string]any{"args": args})
		if reply.Slots[0].Code != 400 {
			t.Fatalf("unexpected preview reply %+v", reply.Slots)
		}
	}
	if b, err := os.ReadFile(final); err != nil || string(b) != "preserve" {
		t.Fatal("unsupported preview wrote bytes")
	}
	link := filepath.Join(dir, "linked")
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reply := s.Term(t.Context(), "write "+sid+" file $1 --op batch-create --data $2", map[string]any{"args": []any{filepath.ToSlash(link), `[["unexpected","x"]]`}})
	if reply.Slots[0].Code < 400 {
		t.Fatalf("accepted linked directory: %+v", reply.Slots)
	}
	if _, err := os.Stat(filepath.Join(outside, "unexpected")); !os.IsNotExist(err) {
		t.Fatalf("wrote through a linked directory: %v", err)
	}
	reply = s.Term(t.Context(), "write "+sid+" file $1 --op batch-create --data $2", map[string]any{"args": []any{filepath.ToSlash(dir), `[["linked","x"]]`}})
	if reply.Slots[0].Code != 409 {
		t.Fatalf("expected conflict on a linked child: %+v", reply.Slots)
	}
}
