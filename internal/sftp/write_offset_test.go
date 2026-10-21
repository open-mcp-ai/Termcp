package sftp

import (
	"os"
	"testing"
)

// TestWriteOpenFlagsDistinguishesZeroFromTruncate pins the fix for bug #1: writing at
// offset 0 must NOT truncate the file, while omitting offset (-1) must truncate.
func TestWriteOpenFlagsDistinguishesZeroFromTruncate(t *testing.T) {
	// Nil: truncate, offset normalized to 0.
	flag, off := writeOpenFlags(nil)
	if flag&os.O_TRUNC == 0 {
		t.Errorf("nil offset must carry O_TRUNC, got flag %x", flag)
	}
	if off != 0 {
		t.Errorf("normalized offset for nil = %d, want 0", off)
	}

	// Zero: must NOT truncate; keep offset at 0.
	zero := int64(0)
	flag, off = writeOpenFlags(&zero)
	if flag&os.O_TRUNC != 0 {
		t.Errorf("offset 0 must NOT carry O_TRUNC, got flag %x", flag)
	}
	if off != 0 {
		t.Errorf("offset 0 should stay 0, got %d", off)
	}

	// Positive: must NOT truncate; keep offset as given.
	pos := int64(42)
	flag, off = writeOpenFlags(&pos)
	if flag&os.O_TRUNC != 0 {
		t.Errorf("offset 42 must NOT carry O_TRUNC, got flag %x", flag)
	}
	if off != 42 {
		t.Errorf("offset 42 should stay 42, got %d", off)
	}
}
