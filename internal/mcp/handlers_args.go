package mcp

import (
	"encoding/json"

	"github.com/open-mcp-ai/termcp/mcp"
)

func getString(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func getBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func getFloat64(args map[string]any, key string, def float64) float64 {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		}
	}
	return def
}

func jsonResult(data map[string]any) *mcp.CallToolResult {
	b, _ := json.Marshal(data)
	return mcp.NewToolResultText(string(b))
}

func successResult() *mcp.CallToolResult {
	return mcp.NewToolResultText(`{"success":true}`)
}

func getStringSlice(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	switch arr := v.(type) {
	case []any:
		// The MCP wire shape: JSON decodes every array to []any.
		var result []string
		for _, item := range arr {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		// Go-side callers (tests, handlers handed a ready-made argv). A silent
		// nil here would look exactly like "the argument was dropped".
		return arr
	}
	return nil
}

// toMap converts a struct to map[string]any via JSON round-trip.
func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}
