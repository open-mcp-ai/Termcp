package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	filesftp "github.com/open-mcp-ai/termcp/internal/sftp"
)

// A WRITE'S RECEIPT PROVES ITSELF (issue #357 §1.4).
//
// `bytes_written: 9` next to a sha256 the caller cannot check is still a claim. `--verify`
// is the flag that makes the FAR SIDE answer: it reads the bytes back and compares their
// digest to the one the write computed, which is the one question a local tool cannot
// answer at all — "did the copy arrive intact on the other machine".
func TestWriteVerifyProvesTheBytesOnTheHost(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	target := filepath.Join(t.TempDir(), "verified.txt")
	payload := "verify-these-bytes"

	got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file " + target + " --data " + payload + " --verify", "timeout": 15,
	})
	if !strings.Contains(got, "verified: true") {
		t.Fatalf("--verify did not report a verified write:\n%s", got)
	}
	// THE DIGEST IS CHECKED AGAINST AN INDEPENDENT COMPUTATION, not against the reply's
	// own other field: two equal fields prove nothing if both are wrong.
	sum := sha256.Sum256([]byte(payload))
	want := hex.EncodeToString(sum[:])
	if !strings.Contains(got, want) {
		t.Errorf("the receipt's sha256 is not the digest of the payload (%s):\n%s", want, got)
	}
	if !strings.Contains(got, "total_size:") {
		t.Errorf("the receipt does not report the file's size on the host:\n%s", got)
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != payload {
		t.Errorf("the file holds %q, want %q", onDisk, payload)
	}
}

// `get --op sha256` IS THE OTHER HALF: hashing a file that was written earlier, from the
// side that wants to compare against its own copy.
func TestGetSha256MatchesAnIndependentDigest(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	target := filepath.Join(t.TempDir(), "hashme.txt")
	body := "hash this file\n"
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := termText(t, s, map[string]any{
		"cmd": "get " + sid + " file " + target + " --op sha256", "timeout": 15,
	})
	sum := sha256.Sum256([]byte(body))
	if !strings.Contains(got, hex.EncodeToString(sum[:])) {
		t.Fatalf("--op sha256 does not match the file's digest:\n%s", got)
	}
	if !strings.Contains(got, "bytes_hashed:") {
		t.Errorf("the hash does not report how many bytes it covered:\n%s", got)
	}
}

// A PARTIAL WRITE HASHES ITS OWN BLOCK, which is the difference between a check that
// passes and a check that reports a false alarm on every `--offset` write.
func TestAVerifiedPartialWriteHashesOnlyItsBlock(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	target := filepath.Join(t.TempDir(), "partial.txt")
	if err := os.WriteFile(target, []byte("AAAAAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file " + target + " --offset 0 --data BBB --verify", "timeout": 15,
	})
	if !strings.Contains(got, "verified: true") {
		t.Fatalf("a correct partial write failed verification:\n%s", got)
	}
	// AND THE FILE IS THE MIXTURE THE WRITE DESCRIBED, with the untouched tail intact.
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != "BBBAAAAAAA" {
		t.Errorf("the file holds %q, want %q", onDisk, "BBBAAAAAAA")
	}
}

// A MISMATCH IS NOT A 200, and it says what differs. The write DID happen, so the refusal
// must not read as "nothing was written": the caller has to treat the contents as unknown.
func TestAVerificationMismatchIsReportedAsAFailure(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	target := filepath.Join(t.TempDir(), "tampered.txt")
	if err := os.WriteFile(target, []byte("what-is-on-the-host"), 0o644); err != nil {
		t.Fatal(err)
	}
	cli, release, bad := s.sftpClientContext(t.Context(), sid)
	if bad != nil {
		t.Fatalf("sftp client: %v", bad)
	}
	defer release()
	defer cli.Close()

	// A report that claims bytes this file does not hold is exactly the case a truncated
	// or redirected transfer produces, and the verifier is the thing that must catch it.
	bogus := filesftp.WriteReport{BytesWritten: 5, SHA256: strings.Repeat("ab", 32)}
	ok, detail := s.verifyRemoteWrite(cli, target, nil, bogus)
	if ok {
		t.Fatalf("a digest that does not match the host was reported as verified: %v", detail)
	}
	if detail["_mismatch"] == nil {
		t.Errorf("the mismatch does not say what differed: %v", detail)
	}
	if got, _ := detail["sha256_on_host"].(string); got == bogus.SHA256 {
		t.Errorf("the host's digest was replaced by the expected one: %v", detail)
	}
}

// `--op sha256` REVEALS CONTENT, so `file_fs` ALONE MUST NOT REACH IT.
//
// A session granted the filesystem group (truncate, realpath, statvfs) could otherwise hash
// any file it can see and compare candidates against the digest — a content oracle wearing
// a stat's clothes. The pair is asked for where the action is chosen.
func TestSha256NeedsFileReadNotJustFileFs(t *testing.T) {
	s := newExtensionTestServer(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(target, []byte("SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callExtension(t, s, map[string]any{
		"action": "create", "name": "fs357", "target": "internal",
		"allow": []any{"file.stat", "file.fs"}, "paths": []any{dir}, "enabled": true,
	})
	got := useExtension(t, s, "fs357")
	sid, _ := got["session_id"].(string)
	if sid == "" {
		t.Fatalf("extension has no session: %v", got)
	}

	out := termText(t, s, map[string]any{"cmd": "get " + sid + " file " + target + " --op sha256", "timeout": 15})
	if !strings.Contains(out, "TERM/1 403") {
		t.Fatalf("a session with file.fs but not file.read could hash a file:\n%s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("the refusal leaked content:\n%s", out)
	}
}

// THE RANGE FLAGS ON `get` ARE NOT FREE-FLOATING: they mean something for `--op sha256`
// and nothing anywhere else on this verb, so an unused one is refused rather than dropped.
func TestGetRefusesRangeFlagsOutsideSha256(t *testing.T) {
	s := newTestServer(t)
	sid, _ := startTestSession(t, s)
	target := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{
		"get " + sid + " file " + target + " --op stat --offset 3",
		"get " + sid + " file " + target + " --length 3",
		"get " + sid + " --offset 1",
	} {
		got := termText(t, s, map[string]any{"cmd": cmd, "timeout": 10})
		if !strings.Contains(got, "TERM/1 400") {
			t.Errorf("`%s` was accepted instead of refused:\n%s", cmd, got)
		}
	}
}
