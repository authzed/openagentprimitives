package workshop

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// namespacedWebhookName and clusterWebhookName are the two entries this
// package's single handler is served under in the shipped
// ValidatingWebhookConfiguration — one scoped to the seven namespaced
// workshop-authored kinds, one to the two cluster-scoped tool kinds. Both
// point at the same PathWorkshopObject route.
const (
	namespacedWebhookName = "workshopobject.agentprimitives.authzed.com"
	clusterWebhookName    = "workshoptool.agentprimitives.authzed.com"
)

// loadShippedWebhook decodes pkg/platform/manifests.Install — the embedded
// bundle `oap install` actually applies — and returns the named
// ValidatingWebhook entry from the spicebox-settings VWC.
//
// This reads the SHIPPED artifact, not config/manager/webhook.yaml off disk.
// pkg/platform/manifests.TestInstallYAMLMatchesKustomize already proves those
// two stay byte-for-byte in sync with EACH OTHER, but that comparison
// structurally cannot catch config/ and install.yaml drifting TOGETHER —
// agreeing with each other while both disagree with the Go source
// (PathWorkshopObject, workshopSASuffix). Asserting against the artifact
// that installs is the only form of this test that would catch that.
func loadShippedWebhook(t *testing.T, name string) *admissionregistrationv1.ValidatingWebhook {
	t.Helper()

	docs, err := manifests.Split(manifests.Install)
	require.NoError(t, err, "split the embedded install bundle")

	var hook *admissionregistrationv1.ValidatingWebhook
	for _, d := range docs {
		if d.GetKind() != "ValidatingWebhookConfiguration" {
			continue
		}
		var vwc admissionregistrationv1.ValidatingWebhookConfiguration
		require.NoError(t, runtime.DefaultUnstructuredConverter.
			FromUnstructured(d.Object, &vwc), "decode %s", d.GetName())
		for i := range vwc.Webhooks {
			if vwc.Webhooks[i].Name == name {
				hook = &vwc.Webhooks[i]
			}
		}
	}
	require.NotNil(t, hook, "install.yaml must ship the %s webhook", name)
	return hook
}

// TestShippedNamespacedWebhookEnforcesTheGate pins the admission wiring for
// the seven-namespaced-kind entry in pkg/platform/manifests/install.yaml,
// because nothing else does: envtest installs only config/crds
// (pkg/controllers/testenv/testenv.go), so no ValidatingWebhookConfiguration
// is ever registered in the integration or e2e suites and every property
// below could be broken with all three suites green.
//
// Each assertion is load-bearing for the workshop lockdown:
//   - flipping failurePolicy to Ignore makes every content rule and limit
//     decorative: an attacker who crashes or saturates the webhook, or who
//     simply waits for an operator rolling update, walks through the ONE
//     gate standing between a workshop sidecar and the seven kinds
//     workshop-agent's Role hands it create/update/patch/delete on;
//   - losing matchConditions turns `Fail` from "only workshop sidecar
//     ServiceAccounts pay the cost of a webhook outage" into "every
//     principal that writes one of these seven kinds anywhere in the
//     cluster is blocked by one";
//   - losing the namespaceSelector removes the second layer of that same
//     blast-radius bound: a Fail-policy webhook with no namespace scoping
//     would be dialed for these seven kinds in EVERY namespace, not just
//     workshop namespaces.
func TestShippedNamespacedWebhookEnforcesTheGate(t *testing.T) {
	hook := loadShippedWebhook(t, namespacedWebhookName)

	require.NotNil(t, hook.FailurePolicy, "failurePolicy must be set explicitly, not defaulted")
	assert.Equal(t, admissionregistrationv1.Fail, *hook.FailurePolicy,
		"failurePolicy: Ignore would make every content rule and limit in this webhook decorative")

	require.NotNil(t, hook.ClientConfig.Service, "webhook must be served by the operator Service")
	require.NotNil(t, hook.ClientConfig.Service.Path, "webhook must declare a path")
	assert.Equal(t, PathWorkshopObject, *hook.ClientConfig.Service.Path,
		"the configured path must match this package's registered route, or the API server dials a route this handler was never registered on")

	require.Len(t, hook.Rules, 1, "one rule keeps the matched surface auditable")
	rule := hook.Rules[0]
	assert.ElementsMatch(t, []string{"agentprimitives.authzed.com"}, rule.APIGroups)
	assert.ElementsMatch(t,
		[]string{"agentclasses", "mcpservers", "sidecartoolboxes", "agentidentities", "skills", "agentuis", "subagentrequests"},
		rule.Resources,
		"must cover exactly the seven namespaced kinds pkg/controllers/workshop/rbac.go's workshop-agent Role grants")
	assert.ElementsMatch(t,
		[]admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
		rule.Operations)
	require.NotNil(t, rule.Scope, "rule scope must be Namespaced")
	assert.Equal(t, admissionregistrationv1.NamespacedScope, *rule.Scope)

	require.Len(t, hook.MatchConditions, 1,
		"matchConditions are what bound the blast radius of failurePolicy: Fail to workshop sidecar ServiceAccounts")

	require.NotNil(t, hook.NamespaceSelector, "namespaceSelector must scope this Fail-policy webhook to workshop namespaces only")
	require.Len(t, hook.NamespaceSelector.MatchExpressions, 1)
	assert.Equal(t, "agentprimitives.authzed.com/workshop-session-name", hook.NamespaceSelector.MatchExpressions[0].Key,
		"must match on LabelWorkshopSessionName, the label pkg/controllers/workshop.BuildWorkshopNamespace stamps on every workshop namespace")
}

// TestShippedClusterWebhookEnforcesTheGate is the same set of properties for
// the cluster-scoped toolspec/toolkit entry.
func TestShippedClusterWebhookEnforcesTheGate(t *testing.T) {
	hook := loadShippedWebhook(t, clusterWebhookName)

	require.NotNil(t, hook.FailurePolicy)
	assert.Equal(t, admissionregistrationv1.Fail, *hook.FailurePolicy,
		"failurePolicy: Ignore would let a workshop sidecar create a SpiceboxToolspec/SpiceboxToolkit named for or "+
			"labeled as ANY workshop — the shared spicebox-workshop-toolwriter ClusterRole cannot scope create by name")

	require.NotNil(t, hook.ClientConfig.Service)
	require.NotNil(t, hook.ClientConfig.Service.Path)
	assert.Equal(t, PathWorkshopObject, *hook.ClientConfig.Service.Path)

	require.Len(t, hook.Rules, 1)
	rule := hook.Rules[0]
	assert.ElementsMatch(t, []string{"agentprimitives.authzed.com"}, rule.APIGroups)
	assert.ElementsMatch(t, []string{"spiceboxtoolspecs", "spiceboxtoolkits"}, rule.Resources)
	assert.ElementsMatch(t,
		[]admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
		rule.Operations)
	require.NotNil(t, rule.Scope)
	assert.Equal(t, admissionregistrationv1.ClusterScope, *rule.Scope)

	require.Len(t, hook.MatchConditions, 1)
	// A cluster-scoped object has no namespace, so this entry must NOT carry
	// a namespaceSelector — one would either be ignored or, worse, silently
	// misconfigured as "never matches", turning failurePolicy: Fail into an
	// unconditional refusal of every SpiceboxToolspec/SpiceboxToolkit write.
	assert.Nil(t, hook.NamespaceSelector, "a cluster-scoped rule cannot be scoped by namespaceSelector")
}

// TestWorkshopMatchConditions_MatchAWorkshopSAAndOnlyAWorkshopSA compiles the
// exact matchConditions expression shipped in
// pkg/platform/manifests/install.yaml and evaluates it the same way the API
// server would: against request.userInfo.username, before the webhook is
// ever dialed. This is the live failure mode — a typo in the CEL, or a
// future change to how the per-workshop sidecar ServiceAccount is named,
// silently stops the webhook from ever being called, and a gate that is
// never dialed denies nothing while looking completely healthy in every
// manifest diff.
func TestWorkshopMatchConditions_MatchAWorkshopSAAndOnlyAWorkshopSA(t *testing.T) {
	for _, name := range []string{namespacedWebhookName, clusterWebhookName} {
		t.Run(name, func(t *testing.T) {
			hook := loadShippedWebhook(t, name)
			require.Len(t, hook.MatchConditions, 1)
			expr := hook.MatchConditions[0].Expression

			env, err := cel.NewEnv(cel.Variable("request", cel.MapType(cel.StringType, types.DynType)))
			require.NoError(t, err)

			ast, iss := env.Compile(expr)
			require.True(t, iss == nil || iss.Err() == nil, "compile matchConditions expression %q: %v", expr, iss)

			prog, err := env.Program(ast)
			require.NoError(t, err)

			cases := []struct {
				name     string
				username string
				matches  bool
			}{
				{
					// The load-bearing row: the exact shape
					// WorkshopServiceAccountName mints, derived from
					// workshopSASuffix rather than retyped, so a future rename
					// of that constant fails THIS test instead of silently
					// unhooking the webhook from every principal it is meant
					// to gate.
					name:     "a real per-session workshop sidecar ServiceAccount: matches",
					username: "system:serviceaccount:default:builder-s1" + workshopSASuffix,
					matches:  true,
				},
				{
					name:     "a runner ServiceAccount: does not match",
					username: "system:serviceaccount:default:builder-s1-runner-sa",
					matches:  false,
				},
				{
					name:     "the operator's own ServiceAccount: does not match",
					username: "system:serviceaccount:agentprimitives-system:spicebox-operator",
					matches:  false,
				},
				{
					name:     "a human administrator: does not match",
					username: "kubernetes-admin",
					matches:  false,
				},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					out, _, err := prog.Eval(map[string]any{
						"request": map[string]any{
							"userInfo": map[string]any{"username": tc.username},
						},
					})
					require.NoError(t, err, "evaluate matchConditions expression for username %q", tc.username)
					got, ok := out.Value().(bool)
					require.True(t, ok, "matchConditions expression must evaluate to bool, got %T", out.Value())
					assert.Equal(t, tc.matches, got, "username %q", tc.username)
				})
			}
		})
	}
}
