//go:build integration

package spiceboxclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolspec"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	_ "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod" // register the built-in sandbox backend so spiceboxclass.Reconciler resolves it
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

func TestSpiceboxClassReconcile_SetsValidCondition(t *testing.T) {
	env := testenv.Shared(t)
	mgr := startManager(t, env)

	class := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "test-class"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "registry.example.com/toolbelt:latest",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("500m"),
				Memory:           resource.MustParse("256Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "echo", Command: []string{"/bin/echo"}},
			},
		},
	}
	require.NoError(t, env.Client.Create(context.Background(), class), "create SpiceboxClass")
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), class) })

	assertConditionEventually(t, env.Client, class,
		spiceboxv1alpha1.SpiceboxClassConditionValid, metav1.ConditionTrue, 5*time.Second)
	_ = mgr
}

func TestSpiceboxClassReconcile_RejectsEmptyTools(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	class := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-tools"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image:     "registry.example.com/toolbelt:latest",
			Resources: validResources(),
			Tools:     nil,
		},
	}
	require.NoError(t, env.Client.Create(context.Background(), class), "create SpiceboxClass with no tools")
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), class) })

	assertConditionEventually(t, env.Client, class,
		spiceboxv1alpha1.SpiceboxClassConditionValid, metav1.ConditionFalse, 5*time.Second)
}

func validResources() spiceboxv1alpha1.SpiceboxResources {
	return spiceboxv1alpha1.SpiceboxResources{
		CPU:              resource.MustParse("500m"),
		Memory:           resource.MustParse("256Mi"),
		EphemeralStorage: resource.MustParse("100Mi"),
	}
}

func startManager(t *testing.T, env *testenv.Env) manager.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme: env.Scheme,
		// Disable metrics to avoid port conflicts when test packages run in parallel.
		Metrics: metricsserver.Options{BindAddress: "0"},
		// SkipNameValidation is required because multiple test functions in the
		// same process each register a fresh manager with the same controller names.
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err, "ctrl.NewManager")
	reg, err := registry.NewWithBuiltins(mgr.GetClient())
	require.NoError(t, err, "registry.NewWithBuiltins")
	require.NoError(t,
		(&spiceboxclass.Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Registry: reg}).SetupWithManager(mgr),
		"spiceboxclass SetupWithManager")
	require.NoError(t,
		(&spiceboxtoolspec.Reconciler{Client: mgr.GetClient(), Registry: reg}).SetupWithManager(mgr),
		"spiceboxtoolspec SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	return mgr
}

func assertConditionEventually(
	t *testing.T, cli client.Client, obj client.Object,
	condType string, condStatus metav1.ConditionStatus, within time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err == nil {
			if class, ok := obj.(*spiceboxv1alpha1.SpiceboxClass); ok {
				for _, c := range class.Status.Conditions {
					if c.Type == condType && c.Status == condStatus {
						return
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("condition %s=%s not observed within %s", condType, condStatus, within)
}

// mustCreate creates an object and registers a cleanup to delete it.
func mustCreate(t *testing.T, cli client.Client, obj client.Object) {
	t.Helper()
	require.NoError(t, cli.Create(context.Background(), obj), "create %T", obj)
	t.Cleanup(func() { _ = cli.Delete(context.Background(), obj) })
}

// eventually polls f until it returns true or the timeout elapses.
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

// hasTrueCondition returns true if the SpiceboxClass has the given condition with Status=True.
func hasTrueCondition(cls *spiceboxv1alpha1.SpiceboxClass, condType string) bool {
	for _, c := range cls.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// hasFalseConditionWithReason returns true if the SpiceboxClass has the given condition
// with Status=False and the specified reason.
func hasFalseConditionWithReason(cls *spiceboxv1alpha1.SpiceboxClass, condType, reason string) bool {
	for _, c := range cls.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionFalse && c.Reason == reason {
			return true
		}
	}
	return false
}

// hasTrueConditionToolspec returns true if the SpiceboxToolspec has the given condition with Status=True.
func hasTrueConditionToolspec(ts *spiceboxv1alpha1.SpiceboxToolspec, condType string) bool {
	for _, c := range ts.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// createReadyToolspec creates a Toolspec and waits until its controller marks
// Valid=True. Helper used across class/session/toolcall integration tests.
func createReadyToolspec(t *testing.T, cli client.Client, name, toolkitName, toolkitRev string) {
	t.Helper()
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             name,
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: toolkitName, Revision: toolkitRev},
			AllowSubcommands: []string{""},
		},
	}
	mustCreate(t, cli, ts)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolspec
		if err := cli.Get(context.Background(), client.ObjectKeyFromObject(ts), &got); err != nil {
			return false
		}
		return hasTrueConditionToolspec(&got, spiceboxv1alpha1.SpiceboxToolspecConditionValid)
	})
}

func TestClass_FullCoverage_Valid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createReadyToolspec(t, env.Client, "echo-default", "echo", "2026-04-25")

	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-cov-1"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "python",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("128Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
				PidsLimit:        64,
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "echo", Command: []string{"/bin/echo"}},
			},
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{{Name: "echo-default"}},
		},
	}
	mustCreate(t, env.Client, cls)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxClass
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(cls), &got); err != nil {
			return false
		}
		if !hasTrueCondition(&got, spiceboxv1alpha1.SpiceboxClassConditionValid) {
			return false
		}
		covers := got.Status.ToolspecCoverage["echo"]
		return len(covers) == 1 && covers[0] == "echo-default"
	})
}

func TestClass_MissingCoverage_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	createReadyToolspec(t, env.Client, "echo-default", "echo", "2026-04-25")

	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls-cov-2"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "python",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("128Mi"),
				EphemeralStorage: resource.MustParse("100Mi"),
				PidsLimit:        64,
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{
				{Name: "echo", Command: []string{"/bin/echo"}},
				{Name: "cat", Command: []string{"/bin/cat"}},
			},
			Toolspecs: []spiceboxv1alpha1.ToolspecRef{{Name: "echo-default"}}, // no cat coverage
		},
	}
	mustCreate(t, env.Client, cls)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxClass
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(cls), &got); err != nil {
			return false
		}
		return hasFalseConditionWithReason(&got, spiceboxv1alpha1.SpiceboxClassConditionValid,
			spiceboxv1alpha1.ReasonToolspecCoverageMissing)
	})
}
