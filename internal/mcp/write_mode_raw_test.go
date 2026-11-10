package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteModeRawWritesVerbatimCharacters pins issue #302:
//
// `--data` in text mode decodes `\n` to a real LF and folds `\\` to `\`. When writing
// code, Dockerfiles, or scripts, this silently corrupts strings like `printf("%s\n")`
// or line-continuation backslashes.
//
// `--mode raw` guarantees bytes are written byte-for-byte as UTF-8 without decoding
// escapes, and every write reply reports `sha256` so the caller can verify the file.
func TestWriteModeRawWritesVerbatimCharacters(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	path := filepath.Join(t.TempDir(), "Dockerfile")

	payload := `printf '%s\n' \\`
	wrote := termText(t, s, map[string]any{
		"cmd":  "write " + h + " file " + path + " --mode raw --data $1",
		"args": []any{payload},
	})
	if code := statusCode(t, wrote); code >= 400 {
		t.Fatalf("write raw failed (%d):\n%s", code, wrote)
	}
	expectedHash := sha256.Sum256([]byte(payload))
	hashHex := hex.EncodeToString(expectedHash[:])
	if !strings.Contains(wrote, hashHex) {
		t.Errorf("write reply must include sha256 of written bytes (%s):\n%s", hashHex, wrote)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != payload {
		t.Errorf("content was rewritten:\n got: %q\nwant: %q", string(content), payload)
	}
}

// TestWriteTextModeReportsDecodedSummary pins the decode receipt for #302:
// When text mode rewrites `\n` or `\\`, the reply must explicitly state what was
// decoded so the caller is never trapped into believing the echo matches the disk bytes.
func TestWriteTextModeReportsDecodedSummary(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	path := filepath.Join(t.TempDir(), "probe.c")

	payload := `line1\nline2\\line3`
	wrote := termText(t, s, map[string]any{
		"cmd":  "write " + h + " file " + path + " --data $1",
		"args": []any{payload},
	})
	if code := statusCode(t, wrote); code >= 400 {
		t.Fatalf("write failed (%d):\n%s", code, wrote)
	}
	if !strings.Contains(wrote, "decoded: \\n -> LF x1, \\\\ -> \\ x1") {
		t.Errorf("write text mode must report decoded summary:\n%s", wrote)
	}
	if !strings.Contains(wrote, "sha256:") {
		t.Errorf("write text mode must report sha256:\n%s", wrote)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "line1\nline2\\line3"
	if string(content) != want {
		t.Errorf("content:\n got: %q\nwant: %q", string(content), want)
	}
}
