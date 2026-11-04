package authorization

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestConstraintSelectionsSurviveJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name, operation    string
		empty, selected    Constraints
		allowed, forbidden Evidence
	}{
		{"keys", "shell.key", Constraints{Keys: []string{}}, Constraints{Keys: []string{"tab"}}, Evidence{Keys: []string{"tab"}, Repeat: 1}, Evidence{Keys: []string{"enter"}, Repeat: 1}},
		{"tool_names", "mcp.open", Constraints{ToolNames: []string{}}, Constraints{ToolNames: []string{"status"}}, Evidence{ToolName: "status"}, Evidence{ToolName: "execute"}},
		{"methods", "cdp.open", Constraints{Methods: []string{}}, Constraints{Methods: []string{"Page.reload"}}, Evidence{Method: "Page.reload"}, Evidence{Method: "Runtime.evaluate"}},
		{"forms", "file.write", Constraints{Forms: []string{}}, Constraints{Forms: []string{"inline"}}, Evidence{Forms: []string{"inline"}}, Evidence{Forms: []string{"local"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []Constraints{{}, tc.empty, tc.selected} {
				raw, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				var restored Constraints
				if err = json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(c, restored) {
					t.Fatalf("selection changed during JSON round-trip: %s", raw)
				}
			}
			for _, c := range []*Constraints{nil, &tc.empty, &tc.selected} {
				policy := Policy{Actions: map[string]Grant{tc.operation: {Effect: EffectAllow, Constraints: c}}}
				raw, err := json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				var restored Policy
				if err = json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				for _, e := range []Evidence{tc.allowed, tc.forbidden} {
					plan := Plan{Requirements: []Requirement{{Operation: tc.operation, Evidence: e}}}
					before, after := CheckPlan(policy, plan), CheckPlan(restored, plan)
					if before.Allowed != after.Allowed {
						t.Fatalf("JSON round-trip changed authorization from %v to %v: %s", before.Allowed, after.Allowed, raw)
					}
				}
			}
		})
	}
}
