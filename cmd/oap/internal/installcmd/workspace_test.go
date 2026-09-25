package installcmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// An explicitly-named --workspace-storage-class must still be provisioning-
// probed, not trusted blindly: a class that cannot actually provision (a
// disabled cloud API, a size below the class floor, a typo) has to fail
// `oap install` loudly instead of being stamped onto the operator and
// discovered only when the first session's PVC hangs.
func TestResolveWorkspaceDecision_ExplicitClassIsProbed(t *testing.T) {
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), false)
	dec, err := resolveWorkspaceDecision(context.Background(), nil, nil,
		WorkspaceResolveOptions{ExplicitClass: "enterprise-multishare-rwx"}, rep)
	require.NoError(t, err)
	assert.Equal(t, "enterprise-multishare-rwx", dec.ClassName)
	assert.Equal(t, cloud.WorkspaceProbeBeforeUse, dec.Verify,
		"an explicit class must be probed so a class that can't provision fails oap install")
}

func TestWorkspaceMarker_ReadWrite(t *testing.T) {
	kc := fake.NewSimpleClientset()
	// Pre-create the namespace so the ConfigMap write doesn't 404.
	_, err := kc.CoreV1().Namespaces().Create(
		context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agentprimitives-system"}},
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	// Read on empty cluster → ("", false, nil).
	got, ok, err := readWorkspaceMarker(context.Background(), kc)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, got)

	// Write → idempotent re-write.
	require.NoError(t, writeWorkspaceMarker(context.Background(), kc, "ap-workspace-rwx"))
	require.NoError(t, writeWorkspaceMarker(context.Background(), kc, "ap-workspace-rwx"))

	// Read → returns the stored value.
	got, ok, err = readWorkspaceMarker(context.Background(), kc)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "ap-workspace-rwx", got)

	// Overwrite with a different value.
	require.NoError(t, writeWorkspaceMarker(context.Background(), kc, "efs-sc"))
	got, ok, err = readWorkspaceMarker(context.Background(), kc)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "efs-sc", got)
}

func TestApplyWorkspaceProvisioner_WaitsForReady(t *testing.T) {
	applied := 0
	stub := func(ctx context.Context, _ dynamic.Interface, d *unstructured.Unstructured, _ string) error {
		applied++
		return nil
	}

	kc := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ap-workspace-provisioner",
				Namespace: "ap-workspace-storage",
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
		},
	)

	docs := []*unstructured.Unstructured{
		{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "ap-workspace-storage"}}},
		{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "ap-workspace-provisioner", "namespace": "ap-workspace-storage"}}},
	}
	err := applyWorkspaceProvisioner(context.Background(), docs, nil, kc, stub)
	require.NoError(t, err)
	assert.Equal(t, 2, applied, "all docs applied")
}

func TestPatchOperatorWorkspaceClass_AddsFlag(t *testing.T) {
	kc := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-operator", Namespace: "agentprimitives-system"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "operator",
						Args: []string{"--runner-image=foo", "--leader-elect=true"},
					}},
				},
			},
		},
	})
	require.NoError(t, patchOperatorWorkspaceClass(context.Background(), kc, "ap-workspace-rwx"))
	dep, err := kc.AppsV1().Deployments("agentprimitives-system").Get(context.Background(), "spicebox-operator", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, dep.Spec.Template.Spec.Containers[0].Args, "--workspace-storage-class=ap-workspace-rwx")
}

func TestPatchOperatorWorkspaceClass_UpdatesExistingFlag(t *testing.T) {
	kc := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-operator", Namespace: "agentprimitives-system"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "operator",
						Args: []string{"--workspace-storage-class=old-class", "--leader-elect=true"},
					}},
				},
			},
		},
	})
	require.NoError(t, patchOperatorWorkspaceClass(context.Background(), kc, "new-class"))
	dep, err := kc.AppsV1().Deployments("agentprimitives-system").Get(context.Background(), "spicebox-operator", metav1.GetOptions{})
	require.NoError(t, err)
	args := dep.Spec.Template.Spec.Containers[0].Args
	assert.Contains(t, args, "--workspace-storage-class=new-class")
	count := 0
	for _, a := range args {
		if strings.HasPrefix(a, "--workspace-storage-class=") {
			count++
		}
	}
	assert.Equal(t, 1, count, "exactly one --workspace-storage-class arg")
}

// ---- resolveWorkspaceStorageWithProbe tests ----

// TestWorkspaceProbe_SlowButProvisions_DoesNotFallBack verifies the core
// no-automatic-fallbacks invariant: when the probe eventually reports bound,
// resolveWorkspaceStorageWithProbe returns the RWX class and NEVER emits a
// "did not provision" / "using isolated /work" warning, regardless of how
// long the provisioner takes in real life. The probe deliberately returns
// not-yet-bound on the first poll and binds on the second, so the patient-wait
// loop genuinely iterates before succeeding — proving the no-fixed-wall behavior
// under real looping, not just an immediate-bind short-circuit.
func TestWorkspaceProbe_SlowButProvisions_DoesNotFallBack(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), true) // non-TTY → streaming, non-interactive

	// Probe returns transient-not-bound on the first call, then binds.
	// This forces the await loop to iterate once (first backoff ~2s) before
	// the probe signals success, exercising the no-fallback invariant under
	// real polling rather than an immediate-bind short-circuit.
	var polls int
	probe := cloud.ProbeStep(func(_ context.Context) (bool, string, error) {
		polls++
		if polls < 2 {
			return false, "ProvisioningPending: PVC not yet bound", nil
		}
		return true, "", nil
	})

	chosen, err := awaitProvisioningProbe(ctx, context.Background(), "workspace storage ap-workspace-rwx", "ap-workspace-rwx", probe, rep)
	require.NoError(t, err)
	assert.Equal(t, "ap-workspace-rwx", chosen)
	assert.NotContains(t, buf.String(), "did not provision", "must not emit the old static-timeout fallback message")
	assert.NotContains(t, buf.String(), "using isolated /work", "must not silently fall back to isolated storage")
}

// TestWorkspaceProbe_NeverBinds_NonInteractive_ReturnsError verifies that a
// probe that never binds returns a hard error in non-interactive mode (--yes /
// non-TTY) rather than silently degrading to isolated /work.
func TestWorkspaceProbe_NeverBinds_NonInteractive_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	rep := progress.New(&buf, strings.NewReader(""), true) // assumeYes=true → non-interactive

	// Probe never binds, simulating a permanently-failing storage class.
	probe := cloud.ProbeStep(func(_ context.Context) (bool, string, error) {
		return false, "ProvisioningFailed: helper pod not yet scheduled", nil
	})

	_, err := awaitProvisioningProbe(ctx, context.Background(), "workspace storage ap-workspace-rwx", "ap-workspace-rwx", probe, rep)
	require.Error(t, err, "non-interactive timeout must return error, not silently fall back to isolated /work")
	assert.NotContains(t, buf.String(), "using isolated /work", "error path must never emit the silent-fallback message")
}

// TestHandleBundledProvisionerFailure verifies the §A3 no-automatic-fallbacks
// invariant: a bundled provisioner apply error must require explicit opt-out or
// interactive consent before degrading to isolated /work. Non-interactive and
// --yes paths must return a hard error when NoRWX is not set.
func TestHandleBundledProvisionerFailure(t *testing.T) {
	cause := fmt.Errorf("apply failed: network timeout")

	cases := []struct {
		name        string
		wsOpts      WorkspaceResolveOptions
		assumeYes   bool
		interactive bool
		userInput   string // piped to stdin; "y\n" or "n\n" or ""
		wantDegrade bool
		wantErr     bool
	}{
		{
			name:        "non-interactive without explicit opt-out: returns error, not silent degrade",
			wsOpts:      WorkspaceResolveOptions{},
			assumeYes:   false,
			interactive: false,
			wantDegrade: false,
			wantErr:     true,
		},
		{
			name:        "assumeYes without explicit opt-out: returns error, not silent degrade",
			wsOpts:      WorkspaceResolveOptions{},
			assumeYes:   true,
			interactive: true,
			wantDegrade: false,
			wantErr:     true,
		},
		{
			name:        "assumeYes non-interactive without explicit opt-out: returns error",
			wsOpts:      WorkspaceResolveOptions{},
			assumeYes:   true,
			interactive: false,
			wantDegrade: false,
			wantErr:     true,
		},
		{
			name:        "explicit NoRWX opt-out: degrades silently",
			wsOpts:      WorkspaceResolveOptions{NoRWX: true},
			assumeYes:   false,
			interactive: false,
			wantDegrade: true,
			wantErr:     false,
		},
		{
			name:        "interactive user consents to degrade: degrades",
			wsOpts:      WorkspaceResolveOptions{},
			assumeYes:   false,
			interactive: true,
			userInput:   "y\n",
			wantDegrade: true,
			wantErr:     false,
		},
		{
			name:        "interactive user declines degrade: returns error",
			wsOpts:      WorkspaceResolveOptions{},
			assumeYes:   false,
			interactive: true,
			userInput:   "n\n",
			wantDegrade: false,
			wantErr:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := strings.NewReader(tc.userInput)
			var buf bytes.Buffer
			// The reporter's own stdin is empty; the prompt reads the piped `in`
			// passed alongside it (the Suspend wrapper only quiesces the region).
			rep := progress.New(&buf, strings.NewReader(""), tc.assumeYes)
			degrade, err := handleBundledProvisionerFailure(cause, tc.wsOpts, tc.assumeYes, tc.interactive, rep, in, &buf)
			assert.Equal(t, tc.wantDegrade, degrade, "degrade flag")
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorContains(t, err, "bundled workspace provisioner failed")
				assert.ErrorIs(t, err, cause)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestDecisionToChoice verifies the up-front decision→choice transform that the
// frontloaded resolver applies, including the interactive Filestore cost-confirm
// prompt. It is exercised non-interactively (piped stdin, interactive=true,
// assumeYes=false) so the accept/decline branches run deterministically: an
// accepted cost-confirm yields ClassName, a declined one degrades to isolated
// /work (ClassName "" + the declined note). The probe/use-directly/degraded
// shapes are mapped without any prompt.
func TestDecisionToChoice(t *testing.T) {
	cases := []struct {
		name       string
		dec        cloud.Decision
		userInput  string // piped to the prompt; only the cost-confirm cases read it
		want       workspaceChoice
		wantOutHas string // substring the reporter must have printed ("" = no check)
	}{
		{
			name:       "degraded → isolated with guidance message, no probe",
			dec:        cloud.Decision{Degraded: true, Message: "no RWX here; isolated /work"},
			want:       workspaceChoice{Message: "no RWX here; isolated /work"},
			wantOutHas: "",
		},
		{
			name: "marker (UseDirectly) → ClassName, no probe, no bundled",
			dec:  cloud.Decision{ClassName: "efs-sc", Verify: cloud.WorkspaceUseDirectly},
			want: workspaceChoice{ClassName: "efs-sc"},
		},
		{
			name: "UseDirectly ignores NeedsBundled (marker for bundled class)",
			dec:  cloud.Decision{ClassName: cloud.BundledWorkspaceStorageClass, NeedsBundled: true, Verify: cloud.WorkspaceUseDirectly},
			want: workspaceChoice{ClassName: cloud.BundledWorkspaceStorageClass},
		},
		{
			name:       "cost-confirm accepted → ClassName, warning shown",
			dec:        cloud.Decision{ClassName: "premium-rwx", Verify: cloud.WorkspaceCostConfirmBeforeUse, CostWarning: "Filestore is billable"},
			userInput:  "y\n",
			want:       workspaceChoice{ClassName: "premium-rwx"},
			wantOutHas: "Filestore is billable",
		},
		{
			name:       "cost-confirm declined → isolated /work, declined note carried",
			dec:        cloud.Decision{ClassName: "premium-rwx", Verify: cloud.WorkspaceCostConfirmBeforeUse, CostWarning: "Filestore is billable"},
			userInput:  "n\n",
			want:       workspaceChoice{Message: "declined Filestore; using isolated /work (pass --workspace-storage-class or -y to use it)"},
			wantOutHas: "Filestore is billable",
		},
		{
			name: "probe + bundled (GKE Standard / local) → probe ClassName, apply bundled",
			dec:  cloud.Decision{ClassName: cloud.BundledWorkspaceStorageClass, NeedsBundled: true, Verify: cloud.WorkspaceProbeBeforeUse},
			want: workspaceChoice{ClassName: cloud.BundledWorkspaceStorageClass, NeedsBundled: true, Probe: true},
		},
		{
			name: "probe existing RWX class → probe ClassName, no bundled",
			dec:  cloud.Decision{ClassName: "efs-sc", Verify: cloud.WorkspaceProbeBeforeUse},
			want: workspaceChoice{ClassName: "efs-sc", Probe: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rep := progress.New(&buf, strings.NewReader(""), false)
			in := strings.NewReader(tc.userInput)
			// interactive=true + assumeYes=false: the prompt reads `in`, so the
			// accept/decline branch is driven deterministically by the piped input.
			got := decisionToChoice(tc.dec, rep, &buf, in, false /*assumeYes*/, true /*interactive*/)
			assert.Equal(t, tc.want, got)
			if tc.wantOutHas != "" {
				assert.Contains(t, buf.String(), tc.wantOutHas, "reporter output must surface the warning")
			}
		})
	}
}

// TestWizardWorkspaceExplicitClass pins the wizard's workspace-forwarding
// decision (M3): keep/empty picks forward nothing; a non-TTY (EOF-fabricated)
// pick forwards nothing; a genuine interactive pick of a non-billable class
// forwards it as an override; and a billable Filestore pick forwards it ONLY
// when the cost consent is granted. It also proves the consent is asked exactly
// when it should be — never for a keep, a non-TTY run, or a non-billable class.
func TestWizardWorkspaceExplicitClass(t *testing.T) {
	classes := []cloud.RWXClassInfo{
		{Name: "ap-workspace-rwx", Bundled: true},
		{Name: "enterprise-multishare-rwx", Filestore: true, Multishare: true},
	}
	cases := []struct {
		name         string
		chosen       string
		detected     string
		interactive  bool
		confirmYes   bool
		wantClass    string
		wantConfirms int
	}{
		{name: "keep current: forwards nothing, no consent", chosen: "ap-workspace-rwx", detected: "ap-workspace-rwx", interactive: true, wantClass: "", wantConfirms: 0},
		{name: "nothing chosen: forwards nothing, no consent", chosen: "", detected: "", interactive: true, wantClass: "", wantConfirms: 0},
		{name: "non-TTY fabricated pick: dropped, no consent", chosen: "enterprise-multishare-rwx", detected: "", interactive: false, wantClass: "", wantConfirms: 0},
		{name: "interactive non-billable pick: forwarded, no consent", chosen: "ap-workspace-rwx", detected: "", interactive: true, wantClass: "ap-workspace-rwx", wantConfirms: 0},
		{name: "interactive billable pick, consented: forwarded, consent asked", chosen: "enterprise-multishare-rwx", detected: "", interactive: true, confirmYes: true, wantClass: "enterprise-multishare-rwx", wantConfirms: 1},
		{name: "interactive billable pick, declined: dropped, consent asked", chosen: "enterprise-multishare-rwx", detected: "", interactive: true, confirmYes: false, wantClass: "", wantConfirms: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			confirms := 0
			got := wizardWorkspaceExplicitClass(tc.chosen, tc.detected, classes, tc.interactive, func(class string) bool {
				confirms++
				assert.Equal(t, tc.chosen, class, "consent must be asked for the chosen class")
				return tc.confirmYes
			})
			assert.Equal(t, tc.wantClass, got, "forwarded ExplicitClass")
			assert.Equal(t, tc.wantConfirms, confirms, "cost consent asked exactly when it should be")
		})
	}
}

// fakeEnabler is a WorkspaceStorage that also carries the optional enable
// capability, recording whether EnsureWorkspaceStorage was invoked.
type fakeEnabler struct{ called bool }

func (f *fakeEnabler) Resolve(context.Context, cloud.WorkspaceParams) (cloud.Decision, error) {
	return cloud.Decision{}, nil
}
func (f *fakeEnabler) EnsureWorkspaceStorage(context.Context, cloud.WorkspaceEnableParams) error {
	f.called = true
	return nil
}

// plainWS is a WorkspaceStorage WITHOUT the enable capability (EKS/AKS/local):
// maybeEnableWorkspaceStorage must skip it via the type assertion, not panic.
type plainWS struct{}

func (plainWS) Resolve(context.Context, cloud.WorkspaceParams) (cloud.Decision, error) {
	return cloud.Decision{}, nil
}

// TestMaybeEnableWorkspaceStorage pins the gate: the consent-gated enable step
// runs ONLY when detection would run (no explicit class, no opt-out, no cached
// marker unless rechecking), so a plain re-install — especially with -y — never
// silently enables billable storage. A WorkspaceStorage without the capability
// is skipped.
func TestMaybeEnableWorkspaceStorage(t *testing.T) {
	markerCluster := func() *fake.Clientset {
		kc := fake.NewSimpleClientset()
		_, _ = kc.CoreV1().Namespaces().Create(context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agentprimitives-system"}}, metav1.CreateOptions{})
		require.NoError(t, writeWorkspaceMarker(context.Background(), kc, cloud.BundledWorkspaceStorageClass))
		return kc
	}
	cases := []struct {
		name       string
		wsOpts     WorkspaceResolveOptions
		kc         *fake.Clientset
		wantCalled bool
	}{
		{name: "no override, no marker → enable runs", kc: fake.NewSimpleClientset(), wantCalled: true},
		{name: "explicit class → skip", wsOpts: WorkspaceResolveOptions{ExplicitClass: "efs-sc"}, kc: fake.NewSimpleClientset(), wantCalled: false},
		{name: "opt-out → skip", wsOpts: WorkspaceResolveOptions{NoRWX: true}, kc: fake.NewSimpleClientset(), wantCalled: false},
		{name: "cached marker → skip", kc: markerCluster(), wantCalled: false},
		{name: "cached marker but recheck → enable runs", wsOpts: WorkspaceResolveOptions{Recheck: true}, kc: markerCluster(), wantCalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEnabler{}
			var buf bytes.Buffer
			err := maybeEnableWorkspaceStorage(context.Background(), &kube.Bundle{Typed: tc.kc}, f,
				tc.wsOpts, progress.New(&buf, strings.NewReader(""), false), strings.NewReader(""), false)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCalled, f.called)
		})
	}

	t.Run("WorkspaceStorage without the capability → no-op", func(t *testing.T) {
		var buf bytes.Buffer
		err := maybeEnableWorkspaceStorage(context.Background(), &kube.Bundle{Typed: fake.NewSimpleClientset()}, plainWS{},
			WorkspaceResolveOptions{}, progress.New(&buf, strings.NewReader(""), false), strings.NewReader(""), false)
		require.NoError(t, err)
	})
}

func TestPatchOperatorWorkspaceClass_EmptyClass_RemovesFlag(t *testing.T) {
	kc := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-operator", Namespace: "agentprimitives-system"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "operator",
						Args: []string{"--workspace-storage-class=foo", "--leader-elect=true"},
					}},
				},
			},
		},
	})
	require.NoError(t, patchOperatorWorkspaceClass(context.Background(), kc, ""))
	dep, err := kc.AppsV1().Deployments("agentprimitives-system").Get(context.Background(), "spicebox-operator", metav1.GetOptions{})
	require.NoError(t, err)
	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		assert.False(t, strings.HasPrefix(a, "--workspace-storage-class="), "flag should be removed when className is empty")
	}
}
