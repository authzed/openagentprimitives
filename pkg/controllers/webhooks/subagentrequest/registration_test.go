package subagentrequest

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

// webhookName is the entry this package's handler is served under in the
// shipped ValidatingWebhookConfiguration.
const webhookName = "subagentrequestparent.agentprimitives.authzed.com"

// loadShippedWebhook decodes pkg/platform/manifests.Install — the embedded bundle
// `oap install` actually applies — and returns the subagentrequestparent
// ValidatingWebhook entry from it.
//
// This reads the SHIPPED artifact, not config/manager/webhook.yaml off disk.
// pkg/platform/manifests.TestInstallYAMLMatchesKustomize already proves those two stay
// byte-for-byte in sync with EACH OTHER, but that comparison structurally
// cannot catch config/ and install.yaml drifting TOGETHER — agreeing with
// each other while both disagree with the Go source (Path, runnerSASuffix).
// Asserting against the artifact that installs is the only form of this test
// that would catch that.
func loadShippedWebhook(t *testing.T) *admissionregistrationv1.ValidatingWebhook {
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
			if vwc.Webhooks[i].Name == webhookName {
				hook = &vwc.Webhooks[i]
			}
		}
	}
	require.NotNil(t, hook, "install.yaml must ship the %s webhook", webhookName)
	return hook
}

// TestShippedWebhookConfigEnforcesTheGate pins the admission wiring in
// pkg/platform/manifests/install.yaml, because nothing else does: envtest installs only
// `config/crds` (pkg/controllers/testenv/testenv.go), so no
// ValidatingWebhookConfiguration is ever registered in the integration or
// e2e suites and every property below could be broken with all three suites
// green.
//
// Each assertion is load-bearing for the delegation-trust-boundary gate:
//   - flipping failurePolicy to Ignore makes the gate decorative: an attacker
//     who crashes or saturates the webhook, or who simply waits for an
//     operator rolling update, walks through it and claims any parent in the
//     namespace — including a userPassthrough parent, which defeats
//     monotonic identity attenuation outright;
//   - dropping UPDATE from operations removes a backstop against post-creation
//     tampering with spec.parent: unlike AgentSession.spec.parent,
//     SubagentRequest carries NO CEL immutability rule on that field, so this
//     webhook cannot lean on a CRD-level rule the way that other resource
//     does. RBAC is today's actual defense (BuildRunnerRBAC grants runners
//     only create and get, never update), so UPDATE coverage here is a cheap
//     backstop against a FUTURE grant widening that Role, not proof an UPDATE
//     path is reachable today;
//   - losing matchConditions turns `Fail` from "only runner ServiceAccounts
//     pay the cost of a webhook outage" into "every principal that creates or
//     updates a SubagentRequest is blocked by one", which would needlessly
//     wedge the operator's and humans' own writes.
func TestShippedWebhookConfigEnforcesTheGate(t *testing.T) {
	hook := loadShippedWebhook(t)

	require.NotNil(t, hook.FailurePolicy, "failurePolicy must be set explicitly, not defaulted")
	assert.Equal(t, admissionregistrationv1.Fail, *hook.FailurePolicy,
		"failurePolicy: Ignore would make this gate decorative: spec.parent is the whole basis of a delegated "+
			"child's standing, and an attacker who crashes or saturates the webhook, or who simply waits for an "+
			"operator rolling update, would walk through unchecked instead of being refused")

	require.NotNil(t, hook.ClientConfig.Service, "webhook must be served by the operator Service")
	require.NotNil(t, hook.ClientConfig.Service.Path, "webhook must declare a path")
	assert.Equal(t, Path, *hook.ClientConfig.Service.Path,
		"the configured path must match this package's registered route, or the API server dials a route this handler was never registered on")

	require.Len(t, hook.Rules, 1, "one rule keeps the matched surface auditable")
	rule := hook.Rules[0]
	assert.ElementsMatch(t, []string{"agentprimitives.authzed.com"}, rule.APIGroups,
		"the rule must target the SubagentRequest CRD's API group")
	assert.ElementsMatch(t, []string{"subagentrequests"}, rule.Resources,
		"the rule must cover subagentrequests")
	assert.ElementsMatch(t,
		[]admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
		rule.Operations,
		"CREATE is when spec.parent is first set; UPDATE must be covered too because SubagentRequest.spec.parent, "+
			"unlike AgentSession.spec.parent, carries no CEL immutability rule to fall back on -- RBAC is today's "+
			"actual defense against post-creation tampering (runners are granted no update verb), so UPDATE "+
			"coverage here is a cheap backstop against a future RBAC grant widening that, not proof one is reachable today")

	require.Len(t, hook.MatchConditions, 1,
		"matchConditions are what bound the blast radius of failurePolicy: Fail to runner ServiceAccounts")
}

// TestWebhookMatchConditions_MatchARunnerAndOnlyARunner compiles the exact
// matchConditions expression shipped in pkg/platform/manifests/install.yaml and
// evaluates it the same way the API server would: against
// request.userInfo.username, before the webhook is ever dialed. This is the
// live failure mode — a typo in the CEL, or a future change to how the
// per-session runner ServiceAccount is named, silently stops the webhook
// from ever being called, and a gate that is never dialed denies nothing
// while looking completely healthy in every manifest diff.
func TestWebhookMatchConditions_MatchARunnerAndOnlyARunner(t *testing.T) {
	hook := loadShippedWebhook(t)
	require.Len(t, hook.MatchConditions, 1, "exactly one matchConditions entry is expected")
	expr := hook.MatchConditions[0].Expression

	env, err := cel.NewEnv(
		cel.Variable("request", cel.MapType(cel.StringType, types.DynType)),
	)
	require.NoError(t, err, "build a CEL environment for the matchConditions expression")

	ast, iss := env.Compile(expr)
	require.True(t, iss == nil || iss.Err() == nil, "compile matchConditions expression %q: %v", expr, iss)

	prog, err := env.Program(ast)
	require.NoError(t, err, "build a CEL program for %q", expr)

	cases := []struct {
		name     string
		username string
		matches  bool
	}{
		{
			// The load-bearing row: the exact shape BuildRunnerRBAC mints,
			// derived from runnerSASuffix rather than retyped, so a future
			// rename of that constant fails THIS test instead of silently
			// unhooking the webhook from every principal it is meant to gate.
			name:     "a real per-session runner ServiceAccount: matches",
			username: "system:serviceaccount:default:lead-1" + runnerSASuffix,
			matches:  true,
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
		{
			name:     "a ServiceAccount that is not a runner: does not match",
			username: "system:serviceaccount:default:some-other-sa",
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
}
