package authorization

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
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

func TestCheckPlanReportsApprovalWithoutTreatingItAsDeny(t *testing.T) {
	policy := Policy{Actions: map[string]Grant{
		"shell.input":  {Effect: EffectAllow},
		"shell.key":    {Effect: EffectAsk},
		"shell.output": {Effect: EffectAllow},
	}}
	decision := CheckPlan(policy, Plan{Requirements: []Requirement{
		{Operation: "shell.input"},
		{Operation: "shell.key", Evidence: Evidence{Keys: []string{"enter"}, Repeat: 1}},
		{Operation: "shell.output"},
	}})
	if !decision.Allowed || !decision.NeedsApproval || decision.Reason != "" {
		t.Fatalf("ask decision = %+v", decision)
	}
	policy.Actions["shell.output"] = Grant{Effect: EffectDeny}
	decision = CheckPlan(policy, Plan{Requirements: []Requirement{
		{Operation: "shell.key", Evidence: Evidence{Keys: []string{"enter"}, Repeat: 1}},
		{Operation: "shell.output"},
	}})
	if decision.Allowed || !strings.Contains(decision.Reason, "denied") {
		t.Fatalf("deny did not win: %+v", decision)
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

func TestPeerToolAndMethodAllowlists(t *testing.T) {
	policy := Policy{Actions: map[string]Grant{
		"mcp.open": {Effect: EffectAllow, Constraints: &Constraints{ToolNames: []string{"status"}}},
		"cdp.open": {Effect: EffectAllow, Constraints: &Constraints{Methods: []string{"Page.navigate"}}},
	}}
	if decision := CheckPlan(policy, Plan{Requirements: []Requirement{
		{Operation: "mcp.open", Evidence: Evidence{ToolName: "status"}},
		{Operation: "cdp.open", Evidence: Evidence{Method: "Page.navigate"}},
	}}); !decision.Allowed {
		t.Fatal("allowed invocation refused", decision.Reason)
	}
	for _, bad := range []Requirement{
		{Operation: "mcp.open", Evidence: Evidence{ToolName: "shell"}},
		{Operation: "mcp.open", Evidence: Evidence{}},
		{Operation: "cdp.open", Evidence: Evidence{Method: "Runtime.evaluate"}},
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
	if err := ValidateGrant("session.start", Grant{Effect: "approval"}); err == nil {
		t.Fatal("unsupported effect accepted")
	}
	if err := ValidateGrant("session.start", Grant{Effect: EffectAsk}); err != nil {
		t.Fatal("ask effect rejected:", err)
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

func TestCatalogMetadataIsCompleteAndRecipesReferenceKnownOperations(t *testing.T) {
	for _, op := range Catalog() {
		if op.Risk != RiskStandard && op.Risk != RiskElevated && op.Risk != RiskHigh {
			t.Errorf("%s has invalid risk %q", op.ID, op.Risk)
		}
		if len(op.AccessPaths) == 0 {
			t.Errorf("%s has no declared access path", op.ID)
		}
		if len(op.DSLVerbs) > 0 && !containsAccessPath(op.AccessPaths, AccessDSL) {
			t.Errorf("%s lists DSL verbs without DSL reachability", op.ID)
		}
		if len(op.InternalTools) > 0 && !containsAccessPath(op.AccessPaths, AccessTool) {
			t.Errorf("%s lists tools without tool reachability", op.ID)
		}
		if len(op.WebUIRoutes) > 0 && !containsAccessPath(op.AccessPaths, AccessWebUI) {
			t.Errorf("%s lists WebUI routes without WebUI reachability", op.ID)
		}
		for _, dependency := range op.Dependencies {
			if _, ok := Lookup(dependency); !ok || dependency == op.ID {
				t.Errorf("%s has invalid dependency %q", op.ID, dependency)
			}
		}
		for _, parameter := range op.Parameters {
			if !supports(op, parameter.Kind) {
				t.Errorf("%s documents unsupported parameter %q", op.ID, parameter.Kind)
			}
		}
	}
	for _, id := range []string{"shell.input", "shell.key", "shell.detect"} {
		op, _ := Lookup(id)
		if op.Risk == RiskHigh {
			t.Errorf("core host operation %s is mislabeled high risk", id)
		}
	}
	for _, id := range []string{"mcp.open", "cdp.open", "jrpc.open", "profile.edit", "ext.create"} {
		op, _ := Lookup(id)
		if op.Risk != RiskHigh {
			t.Errorf("configuration/raw operation %s risk = %q", id, op.Risk)
		}
		if op.Description == "" {
			t.Errorf("operation %s has no description", id)
		}
	}
	seen := map[string]bool{}
	for _, recipe := range Recipes() {
		if recipe.ID == "" || seen[recipe.ID] || len(recipe.Requirements) == 0 {
			t.Errorf("invalid recipe %+v", recipe)
		}
		seen[recipe.ID] = true
		for _, id := range append(append([]string{}, recipe.Requirements...), recipe.Optional...) {
			if _, ok := Lookup(id); !ok {
				t.Errorf("recipe %s references unknown operation %s", recipe.ID, id)
			}
		}
	}
}

func TestPermissionsDocumentTracksCatalog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "permissions.md"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?m)^\\| [^|]+ \\| `([^`]+)` \\|")
	operationID := regexp.MustCompile(`^(?:session|shell|file|profile|ext|forward|mcp|cdp|jrpc|user)\.[a-z0-9._-]+$`)
	documented := map[string]bool{}
	for _, match := range re.FindAllSubmatch(raw, -1) {
		id := string(match[1])
		if operationID.MatchString(id) {
			documented[id] = true
		}
	}
	for _, op := range Catalog() {
		if !documented[op.ID] {
			t.Errorf("docs/permissions.md is missing operation %s", op.ID)
		}
		delete(documented, op.ID)
	}
	for id := range documented {
		t.Errorf("docs/permissions.md documents non-catalog operation %s", id)
	}
}

func containsAccessPath(paths []AccessPath, wanted AccessPath) bool {
	for _, path := range paths {
		if path == wanted {
			return true
		}
	}
	return false
}
