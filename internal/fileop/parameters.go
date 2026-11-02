// Package fileop interprets the effective parameters shared by file execution
// and authorization. It does not resolve paths, open files or start transfers.
package fileop

import (
	"fmt"
	"strings"

	textencoding "github.com/open-mcp-ai/termcp/internal/encoding"
)

// InlineReadLimit is the existing text/hex reply cap, in source-file bytes.
// File transfers are bounded only by a positive requested length.
const InlineReadLimit int64 = 8 << 20

type Read struct {
	Offset, Length int64
	Mode, Charset  string
	LocalPath      string
}

func ReadFromArgs(args map[string]any) Read {
	p := Read{Offset: integer(args, "offset"), Length: integer(args, "length"),
		Mode: text(args, "mode", "text"), Charset: text(args, "charset", "")}
	// text/hex replies never use local_path, even if it was supplied.
	if p.Mode == "file" {
		p.LocalPath = text(args, "local_path", "")
	}
	return p
}

func (p Read) LocalTransfer() bool { return p.Mode == "file" && p.LocalPath != "" }

func (p Read) ByteLimit() (int64, bool) {
	if p.Mode == "file" {
		return p.Length, p.Length > 0
	}
	if p.Length <= 0 || p.Length > InlineReadLimit {
		return InlineReadLimit, true
	}
	return p.Length, true
}

type Write struct {
	Offset              *int64 // absence truncates; explicit zero overwrites
	Data, Mode          string
	LocalPath           string
	LocalOffset, Length int64
}

func WriteFromArgs(args map[string]any) Write {
	p := Write{Data: text(args, "data", ""), Mode: text(args, "mode", ""),
		LocalPath: text(args, "local_path", ""), LocalOffset: integer(args, "local_offset"), Length: integer(args, "length")}
	if _, present := args["offset"]; present {
		n := integer(args, "offset")
		p.Offset = &n
	}
	return p
}

func (p Write) ByteLimit() (int64, bool, error) {
	if p.LocalPath != "" {
		return p.Length, p.Length > 0, nil
	}
	raw, err := DecodeWriteData(p.Data, p.Mode)
	return int64(len(raw)), err == nil, err
}

// DecodeWriteData is also used by the SFTP writer, so authorization counts the
// bytes that will be written, including hex: and escaped text.
func DecodeWriteData(data, mode string) ([]byte, error) {
	switch mode {
	case "hex":
		raw, err := textencoding.HexDecode(data)
		if err != nil {
			return nil, fmt.Errorf("hex decode: %w", err)
		}
		return raw, nil
	case "text", "":
		if hex, ok := strings.CutPrefix(data, "hex:"); ok {
			raw, err := textencoding.HexDecode(hex)
			if err != nil {
				return nil, fmt.Errorf("hex decode after the `hex:` prefix: %w", err)
			}
			return raw, nil
		}
		return textencoding.DecodeText(data), nil
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
}

func text(args map[string]any, key, fallback string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return fallback
}

// Preserve the tool handlers' numeric defaults and conversions. A zero or
// negative transfer length means "remaining bytes", never an empty transfer.
func integer(args map[string]any, key string) int64 {
	switch n := args[key].(type) {
	case float64:
		return int64(n)
	case int:
		return int64(float64(n))
	case int64:
		return int64(float64(n))
	}
	return 0
}
