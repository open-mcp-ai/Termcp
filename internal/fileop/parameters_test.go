package fileop

import (
	"bytes"
	"testing"
)

func TestReadEvidenceMatchesTheExecutedForm(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		local    bool
		limit    int64
		hasLimit bool
	}{
		{"default inline cap", nil, false, InlineReadLimit, true},
		{"inline ignores local path", map[string]any{"local_path": "unused", "length": 4}, false, 4, true},
		{"hex is inline", map[string]any{"mode": "hex", "local_path": "unused", "length": 4}, false, 4, true},
		{"inline over cap", map[string]any{"length": InlineReadLimit + 1}, false, InlineReadLimit, true},
		{"negative means remainder", map[string]any{"length": -1}, false, InlineReadLimit, true},
		{"bounded download", map[string]any{"mode": "file", "local_path": "used", "length": 4}, true, 4, true},
		{"unbounded download", map[string]any{"mode": "file", "local_path": "used"}, true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ReadFromArgs(tc.args)
			n, known := p.ByteLimit()
			if p.LocalTransfer() != tc.local || n != tc.limit || known != tc.hasLimit {
				t.Fatalf("effective read: local=%v bound=%d/%v", p.LocalTransfer(), n, known)
			}
			if !tc.local && p.LocalPath != "" {
				t.Fatal("inline read retained an unused local path")
			}
		})
	}
}

func TestWriteEvidenceUsesActualDecodedBytes(t *testing.T) {
	for _, tc := range []struct {
		data, mode string
		want       []byte
	}{
		{`one\ntwo`, "", []byte("one\ntwo")},
		{"你好", "text", []byte("你好")},
		{"hex:00ff", "", []byte{0, 255}},
		{"00ff", "hex", []byte{0, 255}},
	} {
		p := WriteFromArgs(map[string]any{"data": tc.data, "mode": tc.mode, "length": 1})
		raw, err := DecodeWriteData(p.Data, p.Mode)
		if err != nil || !bytes.Equal(raw, tc.want) {
			t.Fatalf("decoded bytes: %q %v", raw, err)
		}
		n, known, err := p.ByteLimit()
		if err != nil || !known || n != int64(len(tc.want)) {
			t.Fatalf("inline length was allowed to hide the payload size: %d/%v %v", n, known, err)
		}
	}
	for _, args := range []map[string]any{{"data": "hex:xyz"}, {"data": "xyz", "mode": "hex"}, {"data": "ok", "mode": "file"}} {
		if _, known, err := WriteFromArgs(args).ByteLimit(); known || err == nil {
			t.Fatal("invalid inline input acquired byte evidence", args)
		}
	}
}

func TestLocalWriteBoundAndOffsetPresence(t *testing.T) {
	for _, n := range []int{0, -1, 4} {
		p := WriteFromArgs(map[string]any{"local_path": "source", "length": n})
		bound, known, err := p.ByteLimit()
		if bound != int64(n) || known != (n > 0) || err != nil || p.Offset != nil {
			t.Fatalf("local write: %d/%v offset=%v err=%v", bound, known, p.Offset, err)
		}
	}
	p := WriteFromArgs(map[string]any{"offset": 0})
	if p.Offset == nil || *p.Offset != 0 {
		t.Fatal("explicit zero was collapsed into absent offset")
	}
}
