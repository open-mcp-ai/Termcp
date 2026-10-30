package authorization

import (
	"reflect"
	"testing"
)

func TestCheckPlanRequiresEveryAllowGrant(t *testing.T) {
	policy := Policy{Actions: map[string]Grant{
		"session.start": {Effect: EffectAllow},
		"shell.input":   {Effect: EffectAllow},
	}}
	if decision := CheckPlan(policy, Plan{Requirements: []Requirement{
		{Operation: "session.start"}, {Operation: "shell.input"},
	}}); !decision.Allowed {
		t.Fatal("complete grant refused", decision.Reason)
	}
	for _, missing := range []string{"shell.key", "shell.output"} {
		decision := CheckPlan(policy, Plan{Requirements: []Requirement{
			{Operation: "session.start"}, {Operation: missing},
		}})
		if decision.Allowed || decision.Reason == "" {
			t.Fatalf("missing grant %q passed", missing)
		}
	}
}

func TestCheckPlanDenyWins(t *testing.T) {
	policy := Policy{Actions: map[string]Grant{
		"shell.input": {Effect: EffectAllow},
		"shell.key":   {Effect: EffectDeny},
	}}
	decision := CheckPlan(policy, Plan{Requirements: []Requirement{
		{Operation: "shell.input"}, {Operation: "shell.key", Evidence: Evidence{Keys: []string{"enter"}, Repeat: 1}},
	}})
	if decision.Allowed || decision.Reason == "" {
		t.Fatal("denied composite plan passed")
	}
}

func TestConstraintsNarrowTheGrant(t *testing.T) {
	max := 4
	policy := Policy{Actions: map[string]Grant{
		"shell.key": {Effect: EffectAllow, Constraints: &Constraints{
			Keys: []string{"tab"}, RepeatMax: &max,
		}},
	}}
	pass := Evidence{Keys: []string{"tab"}, Repeat: 1}
	if decision := CheckPlan(policy, Plan{Requirements: []Requirement{{Operation: "shell.key", Evidence: pass}}}); !decision.Allowed {
		t.Fatal("allowed key refused", decision.Reason)
	}
	for _, bad := range []Evidence{
		{Keys: []string{"enter"}, Repeat: 1},
		{Keys: []string{"tab"}, Repeat: 5},
		{Repeat: 1},
	} {
		if decision := CheckPlan(policy, Plan{Requirements: []Requirement{{Operation: "shell.key", Evidence: bad}}}); decision.Allowed {
			t.Fatal("violating key passed", bad)
		}
	}
}

func TestPayloadSizeMustBeProvable(t *testing.T) {
	limit := int64(4)
	policy := Policy{Actions: map[string]Grant{
		"file.write": {Effect: EffectAllow, Constraints: &Constraints{
			Forms: []string{"inline"}, MaxBytes: &limit,
		}},
	}}
	pass := Evidence{Forms: []string{"inline"}, Bytes: 4, HasBytes: true}
	if decision := CheckPlan(policy, Plan{Requirements: []Requirement{{Operation: "file.write", Evidence: pass}}}); !decision.Allowed {
		t.Fatal("sized inline write refused", decision.Reason)
	}
	for _, bad := range []Evidence{
		{Forms: []string{"local"}, Bytes: 0, HasBytes: false},
		{Forms: []string{"inline"}, Bytes: 5, HasBytes: true},
	} {
		if decision := CheckPlan(policy, Plan{Requirements: []Requirement{{Operation: "file.write", Evidence: bad}}}); decision.Allowed {
			t.Fatal("unsized or oversized write passed", bad)
		}
	}
}

func TestToolAndMethodAllowlists(t *testing.T) {
	policy := Policy{Actions: map[string]Grant{
		"mcp.tool.invoke":   {Effect: EffectAllow, Constraints: &Constraints{ToolNames: []string{"status"}}},
		"cdp.method.invoke": {Effect: EffectAllow, Constraints: &Constraints{Methods: []string{"Page.navigate"}}},
	}}
	if decision := CheckPlan(policy, Plan{Requirements: []Requirement{
		{Operation: "mcp.tool.invoke", Evidence: Evidence{ToolName: "status"}},
		{Operation: "cdp.method.invoke", Evidence: Evidence{Method: "Page.navigate"}},
	}}); !decision.Allowed {
		t.Fatal("allowed invocation refused", decision.Reason)
	}
	for _, bad := range []Requirement{
		{Operation: "mcp.tool.invoke", Evidence: Evidence{ToolName: "shell"}},
		{Operation: "mcp.tool.invoke", Evidence: Evidence{}},
		{Operation: "cdp.method.invoke", Evidence: Evidence{Method: "Runtime.evaluate"}},
	} {
		if decision := CheckPlan(policy, Plan{Requirements: []Requirement{bad}}); decision.Allowed {
			t.Fatal("violating invocation passed", bad)
		}
	}
}

func TestValidateGrantRejectsUnknownAndMalformed(t *testing.T) {
	if err := ValidateGrant("session.nope", Grant{Effect: EffectAllow}); err == nil {
		t.Fatal("unknown operation accepted")
	}
	if err := ValidateGrant("session.start", Grant{Effect: "ask"}); err == nil {
		t.Fatal("unsupported effect accepted")
	}
	if err := ValidateGrant("shell.input", Grant{Effect: EffectDeny, Constraints: &Constraints{
		Keys: []string{"enter"},
	}}); err == nil {
		t.Fatal("deny with constraints accepted")
	}
	if err := ValidateGrant("session.start", Grant{Effect: EffectAllow, Constraints: &Constraints{
		Keys: []string{"enter"},
	}}); err == nil {
		t.Fatal("unsupported constraint accepted")
	}
	repeat := -1
	if err := ValidateGrant("shell.key", Grant{Effect: EffectAllow, Constraints: &Constraints{
		RepeatMax: &repeat,
	}}); err == nil {
		t.Fatal("negative repeat accepted")
	}
	if err := ValidateGrant("shell.key", Grant{Effect: EffectAllow, Constraints: &Constraints{
		Keys: []string{"enter", "enter"},
	}}); err == nil {
		t.Fatal("duplicate keys accepted")
	}
}

func TestCatalogIsCompleteAndIndexed(t *testing.T) {
	grants := FullGrants()
	if len(grants) != len(Catalog()) {
		t.Fatal("full grants and catalog disagree")
	}
	for _, op := range Catalog() {
		if lookup, ok := Lookup(op.ID); !ok || !reflect.DeepEqual(lookup, op) {
			t.Fatalf("catalog lookup mismatch for %q", op.ID)
		}
		if err := ValidateGrant(op.ID, Grant{Effect: EffectAllow}); err != nil {
			t.Fatalf("catalog grant for %q invalid: %v", op.ID, err)
		}
	}
}
