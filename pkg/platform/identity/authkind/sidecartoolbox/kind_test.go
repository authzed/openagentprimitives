package sidecartoolbox_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/sidecartoolbox"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := spiceboxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("spicebox.AddToScheme: %v", err)
	}
	return s
}

func TestKind_Prefix(t *testing.T) {
	if got, want := sidecartoolbox.New().Prefix(), "toolbox"; got != want {
		t.Errorf("Prefix=%q want %q", got, want)
	}
}

func TestKind_ResolveTarget_Found(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "reddit-readonly", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "reddit-readonly",
			Intent:       "read-only reddit",
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "reddit-app"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(cr).Build()
	tgt, err := sidecartoolbox.New().ResolveTarget(context.Background(), c, "default", "reddit-readonly")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if tgt.Name() != "reddit-readonly" {
		t.Errorf("Name=%q", tgt.Name())
	}
	if got, want := tgt.BindingMatchString(), "toolbox:reddit-readonly"; got != want {
		t.Errorf("BindingMatchString=%q want %q", got, want)
	}
	if tgt.Intent() != "read-only reddit" {
		t.Errorf("Intent=%q", tgt.Intent())
	}
}

func TestKind_ResolveTarget_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := sidecartoolbox.New().ResolveTarget(context.Background(), c, "default", "missing")
	if !errors.Is(err, authkind.ErrTargetNotFound) {
		t.Fatalf("got %v, want ErrTargetNotFound", err)
	}
}

func TestKind_SetupRequirements_UsesProvider(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "reddit-readonly", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "reddit-readonly",
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "reddit-app", EnvVar: "REDDIT_TOKEN"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(cr).Build()
	tgt, err := sidecartoolbox.New().ResolveTarget(context.Background(), c, "default", "reddit-readonly")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	reqs := sidecartoolbox.New().SetupRequirements(context.Background(), tgt)
	if len(reqs) != 1 {
		t.Fatalf("len(reqs)=%d want 1", len(reqs))
	}
	if reqs[0].SuggestedName != "reddit-readonly-creds" {
		t.Errorf("SuggestedName=%q want reddit-readonly-creds", reqs[0].SuggestedName)
	}
	if reqs[0].ProviderID != "reddit-app" {
		t.Errorf("ProviderID=%q", reqs[0].ProviderID)
	}
	if reqs[0].IsBearer {
		t.Errorf("IsBearer must be false (env-projected, not bearer)")
	}
	// Inject carries the upstream-auth env var name. For the sidecar path this
	// is informational: credentials are materialized controller-side into a
	// per-session Secret the sidecar envFrom's, not via the runtime resolver.
	if reqs[0].Inject.EnvVar != "REDDIT_TOKEN" {
		t.Errorf("Inject.EnvVar=%q want REDDIT_TOKEN", reqs[0].Inject.EnvVar)
	}
	if reqs[0].Inject.Header != nil {
		t.Errorf("Inject.Header must be nil for env-projected sidecar requirement")
	}
}

// TestKind_SetupRequirements_NoneProviderReturnsNoRequirements pins plan-3b
// Ruling B's CLI-setup-side half: a controller-issued-token sidecar
// (upstreamAuth.provider: "none") has no AgentIdentity credential to walk, so
// `oap agent setup-identity` must see zero requirements for it — not a
// requirement carrying ProviderID "none" into provider.ByID, which would
// hard-error the whole setup run with "unknown provider".
func TestKind_SetupRequirements_NoneProviderReturnsNoRequirements(t *testing.T) {
	cr := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "workshop", Namespace: "default"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:         "workshop",
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "none"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(cr).Build()
	tgt, err := sidecartoolbox.New().ResolveTarget(context.Background(), c, "default", "workshop")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	reqs := sidecartoolbox.New().SetupRequirements(context.Background(), tgt)
	if len(reqs) != 0 {
		t.Fatalf("len(reqs)=%d want 0, got %+v", len(reqs), reqs)
	}
}
