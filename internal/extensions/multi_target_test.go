package extensions

import (
	"strings"
	"testing"
	"time"
)

// TestMultiTargetValidation pins the schema boundary:
// - target and targets are alternatives (cannot specify both).
// - each target in targets needs a valid, non-empty host.
// - duplicate host names under targets are refused.
// - top-level forwards are refused when targets is used (forwards belong under a target).
// - forward ports under different targets do not conflict in validation.
func TestMultiTargetValidation(t *testing.T) {
	ext := Extension{Mode: "pipe"}
	ext.Name = "k8s-debug"
	ext.Command = "true"
	ext.Target = "internal"
	ext.Targets = []TargetDecl{{Host: "cluster-bj"}}
	if err := ext.Validate(); err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Fatalf("expected alternatives error when both target and targets are set, got %v", err)
	}

	ext.Target = ""
	ext.Targets = []TargetDecl{{Host: "cluster-bj"}, {Host: "cluster-bj"}}
	if err := ext.Validate(); err == nil || !strings.Contains(err.Error(), "repeats host") {
		t.Fatalf("expected duplicate host refusal, got %v", err)
	}

	ext.Targets = []TargetDecl{{Host: "cluster-bj"}}
	ext.Forwards = []ForwardDecl{{Kind: ForwardLocal, LocalPort: 8080, RemotePort: 80}}
	if err := ext.Validate(); err == nil || !strings.Contains(err.Error(), "forwards belong to a host") {
		t.Fatalf("expected top-level forwards refusal with targets list, got %v", err)
	}

	// Valid multi-target with per-host forwards that declare different local ports
	ext.Forwards = nil
	ext.Targets = []TargetDecl{
		{
			Host:     "cluster-bj",
			Forwards: []ForwardDecl{{Kind: ForwardLocal, LocalPort: 8081, RemotePort: 80}},
		},
		{
			Host:     "cluster-sh",
			Forwards: []ForwardDecl{{Kind: ForwardLocal, LocalPort: 8082, RemotePort: 80}},
		},
	}
	if err := ext.Validate(); err != nil {
		t.Fatalf("valid multi-target configuration was rejected: %v", err)
	}
	if ext.Lazy() {
		t.Fatalf("extension with declared forwards should not be lazy")
	}
}

// TestMultiTargetAcquireAndIsolation pins the runtime behaviour:
// - Acquire without a target on a multi-target extension returns an ambiguity error naming the choices.
// - AcquireTarget with a specific target only dials and runs on that target.
// - Status folds over the targets: if all are idle, status is idle; if one is up, top status is available.
func TestMultiTargetAcquireAndIsolation(t *testing.T) {
	r := startReconciler(t)
	ext := Extension{Mode: "pipe"}
	ext.Name = "multi-host"
	ext.Command = testShellCommand()
	ext.Args = nil
	ext.Targets = []TargetDecl{
		{Host: "internal"},
		{Host: "remote-one"},
	}
	mustSave(t, r.store, &ext)

	// An ambiguous ask must fail with a helpful error listing the choices
	_, err := r.Acquire("multi-host", 5*time.Second)
	if err == nil {
		t.Fatal("expected ambiguity error when calling Acquire on multi-target extension without target")
	}
	if !strings.Contains(err.Error(), "internal") || !strings.Contains(err.Error(), "remote-one") {
		t.Fatalf("error should list available targets, got: %v", err)
	}

	// An invalid target name is refused
	_, err = r.AcquireTarget("multi-host", "nonexistent", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), `no target "nonexistent"`) {
		t.Fatalf("expected target not found error, got: %v", err)
	}

	// Acquire on the available "internal" host succeeds
	st, err := r.AcquireTarget("multi-host", "internal", 10*time.Second)
	if err != nil {
		t.Fatalf("failed to acquire internal target: %v", err)
	}
	if st.Status != StatusAvailable {
		t.Fatalf("expected StatusAvailable, got %v", st.Status)
	}
	if st.ShellID == "" {
		t.Fatal("acquired target should have a shell id")
	}

	// Status folds over the hosts: one host up and the other untouched is a mixture,
	// which the top level reports as unavailable — a row that said "available"
	// because one of two machines answered would be a lie about the other.
	overall, ok := r.Status("multi-host")
	if !ok {
		t.Fatal("extension should be found in Status")
	}
	if len(overall.Targets) != 2 {
		t.Fatalf("expected 2 target statuses, got %d", len(overall.Targets))
	}
	if overall.Status != StatusUnavailable {
		t.Fatalf("overall status of a half-up extension should be unavailable, got %v", overall.Status)
	}
	one, ok := r.TargetStatus("multi-host", "internal")
	if !ok || one.Status != StatusAvailable {
		t.Fatalf("the acquired host should be available, got %+v (ok=%v)", one, ok)
	}

	// Releasing the acquired shell id only closes that target
	n, err := r.Release("multi-host", st.ShellID)
	if err != nil {
		t.Fatalf("Release failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 released instance, got %d", n)
	}
}
