//go:build integration

package spiceboxtoolchain_test

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
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/spiceboxtoolchain"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	_ "github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/image"
)

func startManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:     env.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t, (&spiceboxtoolchain.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr),
		"spiceboxtoolchain SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
}

func eventually(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func goTC(name, kind, prefix string) *spiceboxv1alpha1.SpiceboxToolchain {
	return &spiceboxv1alpha1.SpiceboxToolchain{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolchainSpec{
			Source:    spiceboxv1alpha1.ToolchainSource{Kind: kind, Image: "ap-toolchain-go:dev", Prefix: prefix},
			Bin:       []string{"bin"},
			SizeBytes: 1,
		},
	}
}

func awaitCondition(t *testing.T, env *testenv.Env, name string) *metav1.Condition {
	t.Helper()
	var got spiceboxv1alpha1.SpiceboxToolchain
	eventually(t, 5*time.Second, func() bool {
		if err := env.Client.Get(context.Background(), client.ObjectKey{Name: name}, &got); err != nil {
			return false
		}
		return conditions.Find(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolchainConditionValid) != nil
	})
	return conditions.Find(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolchainConditionValid)
}

func TestToolchain_WellFormed_Valid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	require.NoError(t, env.Client.Create(context.Background(), goTC("go", "image", "/opt/ap-toolchains/go")))

	c := awaitCondition(t, env, "go")
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status, "a well-formed toolchain is Valid=True")
	require.Equal(t, spiceboxv1alpha1.ReasonToolchainValid, c.Reason)
}

func TestToolchain_UnknownSourceKind_InvalidWithKnownKindsListed(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	require.NoError(t, env.Client.Create(context.Background(), goTC("wormhole-tc", "wormhole", "/opt/ap-toolchains/wormhole-tc")))

	c := awaitCondition(t, env, "wormhole-tc")
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, spiceboxv1alpha1.ReasonToolchainUnknownKind, c.Reason)
	require.Contains(t, c.Message, "image", "the message names the registered kinds so an operator can fix it")
}

func TestToolchain_PrefixNotMatchingName_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	require.NoError(t, env.Client.Create(context.Background(), goTC("go2", "image", "/opt/ap-toolchains/go")))

	c := awaitCondition(t, env, "go2")
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, spiceboxv1alpha1.ReasonToolchainInvalidSpec, c.Reason)
}
