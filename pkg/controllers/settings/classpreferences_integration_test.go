//go:build integration

// Integration test for the AgentSettings NamespaceReconciler's cross-check
// between classUserPreferences globals (AgentSettings.spec.classUserPreferences)
// and each named class's installed userPreferences schema
// (AgentClass.spec.userPreferences). Drives a real manager + watches (not a
// direct Reconcile call), because the interesting behavior IS the watch: an
// AgentClass edit must re-reconcile the namespace's AgentSettings singleton
// with nothing touching AgentSettings itself. Mirrors the pattern in
// pkg/controllers/agentclass/watches_integration_test.go.
package settings

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// startSettingsManager wires the AgentSettings NamespaceReconciler to a real
// manager against the package-shared envtest apiserver and blocks until the
// cache has synced. Mirrors agentclass's startManager
// (pkg/controllers/agentclass/watches_integration_test.go).
func startSettingsManager(t *testing.T) client.Client {
	t.Helper()
	env := testenv.Shared(t)
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t, (&NamespaceReconciler{}).SetupWithManager(mgr), "settings.NamespaceReconciler.SetupWithManager")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
	return mgr.GetClient()
}

// nsCounter disambiguates namespace names created within one test process;
// each subtest needs its OWN namespace because AgentSettings is a
// namespace-scoped singleton (fixed name "default"), and testenv.Reset
// never deletes namespaces between tests.
var nsCounter int64

// createNamespace creates a uniquely-named Namespace and returns its name.
func createNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	nsCounter++
	name := fmt.Sprintf("settings-cpc-%d-%d", time.Now().UnixNano(), nsCounter)
	require.NoError(t, c.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}), "create namespace %s", name)
	return name
}

// minimalClass returns an unsaved AgentClass in namespace ns with the same
// minimal spec shape as agentclass_test's newClass helper — enough to satisfy
// the CRD schema without needing the referenced Secret to actually exist
// (nothing here drives the AgentClass to Valid=True).
func minimalClass(t *testing.T, ns, name string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-opus-4-7",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
		},
	}
}

// js wraps a raw JSON literal as apiextv1.JSON, the shape
// PreferenceGlobal.Value is stored in.
func js(raw string) apiextv1.JSON {
	return apiextv1.JSON{Raw: []byte(raw)}
}

// eventually polls fn until it returns true or the deadline elapses.
func eventually(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("eventually: condition not met within %v", d)
}

// TestAgentSettings_ClassPreferencesCrossCheck is the load-bearing scenario:
// a global whose value is outside its class's declared enum must land the
// settings singleton at ClassPreferencesValid=False/ClassPreferencesInvalid,
// and widening the class's enum — an AgentClass edit, nothing touching
// AgentSettings — must re-reconcile the singleton back to True through the
// dependency watch. Fails (times out) both if the cross-check is never wired
// and if SetupWithManager never registers the AgentClass watch.
func TestAgentSettings_ClassPreferencesCrossCheck(t *testing.T) {
	c := startSettingsManager(t)
	ctx := context.Background()
	ns := createNamespace(t, c)

	// Class declaring one enum preference.
	ac := minimalClass(t, ns, "reviewbot")
	ac.Spec.UserPreferences = []spiceboxv1alpha1.UserPreferenceSchema{{
		Name: "language", Type: "enum",
		Enum: []spiceboxv1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}},
	}}
	require.NoError(t, c.Create(ctx, ac), "create AgentClass reviewbot")

	// Global with a value outside the enum -> condition False.
	as := &spiceboxv1alpha1.AgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.AgentSettingsName, Namespace: ns},
		Spec: spiceboxv1alpha1.SettingsSpec{ClassUserPreferences: map[string]map[string]spiceboxv1alpha1.PreferenceGlobal{
			"reviewbot": {"language": {Value: js(`"xx"`)}},
		}},
	}
	require.NoError(t, c.Create(ctx, as), "create AgentSettings")

	eventually(t, 30*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentSettings
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: spiceboxv1alpha1.AgentSettingsName}, &got); err != nil {
			return false
		}
		cond := conditions.Find(got.Status.Conditions, spiceboxv1alpha1.SettingsConditionClassPreferencesValid)
		return cond != nil && cond.Status == metav1.ConditionFalse &&
			cond.Reason == spiceboxv1alpha1.ReasonClassPreferencesInvalid
	})

	// Fix the class's schema to admit the value (dependency-watch direction:
	// an AgentClass change must re-reconcile the settings singleton).
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ac), ac), "refresh AgentClass before widening enum")
	ac.Spec.UserPreferences[0].Enum = append(ac.Spec.UserPreferences[0].Enum,
		spiceboxv1alpha1.PreferenceEnumValue{Value: "xx"})
	require.NoError(t, c.Update(ctx, ac), "widen reviewbot's language enum")

	eventually(t, 30*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentSettings
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: spiceboxv1alpha1.AgentSettingsName}, &got); err != nil {
			return false
		}
		return conditions.IsTrue(got.Status.Conditions, spiceboxv1alpha1.SettingsConditionClassPreferencesValid)
	})
}

// TestAgentSettings_ClassPreferences_UnknownClassTolerated asserts
// install-order tolerance: a global naming a class that does not exist YET
// must not fail the cross-check. Without this, an ordinary `kubectl apply -f
// dir/` that applies AgentSettings before its AgentClass would wedge the
// singleton at ClassPreferencesInvalid until the class showed up and
// something else happened to nudge a reconcile.
func TestAgentSettings_ClassPreferences_UnknownClassTolerated(t *testing.T) {
	c := startSettingsManager(t)
	ctx := context.Background()
	ns := createNamespace(t, c)

	as := &spiceboxv1alpha1.AgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.AgentSettingsName, Namespace: ns},
		Spec: spiceboxv1alpha1.SettingsSpec{ClassUserPreferences: map[string]map[string]spiceboxv1alpha1.PreferenceGlobal{
			"not-yet-installed": {"language": {Value: js(`"xx"`)}},
		}},
	}
	require.NoError(t, c.Create(ctx, as), "create AgentSettings naming a not-yet-installed class")

	eventually(t, 30*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentSettings
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: spiceboxv1alpha1.AgentSettingsName}, &got); err != nil {
			return false
		}
		return conditions.IsTrue(got.Status.Conditions, spiceboxv1alpha1.SettingsConditionClassPreferencesValid)
	})
}
