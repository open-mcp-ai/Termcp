package mcp

import (
	"github.com/open-mcp-ai/termcp/mcp"
)

// registerTermTools declares the single terminal entry, which is the whole
// default surface. Keep it that way: the tool listing is context the caller pays
// for on every turn, and the capability count must not move it (issue #117 33b).
//
// The argument list is not written here — it is rendered from termArgDecls
// (term_args.go), the same table docs/api.md's parameter table and the tests read.
// Adding an argument is one entry in that table, and the schema, the doc and the
// parser's default all move together.
func registerTermTools(mcpServer *mcp.Server, s *Server) {
	opts := []mcp.ToolOption{
		// Four default booleans on every tool carry no termcp information and cost
		// ~80 bytes; the annotation block is empty on purpose.
		mcp.WithToolAnnotation(mcp.ToolAnnotation{}),
		mcp.WithDescription(termDescription),
	}
	opts = append(opts, termArgToolOptions()...)
	mcpServer.AddTool(mcp.NewTool(termToolName, opts...), withLogging(termToolName, s.handleTerm, s))
}

// termArgToolOptions renders every argument declaration as its schema option.
//
// The default is repeated in the prose as well as in the schema's `default`
// field: clients differ on whether they render that field, and a hidden default is
// a guess the caller should not have to make. Writing it twice is free here
// because both copies come from the same value.
func termArgToolOptions() []mcp.ToolOption {
	out := make([]mcp.ToolOption, 0, len(termArgDecls))
	for _, d := range termArgDecls {
		switch d.Kind {
		case argString:
			// No DefaultString for an empty default: a client rendering
			// `"default": ""` beside a field whose description already says
			// "Default: empty" is repeating it, and the field is optional either
			// way. A non-empty string default (an enum below) is published.
			opts := []mcp.PropertyOption{mcp.Description(d.Desc)}
			if d.DefText != "" {
				opts = append(opts, mcp.DefaultString(d.DefText))
			}
			out = append(out, mcp.WithString(d.Name, opts...))
		case argNumber:
			// The declared bounds ride on the schema so a client can refuse a bad value
			// BEFORE the call. parseTermArgs enforces the same numbers, because the REST
			// entrance never reaches the validator — one declaration, two
			// enforcement points, no drift.
			opts := []mcp.PropertyOption{
				mcp.Description(d.Desc),
				mcp.DefaultNumber(numDefault(d.Name)),
			}
			if v, ok := boundValue(d.Min); ok {
				opts = append(opts, mcp.Min(v))
			}
			out = append(out, mcp.WithNumber(d.Name, opts...))
		case argBool:
			out = append(out, mcp.WithBoolean(d.Name,
				mcp.Description(d.Desc),
				mcp.DefaultBool(d.Def == true),
			))
		case argEnum:
			// An enum is a string with a closed value set: the schema carries both
			// the default and the allowed values, which is what lets a client
			// reject a typo before the call.
			opts := []mcp.PropertyOption{mcp.Description(d.Desc)}
			if d.DefText != "" {
				opts = append(opts, mcp.DefaultString(d.DefText))
			}
			if len(d.Enum) > 0 {
				opts = append(opts, mcp.Enum(d.Enum...))
			}
			out = append(out, mcp.WithString(d.Name, opts...))
		case argMap:
			// A dictionary of string→string. The schema says object with string
			// values so a client can refuse `{"N": 3}` before the call; readEnvArg
			// enforces the same rule for the REST entrance, which never reaches
			// the validator.
			out = append(out, mcp.WithObject(d.Name,
				mcp.Description(d.Desc),
				mcp.AdditionalProperties(map[string]any{"type": "string"}),
			))
		case argStringArray:
			// A list of strings. `items` is what lets a client refuse `[1, 2]` before
			// the call; readArgsArg enforces the same rule for the REST entrance.
			// minItems is NOT pinned to 1: an empty list is the same request as an
			// omitted one, and refusing it would make two spellings of one call differ.
			out = append(out, mcp.WithArray(d.Name,
				mcp.Description(d.Desc),
				mcp.Items(map[string]any{"type": "string"}),
			))
		}
	}
	return out
}
