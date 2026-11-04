package extensions

import (
	"errors"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestAuthorizationBindingRejectsTemplateRetarget(t *testing.T) {
	r := newTestReconcilerIn(t, t.TempDir())
	ext := Extension{
		Name:    "bound",
		Target:  "internal",
		Mode:    "pipe",
		Command: "true",
		Allow:   []string{"shell.output"},
	}
	if err := r.store.Save(&ext); err != nil {
		t.Fatal(err)
	}
	binding, err := r.Binding(ext.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if binding.TargetProfile.Identity != sshconfig.InternalIdentity || binding.TemplateVersion == "" {
		t.Fatalf("incomplete binding: %+v", binding)
	}
	ext.Description = "changed after authorization"
	if err := r.store.Save(&ext); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadBound(binding); !errors.Is(err, ErrAuthorizationBindingChanged) {
		t.Fatalf("changed template loaded: %v", err)
	}
}
