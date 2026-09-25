//go:build integration

package spiceboxtoolkit_test

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
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolkit"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

func TestToolkit_NoCollision_Valid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-tool-v1"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "custom-tool", Version: "1", ToolkitRevision: "v1",
			Target:      spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/custom"},
			Parser:      spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env:         spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{}},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{minimalSubcommand()},
		},
	}
	mustCreate(t, env.Client, tk)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolkit
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tk), &got); err != nil {
			return false
		}
		return hasTrueCondition(&got, spiceboxv1alpha1.SpiceboxToolkitConditionValid)
	})
}

func TestToolkit_BuiltinCollision_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)

	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-collision"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "echo", Version: "1", ToolkitRevision: "2026-04-25",
			Target:      spiceboxv1alpha1.ToolkitTarget{Binary: "/bin/echo"},
			Parser:      spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env:         spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{}},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{minimalSubcommand()},
		},
	}
	mustCreate(t, env.Client, tk)
	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.SpiceboxToolkit
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tk), &got); err != nil {
			return false
		}
		return hasFalseConditionWithReason(&got, spiceboxv1alpha1.SpiceboxToolkitConditionValid,
			spiceboxv1alpha1.ReasonBuiltinCollision)
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
		(&spiceboxtoolkit.Reconciler{Client: mgr.GetClient(), Registry: reg}).SetupWithManager(mgr),
		"spiceboxtoolkit SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
}

func minimalSubcommand() spiceboxv1alpha1.ToolkitSubcommand {
	return spiceboxv1alpha1.ToolkitSubcommand{
		Path: []string{},
		Effects: spiceboxv1alpha1.ToolkitEffects{
			Reads:      []string{},
			Writes:     []string{},
			Network:    spiceboxv1alpha1.ToolkitNetworkEffect{Destinations: []string{}},
			Filesystem: spiceboxv1alpha1.ToolkitFsEffect{Paths: []string{}},
			Creds:      spiceboxv1alpha1.ToolkitCredsEffect{Required: []string{}, Writes: []string{}},
		},
	}
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

func hasTrueCondition(tk *spiceboxv1alpha1.SpiceboxToolkit, condType string) bool {
	for _, c := range tk.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func hasFalseConditionWithReason(tk *spiceboxv1alpha1.SpiceboxToolkit, condType, reason string) bool {
	for _, c := range tk.Status.Conditions {
		if c.Type == condType && c.Status == metav1.ConditionFalse && c.Reason == reason {
			return true
		}
	}
	return false
}
