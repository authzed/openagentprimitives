package toolspec_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/toolspec"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// resolveTarget installs the given SpiceboxToolspec into a fake client
// and resolves the target. Returns the kind + target for assertions.
func resolveTarget(t *testing.T, ts *spiceboxv1alpha1.SpiceboxToolspec) (authkind.Kind, authkind.Target) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ts).Build()
	k := toolspec.New()
	tgt, err := k.ResolveTarget(context.Background(), c, "default", ts.Name)
	require.NoError(t, err, "ResolveTarget(%s)", ts.Name)
	return k, tgt
}

func TestPrefix(t *testing.T) {
	assert.Equal(t, "toolspec", toolspec.New().Prefix(), "Prefix")
}

func TestResolveAndIntent(t *testing.T) {
	// SpiceboxToolspec is cluster-scoped; no namespace in ObjectMeta.
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-readonly"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Intent:  "Read GitHub PRs",
			Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "gh"},
		},
	}
	_, tgt := resolveTarget(t, ts)
	assert.Equal(t, "gh-readonly", tgt.Name(), "Name")
	assert.Equal(t, "toolspec:gh-readonly", tgt.BindingMatchString(), "BindingMatchString")
	assert.Equal(t, "Read GitHub PRs", tgt.Intent(), "Intent")
}

func TestSetupRequirementsForToolspecPointingAtGH(t *testing.T) {
	// Build a toolspec referencing the gh toolkit. AllowSubcommands is empty
	// so the safe-default path fires: returns the underlying toolkit's full
	// sensitive-env set (GITHUB_TOKEN for gh).
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-anything"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "gh"},
			// AllowSubcommands intentionally empty → safe-default path.
		},
	}
	k, tgt := resolveTarget(t, ts)
	reqs := k.SetupRequirements(context.Background(), tgt)
	require.Len(t, reqs, 1, "exactly one requirement (GITHUB_TOKEN)")
	assert.Equal(t, "github-token", reqs[0].SuggestedName, "SuggestedName is the declared credential name")
	assert.Contains(t, reqs[0].BindingEnv, "GITHUB_TOKEN", "BindingEnv includes GITHUB_TOKEN")
	// toolspec delegates to cli, so it carries the explicit env Inject too.
	assert.Equal(t, "GITHUB_TOKEN", reqs[0].Inject.EnvVar, "Inject.EnvVar carried from cli delegation")
}
