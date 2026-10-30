// Package authorization describes the fine-grained business operations an Agent
// may run and decides whether a resolved plan satisfies a saved policy.
//
// It is deliberately free of HTTP, MCP, session and storage concepts. The MCP
// adapter resolves named resources into requirements; this package only names
// semantic operations and compares them with typed constraints.
package authorization

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Effect is the single per-operation verdict. Only allow and deny exist today:
// approval is delivered separately and cannot be encoded as a third effect.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// ConstraintKind names one typed condition a grant may attach to an operation.
// The catalog declares which kinds an operation can prove.
type ConstraintKind string

const (
	KindKeys      ConstraintKind = "keys"
	KindRepeatMax ConstraintKind = "repeat_max"
	KindToolNames ConstraintKind = "tool_names"
	KindMethods   ConstraintKind = "methods"
	KindForms     ConstraintKind = "forms"
	KindMaxBytes  ConstraintKind = "max_bytes"
)

// Constraints are applied after argument defaults, alias expansion and semantic
// resolution. A nil selection is unset; an explicit empty selection allows no
// value. JSON must preserve this distinction in responses and encrypted storage.
type Constraints struct {
	Keys              []string `json:"keys"`
	RepeatMax         *int     `json:"repeat_max,omitempty"`
	ToolNames         []string `json:"tool_names"`
	Methods           []string `json:"methods"`
	Forms             []string `json:"forms"`
	MaxBytes          *int64   `json:"max_bytes,omitempty"`
	missingSelections uint8
}

// UnmarshalJSON retains which selections old omitempty writers might have lost.
// New writers encode all four selections, using null for unrestricted values.
// Strict decoding is required here as well as at the enclosing HTTP boundary.
func (c *Constraints) UnmarshalJSON(raw []byte) error {
	type wire Constraints
	var value wire
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for i, key := range []string{"keys", "tool_names", "methods", "forms"} {
		if _, present := fields[key]; !present {
			value.missingSelections |= 1 << i
		}
	}
	*c = Constraints(value)
	return nil
}

// LegacyAmbiguous reports a condition that a previous lossy writer may have
// widened. A repeat/size condition does not prove a missing selection was unset.
// Restoration must quarantine these entries until an explicit operator save.
func (c Constraints) LegacyAmbiguous(id string) bool {
	if c.Empty() {
		return true
	}
	op, ok := Lookup(id)
	if !ok {
		return false
	}
	for _, kind := range op.ConstraintKinds {
		for i, selection := range []ConstraintKind{KindKeys, KindToolNames, KindMethods, KindForms} {
			if kind == selection && c.missingSelections&(1<<i) != 0 {
				return true
			}
		}
	}
	return false
}

// Empty reports whether no constraint is set.
func (c Constraints) Empty() bool {
	return c.Keys == nil && c.RepeatMax == nil && c.ToolNames == nil &&
		c.Methods == nil && c.Forms == nil && c.MaxBytes == nil
}

// Grant is one operation entry in a policy. deny takes no constraints, because
// ordering between exceptions is not defined.
type Grant struct {
	Effect      Effect       `json:"effect"`
	Constraints *Constraints `json:"constraints,omitempty"`
}

// Policy is the authorization half of a saved Agent policy. Host selection stays
// alongside it because every requirement inherits the Agent's total host scope.
type Policy struct {
	HostMode string           `json:"host_mode"`
	Hosts    []string         `json:"hosts"`
	Actions  map[string]Grant `json:"actions"`
}

// Requirement is one resolved semantic operation plus the evidence its
// constraints need. Host membership is checked by the caller against the policy.
type Requirement struct {
	Operation string
	Evidence  Evidence
}

// Plan is the complete set of operations one invocation must be allowed to run.
type Plan struct {
	Requirements []Requirement
}

// Evidence carries the resolved, effective values a constraint inspects.
type Evidence struct {
	Keys     []string
	Repeat   int
	ToolName string
	Method   string
	Forms    []string
	Bytes    int64
	HasBytes bool
}

// Decision is the outcome of checking a plan. Reason is empty on allow.
type Decision struct {
	Allowed bool
	Reason  string
}

// CheckPlan requires every operation in the plan to carry an explicit allow grant
// whose constraints all hold. Missing grants and deny grants both refuse.
func CheckPlan(p Policy, plan Plan) Decision {
	if len(plan.Requirements) == 0 {
		return Decision{false, "the plan has no operation"}
	}
	for _, req := range plan.Requirements {
		if _, known := Lookup(req.Operation); !known {
			return Decision{false, fmt.Sprintf("unknown operation %q", req.Operation)}
		}
		grant, ok := p.Actions[req.Operation]
		if !ok {
			return Decision{false, fmt.Sprintf("operation %q is not granted", req.Operation)}
		}
		if grant.Effect != EffectAllow {
			return Decision{false, fmt.Sprintf("operation %q is denied", req.Operation)}
		}
		if grant.Constraints != nil {
			if grant.Constraints.Empty() {
				return Decision{false, fmt.Sprintf("operation %q has ambiguous empty constraints", req.Operation)}
			}
			if ok, reason := grant.Constraints.Satisfied(req.Evidence); !ok {
				return Decision{false, fmt.Sprintf("operation %q violates constraints: %s", req.Operation, reason)}
			}
		}
	}
	return Decision{Allowed: true}
}

// Satisfied checks one set of constraints against resolved evidence. A set
// constraint can only narrow an invocation; missing evidence refuses.
func (c Constraints) Satisfied(e Evidence) (bool, string) {
	if c.Keys != nil {
		if len(e.Keys) == 0 {
			return false, "the invocation names no key"
		}
		for _, key := range e.Keys {
			if !contains(c.Keys, key) {
				return false, fmt.Sprintf("key %q is not allowed", key)
			}
		}
	}
	if c.RepeatMax != nil {
		if e.Repeat <= 0 || e.Repeat > *c.RepeatMax {
			return false, fmt.Sprintf("repeat %d exceeds the maximum %d", e.Repeat, *c.RepeatMax)
		}
	}
	if c.ToolNames != nil {
		if e.ToolName == "" || !contains(c.ToolNames, e.ToolName) {
			return false, fmt.Sprintf("tool %q is not allowed", e.ToolName)
		}
	}
	if c.Methods != nil {
		if e.Method == "" || !contains(c.Methods, e.Method) {
			return false, fmt.Sprintf("method %q is not allowed", e.Method)
		}
	}
	if c.Forms != nil {
		if len(e.Forms) == 0 {
			return false, "the invocation names no form"
		}
		for _, form := range e.Forms {
			if !contains(c.Forms, form) {
				return false, fmt.Sprintf("form %q is not allowed", form)
			}
		}
	}
	if c.MaxBytes != nil {
		if !e.HasBytes {
			return false, "the payload size cannot be proven before execution"
		}
		if e.Bytes < 0 || e.Bytes > *c.MaxBytes {
			return false, fmt.Sprintf("payload of %d bytes exceeds the maximum %d", e.Bytes, *c.MaxBytes)
		}
	}
	return true, ""
}

// Operation is one catalog row: the semantic name a policy stores, the family the
// UI groups it under, and the resource capability that still answers resource
// admission. Global marks configuration writes that cannot be scoped to one host.
type Operation struct {
	ID              string           `json:"id"`
	Family          string           `json:"family"`
	Name            string           `json:"name"`
	Description     string           `json:"description,omitempty"`
	Resource        string           `json:"resource,omitempty"`
	Global          bool             `json:"global"`
	ConstraintKinds []ConstraintKind `json:"constraint_kinds"`
}

var catalog = []Operation{
	// Sessions and terminal channels.
	{ID: "session.list", Family: "session", Name: "List sessions", Resource: "session.list"},
	{ID: "session.info", Family: "session", Name: "Inspect sessions", Resource: "session.info"},
	{ID: "session.start", Family: "session", Name: "Start sessions", Resource: "session.start"},
	{ID: "session.terminate", Family: "session", Name: "Stop sessions", Resource: "session.terminate"},
	{ID: "session.delete", Family: "session", Name: "Delete sessions", Resource: "session.delete"},
	{ID: "shell.list", Family: "shell", Name: "List channels", Resource: "shell.list"},
	{ID: "shell.open", Family: "shell", Name: "Open channels", Resource: "shell.open"},
	{ID: "shell.close", Family: "shell", Name: "Close channels", Resource: "shell.close"},
	{ID: "shell.exec", Family: "shell", Name: "Start programs", Resource: "shell.open"},
	{ID: "shell.resize", Family: "shell", Name: "Resize channels", Resource: "shell.resize"},
	{ID: "shell.output", Family: "shell", Name: "Read output", Resource: "shell.output"},
	{ID: "shell.input", Family: "shell", Name: "Type input", Resource: "shell.input"},
	{ID: "shell.key", Family: "shell", Name: "Press keys", Resource: "shell.key",
		ConstraintKinds: []ConstraintKind{KindKeys, KindRepeatMax}},
	{ID: "shell.marks", Family: "shell", Name: "Read spans", Resource: "session.message"},
	{ID: "shell.detect", Family: "shell", Name: "Detect environment", Resource: "shell.detect"},
	{ID: "shell.reader.register", Family: "shell", Name: "Register readers", Resource: "shell.reader.register"},
	{ID: "shell.reader.unregister", Family: "shell", Name: "Unregister readers", Resource: "shell.reader.unregister"},

	// Files. Read-only path queries stop borrowing file.write.
	{ID: "file.read", Family: "file", Name: "Read files", Resource: "file.read",
		ConstraintKinds: []ConstraintKind{KindForms, KindMaxBytes}},
	{ID: "file.stat", Family: "file", Name: "Stat files", Resource: "file.stat"},
	{ID: "file.realpath", Family: "file", Name: "Resolve real paths", Resource: "file.fs"},
	{ID: "file.readlink", Family: "file", Name: "Read links", Resource: "file.link"},
	{ID: "file.getwd", Family: "file", Name: "Read working directory", Resource: "file.getwd"},
	{ID: "file.statvfs", Family: "file", Name: "Read filesystem stats", Resource: "file.fs"},
	{ID: "file.urls", Family: "file", Name: "Get transfer URLs", Resource: "file.urls"},
	{ID: "file.write", Family: "file", Name: "Write files", Resource: "file.write",
		ConstraintKinds: []ConstraintKind{KindForms, KindMaxBytes}},
	{ID: "file.replace", Family: "file", Name: "Replace file text", Resource: "file.write",
		ConstraintKinds: []ConstraintKind{KindForms}},
	{ID: "file.delete", Family: "file", Name: "Delete files", Resource: "file.delete"},
	{ID: "file.mkdir", Family: "file", Name: "Create directories", Resource: "file.mkdir"},
	{ID: "file.truncate", Family: "file", Name: "Truncate files", Resource: "file.fs"},
	{ID: "file.chmod", Family: "file", Name: "Change file modes", Resource: "file.perm"},
	{ID: "file.chown", Family: "file", Name: "Change file owners", Resource: "file.perm"},
	{ID: "file.chtimes", Family: "file", Name: "Change file times", Resource: "file.perm"},
	{ID: "file.symlink", Family: "file", Name: "Create symbolic links", Resource: "file.link"},
	{ID: "file.link", Family: "file", Name: "Create hard links", Resource: "file.link"},
	{ID: "file.rename", Family: "file", Name: "Rename files", Resource: "file.rename"},

	// Port forwarding.
	{ID: "forward.list", Family: "forward", Name: "List forwards", Resource: "forward.list"},
	{ID: "forward.open.local", Family: "forward", Name: "Open local forwards", Resource: "forward.open"},
	{ID: "forward.open.remote", Family: "forward", Name: "Open remote forwards", Resource: "forward.open"},
	{ID: "forward.open.dynamic", Family: "forward", Name: "Open dynamic forwards", Resource: "forward.open"},
	{ID: "forward.close", Family: "forward", Name: "Close forwards", Resource: "forward.close"},

	// External protocol peers.
	{ID: "mcp.open", Family: "peer", Name: "Open MCP peers", Resource: "mcp.open"},
	{ID: "mcp.list", Family: "peer", Name: "List MCP peers", Resource: "mcp.open"},
	{ID: "mcp.info", Family: "peer", Name: "Inspect MCP peers", Resource: "mcp.open"},
	{ID: "mcp.close", Family: "peer", Name: "Close MCP peers", Resource: "mcp.open"},
	{ID: "mcp.read", Family: "peer", Name: "Read MCP frames", Resource: "mcp.open"},
	{ID: "mcp.tools.list", Family: "peer", Name: "List MCP tools", Resource: "mcp.open"},
	{ID: "mcp.tool.invoke", Family: "peer", Name: "Invoke MCP tools", Resource: "mcp.open",
		ConstraintKinds: []ConstraintKind{KindToolNames}},
	{ID: "mcp.raw", Family: "peer", Name: "Send raw MCP frames", Resource: "mcp.open"},
	{ID: "cdp.open", Family: "peer", Name: "Open CDP peers", Resource: "cdp.open"},
	{ID: "cdp.list", Family: "peer", Name: "List CDP peers", Resource: "cdp.open"},
	{ID: "cdp.info", Family: "peer", Name: "Inspect CDP peers", Resource: "cdp.open"},
	{ID: "cdp.close", Family: "peer", Name: "Close CDP peers", Resource: "cdp.open"},
	{ID: "cdp.read", Family: "peer", Name: "Read CDP frames", Resource: "cdp.open"},
	{ID: "cdp.protocol.read", Family: "peer", Name: "Read CDP signatures", Resource: "cdp.open"},
	{ID: "cdp.attach", Family: "peer", Name: "Attach CDP targets", Resource: "cdp.open"},
	{ID: "cdp.detach", Family: "peer", Name: "Detach CDP targets", Resource: "cdp.open"},
	{ID: "cdp.method.invoke", Family: "peer", Name: "Invoke CDP methods", Resource: "cdp.open",
		ConstraintKinds: []ConstraintKind{KindMethods}},
	{ID: "cdp.raw", Family: "peer", Name: "Send raw CDP frames", Resource: "cdp.open"},
	{ID: "jrpc.open", Family: "peer", Name: "Open JSON-RPC peers", Resource: "jrpc.open"},
	{ID: "jrpc.list", Family: "peer", Name: "List JSON-RPC peers", Resource: "jrpc.open"},
	{ID: "jrpc.info", Family: "peer", Name: "Inspect JSON-RPC peers", Resource: "jrpc.open"},
	{ID: "jrpc.close", Family: "peer", Name: "Close JSON-RPC peers", Resource: "jrpc.open"},
	{ID: "jrpc.read", Family: "peer", Name: "Read JSON-RPC frames", Resource: "jrpc.open"},
	{ID: "jrpc.method.invoke", Family: "peer", Name: "Invoke JSON-RPC methods", Resource: "jrpc.open",
		ConstraintKinds: []ConstraintKind{KindMethods}},
	{ID: "jrpc.raw", Family: "peer", Name: "Send raw JSON-RPC frames", Resource: "jrpc.open"},

	// Background transfers.
	{ID: "task.list", Family: "task", Name: "List tasks"},
	{ID: "task.info", Family: "task", Name: "Inspect tasks"},
	{ID: "task.cancel", Family: "task", Name: "Cancel tasks"},

	// Saved profiles.
	{ID: "profile.list", Family: "profile", Name: "List profiles", Resource: "profile.list"},
	{ID: "profile.get", Family: "profile", Name: "View profile summaries", Resource: "profile.list"},
	{ID: "profile.create", Family: "profile", Name: "Create profiles", Resource: "profile.create", Global: true},
	{ID: "profile.edit", Family: "profile", Name: "Edit profiles", Resource: "profile.edit", Global: true},
	{ID: "profile.copy", Family: "profile", Name: "Copy profiles", Resource: "profile.create", Global: true},
	{ID: "profile.delete", Family: "profile", Name: "Delete profiles", Resource: "profile.delete", Global: true},

	// Extensions. Using one is scoped to its host; authoring is global.
	{ID: "ext.list", Family: "extension", Name: "List extensions", Resource: "ext.list"},
	{ID: "ext.get", Family: "extension", Name: "View extensions", Resource: "ext.get"},
	{ID: "ext.use", Family: "extension", Name: "Use extensions", Resource: "ext.list"},
	{ID: "ext.release", Family: "extension", Name: "Release extensions", Resource: "ext.list"},
	{ID: "ext.retry", Family: "extension", Name: "Retry extensions", Resource: "ext.retry"},
	{ID: "ext.create", Family: "extension", Name: "Create extensions", Resource: "ext.create", Global: true},
	{ID: "ext.update", Family: "extension", Name: "Update extensions", Resource: "ext.update", Global: true},
	{ID: "ext.delete", Family: "extension", Name: "Delete extensions", Resource: "ext.delete", Global: true},

	// Shared vocabulary and operator notifications.
	{ID: "alias.list", Family: "alias", Name: "List aliases"},
	{ID: "alias.write", Family: "alias", Name: "Write aliases", Global: true},
	{ID: "alias.delete", Family: "alias", Name: "Delete aliases", Global: true},
	{ID: "user.notify", Family: "notify", Name: "Notify the operator", Resource: "user.notify"},
}

var byID = func() map[string]Operation {
	m := make(map[string]Operation, len(catalog))
	for _, op := range catalog {
		if _, dup := m[op.ID]; dup {
			panic("authorization: duplicate operation " + op.ID)
		}
		m[op.ID] = op
	}
	return m
}()

// Catalog returns every semantic operation in a stable order. Callers must not
// mutate it.
func Catalog() []Operation { return catalog }

// Lookup returns one operation and whether this build knows it.
func Lookup(id string) (Operation, bool) {
	op, ok := byID[id]
	return op, ok
}

// FullGrants returns an allow grant for every catalog operation.
func FullGrants() map[string]Grant {
	out := make(map[string]Grant, len(catalog))
	for _, op := range catalog {
		out[op.ID] = Grant{Effect: EffectAllow}
	}
	return out
}

// ValidateGrant checks one grant against the catalog, including which constraints
// the operation can prove and what deny may carry.
func ValidateGrant(id string, grant Grant) error {
	op, ok := Lookup(id)
	if !ok {
		return fmt.Errorf("unknown operation %q", id)
	}
	if grant.Effect != EffectAllow && grant.Effect != EffectDeny {
		return fmt.Errorf("operation %q has an unknown effect %q", id, grant.Effect)
	}
	if grant.Constraints == nil {
		return nil
	}
	if grant.Effect == EffectAllow && grant.Constraints.Empty() {
		return fmt.Errorf("operation %q has empty constraints; omit constraints for an unrestricted allow", id)
	}
	if grant.Effect == EffectDeny && !grant.Constraints.Empty() {
		return fmt.Errorf("deny for %q cannot carry constraints", id)
	}
	if err := ValidateConstraints(op, *grant.Constraints); err != nil {
		return err
	}
	return nil
}

// ValidateConstraints rejects unsupported kinds and malformed values. Unknown
// constraints are a hard error so a future field cannot silently widen a grant.
func ValidateConstraints(op Operation, c Constraints) error {
	if c.Keys != nil {
		if !supports(op, KindKeys) {
			return fmt.Errorf("operation %q does not support keys", op.ID)
		}
		if err := validateStrings(c.Keys, "keys"); err != nil {
			return err
		}
	}
	if c.RepeatMax != nil {
		if !supports(op, KindRepeatMax) {
			return fmt.Errorf("operation %q does not support repeat_max", op.ID)
		}
		if *c.RepeatMax <= 0 {
			return fmt.Errorf("repeat_max must be positive")
		}
	}
	if c.ToolNames != nil {
		if !supports(op, KindToolNames) {
			return fmt.Errorf("operation %q does not support tool_names", op.ID)
		}
		if err := validateStrings(c.ToolNames, "tool_names"); err != nil {
			return err
		}
	}
	if c.Methods != nil {
		if !supports(op, KindMethods) {
			return fmt.Errorf("operation %q does not support methods", op.ID)
		}
		if err := validateStrings(c.Methods, "methods"); err != nil {
			return err
		}
	}
	if c.Forms != nil {
		if !supports(op, KindForms) {
			return fmt.Errorf("operation %q does not support forms", op.ID)
		}
		if err := validateStrings(c.Forms, "forms"); err != nil {
			return err
		}
		for _, form := range c.Forms {
			if form != "inline" && form != "local" && form != "pipe" || op.ID == "file.replace" && form != "inline" {
				return fmt.Errorf("operation %q does not support form %q", op.ID, form)
			}
		}
	}
	if c.MaxBytes != nil {
		if !supports(op, KindMaxBytes) {
			return fmt.Errorf("operation %q does not support max_bytes", op.ID)
		}
		if *c.MaxBytes < 0 {
			return fmt.Errorf("max_bytes cannot be negative")
		}
	}
	return nil
}

func supports(op Operation, kind ConstraintKind) bool {
	for _, k := range op.ConstraintKinds {
		if k == kind {
			return true
		}
	}
	return false
}

func validateStrings(values []string, field string) error {
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			return fmt.Errorf("%s contains an empty or duplicate entry", field)
		}
		seen[value] = true
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
