package mcp

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// registerTermTools declares the single terminal entry, which is the whole
// default surface. Keep it that way: the tool listing is context the caller pays
// for on every turn, and the capability count must not move it (issue #117 33b).
//
// The argument list is not written here — it is rendered from termArgDecls
// (term_args.go), the same table docs/api.md's parameter table and the tests read.
// Adding an argument is one entry in that table, and the schema, the doc and the
// parser's default all move together.
func registerTermTools(mcpServer *mcpserver.MCPServer, s *Server) {
	opts := []mcpgo.ToolOption{
		// Four default booleans on every tool carry no termcp information and cost
		// ~80 bytes; the annotation block is empty on purpose.
		mcpgo.WithToolAnnotation(mcpgo.ToolAnnotation{}),
		mcpgo.WithDescription(termDescription),
	}
	opts = append(opts, termArgToolOptions()...)
	mcpServer.AddTool(mcpgo.NewTool(termToolName, opts...), withLogging(termToolName, s.handleTerm, s))
}

// termArgToolOptions renders every argument declaration as its schema option.
//
// The default is repeated in the prose as well as in the schema's `default`
// field: clients differ on whether they render that field, and a hidden default is
// a guess the caller should not have to make. Writing it twice is free here
// because both copies come from the same value.
func termArgToolOptions() []mcpgo.ToolOption {
	out := make([]mcpgo.ToolOption, 0, len(termArgDecls))
	for _, d := range termArgDecls {
		switch d.Kind {
		case argString:
			// No DefaultString for an empty default: a client rendering
			// `"default": ""` beside a field whose description already says
			// "Default: empty" is repeating it, and the field is optional either
			// way. A non-empty string default (an enum below) is published.
			opts := []mcpgo.PropertyOption{mcpgo.Description(d.Desc)}
			if d.DefText != "" {
				opts = append(opts, mcpgo.DefaultString(d.DefText))
			}
			out = append(out, mcpgo.WithString(d.Name, opts...))
		case argNumber:
			// The declared bounds ride on the schema so a client can refuse a bad value
			// BEFORE the call. parseTermArgs enforces the same numbers, because the REST
			// entrance never reaches mcp-go's validator — one declaration, two
			// enforcement points, no drift.
			opts := []mcpgo.PropertyOption{
				mcpgo.Description(d.Desc),
				mcpgo.DefaultNumber(numDefault(d.Name)),
			}
			if v, ok := boundValue(d.Min); ok {
				opts = append(opts, mcpgo.Min(v))
			}
			if v, ok := boundValue(d.Max); ok {
				opts = append(opts, mcpgo.Max(v))
			}
			out = append(out, mcpgo.WithNumber(d.Name, opts...))
		case argBool:
			out = append(out, mcpgo.WithBoolean(d.Name,
				mcpgo.Description(d.Desc),
				mcpgo.DefaultBool(d.Def == true),
			))
		case argEnum:
			// An enum is a string with a closed value set: the schema carries both
			// the default and the allowed values, which is what lets a client
			// reject a typo before the call.
			opts := []mcpgo.PropertyOption{mcpgo.Description(d.Desc)}
			if d.DefText != "" {
				opts = append(opts, mcpgo.DefaultString(d.DefText))
			}
			if len(d.Enum) > 0 {
				opts = append(opts, mcpgo.Enum(d.Enum...))
			}
			out = append(out, mcpgo.WithString(d.Name, opts...))
		case argMap:
			// A dictionary of string→string. The schema says object with string
			// values so a client can refuse `{"N": 3}` before the call; readEnvArg
			// enforces the same rule for the REST entrance, which never reaches
			// mcp-go's validator.
			out = append(out, mcpgo.WithObject(d.Name,
				mcpgo.Description(d.Desc),
				mcpgo.AdditionalProperties(map[string]any{"type": "string"}),
			))
		case argStringArray:
			// A list of strings. `items` is what lets a client refuse `[1, 2]` before
			// the call; readArgsArg enforces the same rule for the REST entrance.
			// minItems is NOT pinned to 1: an empty list is the same request as an
			// omitted one, and refusing it would make two spellings of one call differ.
			out = append(out, mcpgo.WithArray(d.Name,
				mcpgo.Description(d.Desc),
				mcpgo.Items(map[string]any{"type": "string"}),
			))
		}
	}
	return out
}
