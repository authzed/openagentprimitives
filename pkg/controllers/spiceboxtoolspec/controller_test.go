//go:build integration

package spiceboxtoolspec_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolspec"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

func TestToolspec_BuiltinToolkit_Valid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-default"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "echo-default",
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "echo", Revision: "2026-04-25"},
			AllowSubcommands: []string{""},
		},
	}
	mustCreate(t, env.Client, ts)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolspec
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ts), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.SpiceboxToolspecConditionValid) &&
			got.Status.ResolvedToolkit == "<builtin>"
	})
}

func TestToolspec_MissingToolkit_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-spec"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "ghost", Revision: "v1"},
			AllowSubcommands: []string{""},
		},
	}
	mustCreate(t, env.Client, ts)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolspec
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ts), &got); err != nil {
			return false
		}
		return hasFalseConditionWithReason(&got, spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			spiceboxv1alpha1.ReasonToolkitMissing)
	})
}

func TestToolspec_BadCEL_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-cel"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "bad-cel",
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "echo", Revision: "2026-04-25"},
			AllowSubcommands: []string{""},
			Constraints: []spiceboxv1alpha1.ToolspecConstraint{
				{CEL: "this is not valid CEL ! @#$"},
			},
		},
	}
	mustCreate(t, env.Client, ts)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolspec
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ts), &got); err != nil {
			return false
		}
		return hasFalseConditionWithReason(&got, spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			spiceboxv1alpha1.ReasonCELCompileError)
	})
}

func TestToolspec_MissingRequiredFields_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	// Spec WITHOUT Name or Version — spec.LoadBytes will reject it.
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-fields"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "echo", Revision: "2026-04-25"},
			AllowSubcommands: []string{""},
			// Name and Version intentionally omitted
		},
	}
	mustCreate(t, env.Client, ts)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolspec
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ts), &got); err != nil {
			return false
		}
		return hasFalseConditionWithReason(&got, spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			spiceboxv1alpha1.ReasonSpecLoadFailed)
	})
}

// --- helpers ---

func startManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:     env.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err, "ctrl.NewManager")
	reg, err := registry.NewWithBuiltins(mgr.GetClient())
	require.NoError(t, err, "registry.NewWithBuiltins")
	require.NoError(t,
		(&spiceboxtoolspec.Reconciler{Client: mgr.GetClient(), Registry: reg}).SetupWithManager(mgr),
		"spiceboxtoolspec SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
}

func mustCreate(t *testing.T, cli client.Client, obj client.Object) {
	t.Helper()
	require.NoError(t, cli.Create(context.Background(), obj), "create %T", obj)
}

func eventually(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("eventually: condition not met within %v", timeout)
}

func hasTrueCondition(ts *spiceboxv1alpha1.SpiceboxToolspec, condType string) bool {
	for _, c := range ts.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func hasFalseConditionWithReason(ts *spiceboxv1alpha1.SpiceboxToolspec, condType, reason string) bool {
	for _, c := range ts.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionFalse && c.Reason == reason {
			return true
		}
	}
	return false
}
