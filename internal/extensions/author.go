package extensions

import (
	"fmt"
	"strings"
)

// This file is the single authoring layer. Both callers that write an
// extension template — the MCP `extension` tool and the WebUI's own create/
// update routes — apply an author's arguments through ApplyArgs and save
// through the same Store, so a rule added here reaches both surfaces at once
// and neither can drift into accepting a template the other rejects.

// ApplyArgs writes the fields an author supplied onto a template and reports
// whether the arguments were usable. A key that is absent is left alone, so an
// update changes only what was sent; an empty array is meaningful and clears the
// field, because "no whitelisted tools" and "no restriction" are different
// templates.
//
// update distinguishes the two call shapes: a create starts from a preset and
// takes its conventional values for anything left out, while an update already
// has the stored template behind it and must not gain preset values the author
// did not ask for.
//
// A malformed value is refused rather than silently dropped. It used to fall back
// to "no change", which for an array field meant CLEARING it: `allow: [1,2]`
// parsed to an empty slice, and an empty whitelist is not "fewer tools" — it is
// the unrestricted default, so a typo widened the session instead of narrowing
// it. Refusing is the only reading a client sending the wrong type cannot be
// surprised by.
func ApplyArgs(ext *Extension, args map[string]any, update bool) error {
	if hasOpt(args, "kind") {
		if kind := strings.TrimSpace(optString(args, "kind", "")); kind != "" {
			ext.Kind = kind
		}
	}
	if hasOpt(args, "target") {
		ext.Target = strings.TrimSpace(optString(args, "target", ""))
	}
	if hasOpt(args, "description") {
		ext.Description = strings.TrimSpace(optString(args, "description", ""))
	}
	if hasOpt(args, "command") {
		ext.Command = strings.TrimSpace(optString(args, "command", ""))
	}
	if hasOpt(args, "mode") {
		ext.Mode = strings.TrimSpace(optString(args, "mode", ""))
	}
	if hasOpt(args, "args") {
		v, err := optStringSlice(args, "args")
		if err != nil {
			return err
		}
		ext.Args = v
	}
	if hasOpt(args, "url") {
		ext.URL = strings.TrimSpace(optString(args, "url", ""))
		// A URL template runs termcp's in-process relay instead of a program, and
		// Validate refuses the pair — but the preset a create starts from is a
		// stdio server, so it already carries `npx -y example-mcp-server`. Left
		// in place that preset value collides with the URL the author just gave
		// and the create fails with "url and command are alternatives", which is
		// a rule about what the author sent, not about a default nobody asked
		// for. Clear only what the caller did not name, so an explicit
		// command/args still wins and still meets the same refusal.
		//
		// This also covers the other direction on update: a stored stdio template
		// edited into an endpoint drops the command it no longer runs.
		if ext.URL != "" {
			if !hasOpt(args, "command") {
				ext.Command = ""
			}
			if !hasOpt(args, "args") {
				ext.Args = nil
			}
		}
	}
	if hasOpt(args, "headers") {
		v, err := optStringMap(args, "headers")
		if err != nil {
			return err
		}
		ext.Headers = v
	}
	if hasOpt(args, "allow") {
		v, err := optStringSlice(args, "allow")
		if err != nil {
			return err
		}
		ext.Allow = v
	}
	if hasOpt(args, "paths") {
		v, err := optStringSlice(args, "paths")
		if err != nil {
			return err
		}
		ext.Paths = v
	}
	if hasOpt(args, "notes") {
		ext.Notes = optString(args, "notes", "")
	}
	if hasOpt(args, "enabled") {
		if _, ok := args["enabled"].(bool); !ok {
			return fmt.Errorf("enabled must be true or false, got %v", args["enabled"])
		}
		ext.SetEnabled(optBool(args, "enabled", true))
	}
	if hasOpt(args, "rows") {
		ext.Rows = optInt(args, "rows", 0)
	}
	if hasOpt(args, "cols") {
		ext.Cols = optInt(args, "cols", 0)
	}
	if hasOpt(args, "forwards") {
		decls, err := ParseForwardDecls(args["forwards"])
		if err != nil {
			return err
		}
		ext.Forwards = decls
	}
	if !update {
		ApplyDefaults(ext)
	}
	// A stored template that still carries its kind's preset recipe and has just
	// changed shape (stdio server → HTTP endpoint, or back) gets the recipe that
	// describes it now. An author's own notes are never touched — isPresetNotes is
	// what tells the two apart — and neither is a call that named `notes` at all,
	// including one that cleared it: "give me the default" and "leave it empty"
	// are different requests, and only the first is what an absent key means.
	if update && !hasOpt(args, "notes") && isPresetNotes(ext.Notes) {
		ext.Notes = PresetNotes(ext)
	}
	ext.Normalize()
	return nil
}

// SaveTemplate validates and persists one template. It is the only write path
// both callers use, so a template the WebUI accepts is a template the MCP tool
// accepts, and vice versa.
func SaveTemplate(store *Store, ext *Extension) error {
	if err := ext.Validate(); err != nil {
		return err
	}
	return store.Save(ext)
}

// ParseForwardDecls turns the JSON array an author sent into forward
// declarations. An item that is not an object is refused rather than dropped: a
// silently shortened list is a template the author did not write, and the next
// save would persist it as if they had.
func ParseForwardDecls(v any) ([]ForwardDecl, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("forwards must be an array of objects, got %T", v)
	}
	out := make([]ForwardDecl, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("forward #%d must be an object, got %T", i+1, item)
		}
		out = append(out, ForwardDecl{
			Kind:       optString(m, "kind", ""),
			BindHost:   optString(m, "bind_host", ""),
			LocalHost:  optString(m, "local_host", ""),
			LocalPort:  optInt(m, "local_port", 0),
			RemoteHost: optString(m, "remote_host", ""),
			RemotePort: optInt(m, "remote_port", 0),
		})
	}
	return out, nil
}

/* The option readers below are deliberately narrower than a general JSON getter:
   each one accepts only the type the field's author sends, and a value of the
   wrong type is refused by the field that reads it rather than coerced. A silent
   default on a capability field is a security decision made by a typo. */

func hasOpt(args map[string]any, key string) bool {
	_, ok := args[key]
	return ok
}

func optString(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func optBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func optInt(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
	}
	return def
}

func optStringSlice(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	switch arr := v.(type) {
	case []any:
		out := make([]string, 0, len(arr))
		for i, item := range arr {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d] must be a string, got %v", key, i, item)
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return append([]string(nil), arr...), nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings, got %T", key, v)
	}
}

// optStringMap reads a header table. A non-string value is refused rather than
// stringified: a header is a credential or an identifier, and `600` becoming
// "600" is the kind of help that hides a typo.
func optStringMap(args map[string]any, key string) (map[string]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object of name/value pairs, got %T", key, v)
	}
	if len(m) == 0 {
		return nil, nil // an empty table clears the field, like an empty array
	}
	out := make(map[string]string, len(m))
	for name, item := range m {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%s] must be a string, got %v", key, name, item)
		}
		out[name] = s
	}
	return out, nil
}
