package extensions

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// AN EXTENSION IS A TEMPLATE PLUS THIS INSTALLATION'S VALUES.
//
// config.toml is the template: what the extension IS, with holes where only the
// machine it gets installed on can answer.
//
//	mode = "pipe"
//	command = "{{command}}"
//	targets = "{{targets}}"
//
//	[params.command]
//	type = "string"
//	help = "The program that serves MCP over the pipe"
//
//	[params.targets]
//	type = "array"
//	help = "The hosts this runs on, each with its own tunnels"
//
// config.local.toml holds those values and nothing else:
//
//	command = "kubectl-mcp-server"
//	targets = [{host = "pi", forward = [{kind = "local", local_port = 8443, remote_port = 443}]}]
//
// WHY A HOLE IS A WHOLE STRING VALUE. A bare `command = {{command}}` is not TOML,
// so a package's template would stop being a readable document before the
// installation fills it — and a package arrives with holes and NO values, so it has
// to parse at the moment it is stored. Written as `"{{command}}"` the hole is one
// complete string, and filling it replaces the value rather than patching text, so
// `local_port` stays a number and a command containing a quote, comma or newline
// cannot corrupt the document around it.
//
// TWO FILES, TWO OWNERS. The template belongs to whoever wrote the extension — an
// author, an agent, a package being upgraded — and is replaced wholesale when that
// changes. The values belong to THIS installation and survive the replacement. That
// split is what makes an extension shippable: the package carries the recipe, the
// installation carries the hosts, and upgrading one leaves the other alone.
//
// NEITHER FILE IS AN EDITOR TARGET. A caller edits a template or sets a named value.
// Where those are stored is this package's business.

// LocalFileName is the values file beside config.toml. This package writes it and
// nothing else does: setting a value is the interface, the file is the storage.
const LocalFileName = "config.local.toml"

var (
	// A hole is an identifier, and it is the WHOLE string value. Both halves matter:
	// the identifier keeps a note that happens to contain `{{` from becoming a hole,
	// and "the whole value" is what lets the replacement keep the value's own type
	// instead of being escaped into somebody else's string.
	exactHole = regexp.MustCompile(`^\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}$`)
	holeAt    = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
)

// Param is what a template says about one hole: enough that a caller seeing
// `targets` knows what to put there, without reading the template.
type Param struct {
	Type string `toml:"type,omitempty"`
	Help string `toml:"help,omitempty"`
}

// Hole is one template request as a caller sees it: what to fill, what the author
// said it is for, and whether this installation has filled it.
type Hole struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Help  string `json:"help,omitempty"`
	Set   bool   `json:"set"`
	Value any    `json:"value,omitempty"`
}

// TemplateOf reads a template's declarations and the holes it asks for.
//
// A hole written inside a longer string is refused, and so is a hole with no
// declaration. Both are template errors rather than value errors: they make the
// template unable to say what it wants, which is the one thing a package has to be
// able to do before anyone installs it.
func TemplateOf(data []byte) (map[string]Param, []string, error) {
	if len(data) > MaxConfigSize {
		return nil, nil, fmt.Errorf("extension template is too large (max %d bytes)", MaxConfigSize)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	params := map[string]Param{}
	if raw, ok := doc["params"]; ok {
		blocks, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("params must be a table of hole declarations")
		}
		for name, rawParam := range blocks {
			fields, ok := rawParam.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("[params.%s] must be a table with a help line", name)
			}
			for key := range fields {
				if key != "help" && key != "type" {
					return nil, nil, fmt.Errorf("[params.%s] has an unknown field %q: use type and help", name, key)
				}
			}
			kind, _ := fields["type"].(string)
			if kind == "" {
				kind = "string"
			}
			switch kind {
			case "string", "integer", "number", "boolean", "array", "table":
			default:
				return nil, nil, fmt.Errorf("[params.%s] type must be string, integer, number, boolean, array or table; got %q", name, kind)
			}
			help, _ := fields["help"].(string)
			params[name] = Param{Type: kind, Help: help}
		}
		// The declarations are authoring metadata, not configuration: leaving them in
		// would send `params` to the extension parser, which has no field for it.
		delete(doc, "params")
	}
	written, err := holesIn(doc)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range written {
		if _, ok := params[name]; !ok {
			return nil, nil, fmt.Errorf("this template asks for %s but does not say what it is for: add a [params.%s] block with a help line", brace(name), name)
		}
	}
	return params, written, nil
}

// holesIn walks PARSED TOML, so what counts as a value is decided by the TOML
// parser rather than by a second grammar over the text.
func holesIn(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		if m := exactHole.FindStringSubmatch(x); m != nil {
			return []string{m[1]}, nil
		}
		if m := holeAt.FindStringSubmatch(x); m != nil {
			return nil, fmt.Errorf("%s sits inside a longer string: a hole must be the whole value, so what fills it keeps its own type", brace(m[1]))
		}
		return nil, nil
	case map[string]any:
		var out []string
		for _, value := range x {
			names, err := holesIn(value)
			if err != nil {
				return nil, err
			}
			out = append(out, names...)
		}
		return unique(out), nil
	default:
		rv := reflect.ValueOf(v)
		if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
			return nil, nil
		}
		var out []string
		for i := 0; i < rv.Len(); i++ {
			names, err := holesIn(rv.Index(i).Interface())
			if err != nil {
				return nil, err
			}
			out = append(out, names...)
		}
		return unique(out), nil
	}
}

// Render fills a template and returns the document an Extension is built from.
//
// A MISSING VALUE IS AN ERROR, never a blank. A template whose `targets` hole was
// quietly left empty loads as an extension with no hosts, reports that it works,
// and does nothing — and the one caller who could fix it is never told.
func Render(data []byte, values map[string]any) ([]byte, error) {
	params, written, err := TemplateOf(data)
	if err != nil {
		return nil, err
	}
	// VALUES CAN OUTLIVE THE HOLE THAT USED THEM. An update replaces the package's
	// template wholesale and keeps this installation's values wholesale too; a new
	// version may rename or remove a hole. Ignoring an old entry here preserves it
	// for a later version without letting it affect this one. SetValues refuses
	// unknown names when they are written, which is where a typo belongs.
	var missing []string
	for _, name := range written {
		value, ok := values[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if err := checkParamValue(name, params[name].Type, value); err != nil {
			return nil, err
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no value for %s yet — set it and this extension works", strings.Join(braceAll(missing), " and "))
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	delete(doc, "params")
	filled, err := fill(doc, values)
	if err != nil {
		return nil, err
	}
	out, err := toml.Marshal(filled)
	if err != nil {
		return nil, fmt.Errorf("rendering extension values: %w", err)
	}
	return out, nil
}

func fill(v any, values map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		if m := exactHole.FindStringSubmatch(x); m != nil {
			return values[m[1]], nil
		}
		return x, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			filled, err := fill(value, values)
			if err != nil {
				return nil, err
			}
			out[k] = filled
		}
		return out, nil
	default:
		rv := reflect.ValueOf(v)
		if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
			return v, nil
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			filled, err := fill(rv.Index(i).Interface(), values)
			if err != nil {
				return nil, err
			}
			out[i] = filled
		}
		return out, nil
	}
}

// HolesFor lists a template's holes against the values this installation stored,
// which is the whole of what a panel or an agent needs to finish setting it up.
func HolesFor(data []byte, values map[string]any) ([]Hole, error) {
	params, written, err := TemplateOf(data)
	if err != nil {
		return nil, err
	}
	out := make([]Hole, 0, len(written))
	for _, name := range written {
		h := Hole{Name: name, Type: params[name].Type, Help: params[name].Help}
		if value, ok := values[name]; ok {
			h.Set, h.Value = true, value
		}
		out = append(out, h)
	}
	return out, nil
}

// MissingOf reports what this template still needs from its installation.
func MissingOf(data []byte, values map[string]any) ([]string, error) {
	_, written, err := TemplateOf(data)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range written {
		if _, ok := values[name]; !ok {
			out = append(out, name)
		}
	}
	return out, nil
}

func checkParamValue(name, kind string, value any) error {
	valid := false
	switch kind {
	case "string":
		_, valid = value.(string)
	case "integer":
		switch value.(type) {
		case int, int64, int32:
			valid = true
		}
	case "number":
		switch value.(type) {
		case int, int64, int32, float64, float32:
			valid = true
		}
	case "boolean":
		_, valid = value.(bool)
	case "array":
		rv := reflect.ValueOf(value)
		valid = rv.IsValid() && (rv.Kind() == reflect.Array || rv.Kind() == reflect.Slice)
	case "table":
		_, valid = value.(map[string]any)
	}
	if !valid {
		return fmt.Errorf("value for %s must be a TOML %s, got %T", brace(name), kind, value)
	}
	return nil
}

func brace(name string) string { return "{{" + name + "}}" }

func braceAll(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = brace(name)
	}
	return out
}

func unique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// wholeNumbers rewrites a JSON float that carries an integer as an integer.
//
// THIS IS WHY IT EXISTS AND NOT A CLEANUP. A value that arrives over JSON has no
// integers: a decoder hands `8443` back as float64, and writing that into the
// values file stores `8443.0`. Reading the rendered template then fails on
// `local_port`, which is an int — so a host added through the MCP tool would break
// an extension that the same host added through the panel configured fine. The two
// entrances must not disagree about what a port is.
//
// Only an integral float is rewritten. A float that is genuinely fractional (a
// duration, a ratio) keeps its type rather than being silently truncated.
func wholeNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = wholeNumbers(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = wholeNumbers(item)
		}
		return out
	default:
		return v
	}
}
