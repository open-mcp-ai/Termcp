package configstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The publishing write is the only way a profile body reaches disk, so the two
// properties a crash can violate are checked here: a reader never sees a
// half-written file, and nothing is left behind to be mistaken for one.

func TestPublishingWriteLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "object.enc")

	if err := writeFileAtomic(path, []byte("first")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if err := writeFileAtomic(path, []byte("second")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := string(body); got != "second" {
		t.Fatalf("published body = %q, want the whole replacement", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "object.enc" {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Fatalf("directory holds %v, want only the published file", names)
	}
}

func TestPublishingWriteIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not report POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "body.enc")
	if err := writeFileAtomic(path, []byte("secret")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("published file mode = %04o, want 0600", perm)
	}
}

// A symlinked directory would let a write land somewhere the store never
// authenticated, so it is refused rather than followed.
func TestPublishingWriteRefusesASymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, privateDirMode); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, link); err != nil {
		if errors.Is(err, os.ErrPermission) || runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable here: %v", err)
		}
		t.Fatalf("Symlink: %v", err)
	}

	err := writeFileAtomic(filepath.Join(link, "body.enc"), []byte("secret"))
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("write through a symlinked directory = %v, want ErrFormat", err)
	}
	if _, statErr := os.Stat(filepath.Join(realDir, "body.enc")); statErr == nil {
		t.Fatal("the refused write still landed in the target directory")
	}
}

func TestRemoveIfExistsToleratesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.enc")
	if err := removeIfExists(path); err != nil {
		t.Fatalf("removeIfExists on a missing file = %v, want nil", err)
	}
	if err := os.WriteFile(path, []byte("x"), privateFileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := removeIfExists(path); err != nil {
		t.Fatalf("removeIfExists = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still present after removeIfExists: %v", err)
	}
}

func TestWriteErrorsNameTheDestination(t *testing.T) {
	// A failure has to be reportable without leaking the plaintext it was
	// carrying, so the message names paths, never content.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), privateFileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := writeFileAtomic(filepath.Join(blocker, "body.enc"), []byte("secret"))
	if err == nil {
		t.Fatal("writing under a regular file succeeded")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the error leaked the body it was carrying: %v", err)
	}
}
