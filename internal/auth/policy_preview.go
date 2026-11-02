package auth

import (
	"context"
	"net/http"
)

// PolicyPreviewInput is a read-only authorization simulation. It can describe a
// direct internal-tool call or one DSL command line, but never carries an
// execution override.
type PolicyPreviewInput struct {
	Tool          string         `json:"tool"`
	Arguments     map[string]any `json:"arguments"`
	Cmd           string         `json:"cmd"`
	TermArguments map[string]any `json:"term_arguments"`
}

// PolicyPreviewRequirement explains one operation in the resolved plan.
type PolicyPreviewRequirement struct {
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Allowed   bool   `json:"allowed"`
	Reason    string `json:"reason,omitempty"`
}

// PolicyPreview is the result of a non-executing authorization check.
type PolicyPreview struct {
	Allowed      bool                       `json:"allowed"`
	Reason       string                     `json:"reason,omitempty"`
	Requirements []PolicyPreviewRequirement `json:"requirements"`
	Recipes      []string                   `json:"recipes,omitempty"`
	Resource     string                     `json:"resource_error,omitempty"`
	Executed     bool                       `json:"executed"`
}

// PolicyPreviewer is wired by the composition root after the MCP adapter exists.
// It must only parse and inspect the request; it must not create sessions,
// acquire extensions, renew leases, send protocol bytes or start transfers.
type PolicyPreviewer interface {
	PreviewPolicy(context.Context, AgentPolicy, PolicyPreviewInput) PolicyPreview
}

// SetPolicyPreviewer installs the process-local preview adapter.
func (m *Managed) SetPolicyPreviewer(p PolicyPreviewer) {
	m.mu.Lock()
	m.policyPreviewer = p
	m.mu.Unlock()
}

func (m *Managed) accessPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		authError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		Policy    AgentPolicy    `json:"policy"`
		Tool      string         `json:"tool"`
		Args      map[string]any `json:"arguments"`
		Cmd       string         `json:"cmd"`
		Term      map[string]any `json:"term_arguments"`
		Overrides map[string]any `json:"overrides"`
	}
	if decodeJSON(http.MaxBytesReader(w, r.Body, 262144), &input) != nil {
		authError(w, http.StatusBadRequest, "invalid preview request")
		return
	}
	if len(input.Overrides) != 0 {
		authError(w, http.StatusBadRequest, "preview does not accept execution overrides")
		return
	}
	if err := validateAgentPolicy(input.Policy); err != nil {
		authError(w, http.StatusBadRequest, err.Error())
		return
	}
	if (input.Tool == "") == (input.Cmd == "") {
		authError(w, http.StatusBadRequest, "provide exactly one of tool or cmd")
		return
	}
	m.mu.Lock()
	previewer := m.policyPreviewer
	m.mu.Unlock()
	if previewer == nil {
		authError(w, http.StatusNotImplemented, "authorization preview is unavailable")
		return
	}
	result := previewer.PreviewPolicy(r.Context(), clonePolicy(input.Policy), PolicyPreviewInput{
		Tool: input.Tool, Arguments: input.Args, Cmd: input.Cmd, TermArguments: input.Term,
	})
	result.Executed = false
	authJSON(w, http.StatusOK, result)
}
