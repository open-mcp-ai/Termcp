package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

const maxBatchManifest = 256 << 10

// Array pairs preserve order and rule out duplicate object keys. Validate
// the whole manifest before allowing the first side effect.
func parseCreateManifest(raw string) ([][]*string, error) {
	if len(raw) > maxBatchManifest {
		return nil, fmt.Errorf("batch-create manifest exceeds 256 KiB")
	}
	var entries [][]*string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || len(entries) == 0 || len(entries) > 32 {
		return nil, fmt.Errorf("batch-create needs a JSON array of 1–32 [name,text] pairs")
	}
	seen := map[string]bool{}
	for i, pair := range entries {
		if len(pair) != 2 || pair[0] == nil || pair[1] == nil {
			return nil, fmt.Errorf("entry %d needs two strings: [name,text]", i+1)
		}
		name := *pair[0]
		key := strings.ToUpper(name)
		stem, _, _ := strings.Cut(key, ".")
		stem = strings.TrimRight(stem, " ")
		device := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" ||
			((strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && len([]rune(stem)) == 4 && strings.ContainsRune("0123456789¹²³", []rune(stem)[3]))
		if name == "" || len(name) > 255 || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") ||
			strings.ContainsAny(name, `\/:*?"<>|`) || strings.IndexFunc(name, unicode.IsControl) >= 0 || device || seen[key] {
			return nil, fmt.Errorf("entry %d needs a unique, portable direct-child filename", i+1)
		}
		seen[key] = true
	}
	return entries, nil
}

func (s *Server) batchCreate(ctx context.Context, stmt directive.Statement) []envelope.Slot {
	for k := range stmt.Flags {
		if k != "op" && k != "data" && k != "dry-run" {
			return []envelope.Slot{badRequest(stmt, "batch-create does not take --"+k)}
		}
	}
	dir := strings.ReplaceAll(stmt.Key, `\`, "/")
	isDrive := len(dir) >= 3 && ((dir[0] >= 'A' && dir[0] <= 'Z') || (dir[0] >= 'a' && dir[0] <= 'z')) && dir[1:3] == ":/"
	if (!path.IsAbs(dir) && !isDrive) || strings.HasPrefix(dir, "//") || strings.Contains("/"+dir+"/", "/../") || strings.IndexFunc(dir, unicode.IsControl) >= 0 {
		return []envelope.Slot{badRequest(stmt, "batch-create needs an absolute local-drive or POSIX directory without '..'")}
	}
	raw, _ := stmt.Flag("data")
	entries, err := parseCreateManifest(raw)
	if err != nil {
		return []envelope.Slot{badRequest(stmt, err.Error())}
	}
	target := stmt.Target
	if _, use := s.parseUseHost(target); !use {
		sess, bad := s.requireRunningSession(target)
		if bad != nil {
			return []envelope.Slot{s.slotFromFailure(ctx, stmt, failureFromResult(bad))}
		}
		target = sess.ID
	}
	slots := make([]envelope.Slot, 0, len(entries))
	for _, pair := range entries {
		name, data := *pair[0], *pair[1]
		child := path.Join(dir, name)
		entry := directive.Statement{Op: "write", Path: "batch-create " + child}
		args := argsOf("session_id", target, "remote_path", child, "data", data, "create_only", true)
		if boolFlag(stmt, "dry-run") {
			req := mcp.CallToolRequest{}
			req.Params.Name, req.Params.Arguments = "file_write", args
			if bad := s.admission(ctx, "file_write", lookupGate("file_write"), req); bad != nil {
				slots = append(slots, s.slotFromFailure(ctx, entry, failureFromResult(bad)))
			} else {
				slots = append(slots, envelope.Slot{Code: 200, Headers: map[string]string{"Path": entry.Path, "Dry-Run": "true"}, Body: fmt.Sprintf("planned: %d UTF-8 bytes; existence and writability not checked", len(data))})
			}
			continue
		}
		res, fail := s.callTool(ctx, "file_write", args)
		if fail != nil {
			slots = append(slots, s.slotFromFailure(ctx, entry, fail))
		} else if held := reviewPendingFrom(res); held != nil {
			slots = append(slots, held.slot(entry.Path))
		} else {
			slots = append(slots, envelope.Slot{Code: 200, Headers: map[string]string{"Path": entry.Path}, Body: toolResultText(res)})
		}
	}
	return slots
}
