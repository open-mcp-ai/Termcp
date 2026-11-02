package capability

import (
	"testing"

	"github.com/open-mcp-ai/termcp/internal/authorization"
)

func TestFilePlanUsesEffectiveParametersForBothEndpoints(t *testing.T) {
	for _, tool := range []string{"file_read", "file_write"} {
		args := map[string]any{"mode": "file", "local_path": "transfer", "length": 4}
		plan, err := AgentToolPlan(tool, args)
		if err != nil || len(plan.Requirements) != 2 || !AgentToolTargets(tool, args).NeedsLocal {
			t.Fatalf("%s: %v %v", tool, plan, err)
		}
		for _, req := range plan.Requirements {
			if req.Evidence.Bytes != 4 || !req.Evidence.HasBytes || len(req.Evidence.Forms) != 1 || req.Evidence.Forms[0] != "local" {
				t.Fatalf("%s: endpoint lost effective evidence: %+v", tool, req)
			}
		}
		limit := int64(3)
		policy := authorization.Policy{Actions: map[string]authorization.Grant{
			"file.read":  {Effect: authorization.EffectAllow},
			"file.write": {Effect: authorization.EffectAllow, Constraints: &authorization.Constraints{MaxBytes: &limit}},
		}}
		if authorization.CheckPlan(policy, plan).Allowed {
			t.Fatal("destination bound did not participate in the plan")
		}
	}
	for _, tool := range []string{"file_read", "file_replace"} {
		args := map[string]any{"local_path": "ignored", "length": 4}
		plan, err := AgentToolPlan(tool, args)
		if err != nil || len(plan.Requirements) != 1 || plan.Requirements[0].Evidence.Forms[0] != "inline" || AgentToolTargets(tool, args).NeedsLocal {
			t.Fatalf("unused local_path changed %s's permissions: %v %v", tool, plan, err)
		}
	}
}
