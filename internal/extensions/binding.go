package extensions

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

var ErrAuthorizationBindingChanged = errors.New("extension authorization binding changed")

// AuthorizationBinding freezes the extension template and the profile identity
// and content it resolved to when permission was checked.
type AuthorizationBinding struct {
	Name            string                         `json:"name"`
	Target          string                         `json:"target,omitempty"`
	TemplateVersion string                         `json:"template_version"`
	TargetProfile   sshconfig.AuthorizationBinding `json:"target_profile"`
}

func templateVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Binding captures the current extension and target profile without acquiring or
// starting the extension.
func (r *Reconciler) Binding(name, target string) (AuthorizationBinding, error) {
	ext, err := r.store.Load(name)
	if err != nil {
		return AuthorizationBinding{}, err
	}
	t, ok := ext.TargetFor(target)
	if !ok {
		return AuthorizationBinding{}, fmt.Errorf("target %q not found in extension %q", target, name)
	}
	targetHost := t.Host
	view := forTarget(ext, targetHost)
	raw, err := r.store.Template(name)
	if err != nil {
		return AuthorizationBinding{}, err
	}
	if r.sshConfigs == nil {
		return AuthorizationBinding{}, errors.New("no ssh config store configured")
	}
	canonical, identity, err := r.sshConfigs.ResolveReference(view.Target, view.TargetIdentity)
	if err != nil {
		return AuthorizationBinding{}, err
	}
	profileBinding, err := r.sshConfigs.Binding(canonical)
	if err != nil {
		return AuthorizationBinding{}, err
	}
	if profileBinding.Identity != identity {
		return AuthorizationBinding{}, ErrAuthorizationBindingChanged
	}
	rawAfter, err := r.store.Template(name)
	if err != nil {
		return AuthorizationBinding{}, err
	}
	if templateVersion(raw) != templateVersion(rawAfter) {
		return AuthorizationBinding{}, ErrAuthorizationBindingChanged
	}
	return AuthorizationBinding{Name: ext.Name, Target: targetHost, TemplateVersion: templateVersion(raw), TargetProfile: profileBinding}, nil
}

func (r *Reconciler) loadBound(binding AuthorizationBinding) (*Extension, hostPlan, error) {
	ext, err := r.store.Load(binding.Name)
	if err != nil {
		return nil, hostPlan{}, err
	}
	view := forTarget(ext, binding.Target)
	ext = &view
	raw, err := r.store.Template(binding.Name)
	if err != nil {
		return nil, hostPlan{}, err
	}
	if templateVersion(raw) != binding.TemplateVersion || ext.TargetIdentity != binding.TargetProfile.Identity {
		return nil, hostPlan{}, fmt.Errorf("%w: extension template changed", ErrAuthorizationBindingChanged)
	}
	plan, err := r.resolveBound(ext, binding.TargetProfile)
	if err != nil {
		return nil, hostPlan{}, err
	}
	return ext, plan, nil
}

// LoadBound loads an extension using the exact template and target binding that
// authorization approved.
func (r *Reconciler) LoadBound(binding AuthorizationBinding) (*Extension, error) {
	ext, _, err := r.loadBound(binding)
	return ext, err
}

// DeleteBound deletes only the template whose bytes authorization approved.
func (r *Reconciler) DeleteBound(binding AuthorizationBinding) error {
	if err := r.store.deleteBound(binding); err != nil {
		return err
	}
	r.notifyChange()
	return nil
}

// AcquireBound is the authorized Acquire path. The returned instance is built
// from the already validated template and target profile, so a concurrent name
// replacement cannot redirect the launch.
func (r *Reconciler) AcquireBound(binding AuthorizationBinding, wait time.Duration) (Status, error) {
	ext, plan, err := r.loadBound(binding)
	if err != nil {
		return Status{}, err
	}
	return r.acquirePlan(binding.Name, binding.Target, ext, plan, wait)
}

// RetryBound is the authorized manual retry path.
func (r *Reconciler) RetryBound(binding AuthorizationBinding) error {
	ext, plan, err := r.loadBound(binding)
	if err != nil {
		return err
	}
	if !ext.IsEnabled() {
		return errors.New("extension is disabled; enable it before retrying")
	}
	if ext.Lazy() {
		_, err := r.acquirePlan(binding.Name, binding.Target, ext, plan, 0)
		return err
	}
	st := r.stateFor(binding.Name, binding.Target)
	st.want()
	r.detach(st)
	st.mark(StatusUnavailable)
	st.resetRetryBudget()
	r.startPreparedProbe(r.runCtx(), binding.Name, binding.Target, ext, plan)
	return nil
}

func (s *Store) deleteBound(binding AuthorizationBinding) error {
	p, err := s.configPath(binding.Name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("extension %q %w", binding.Name, ErrNotFound)
		}
		return err
	}
	if templateVersion(raw) != binding.TemplateVersion {
		return fmt.Errorf("%w: extension template changed", ErrAuthorizationBindingChanged)
	}
	return s.removeLocked(p)
}
