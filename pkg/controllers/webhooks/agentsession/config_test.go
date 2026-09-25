package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// webhookName is the entry this package's handler is served under in the
// shipped ValidatingWebhookConfiguration.
const webhookName = "agentsessionidentity.agentprimitives.authzed.com"

// TestShippedWebhookConfigEnforcesTheGate pins the admission wiring in
// pkg/platform/manifests/install.yaml, because nothing else does: envtest installs only
// `config/crds` (pkg/controllers/testenv/testenv.go), so no ValidatingWebhook
// Configuration is ever registered in the integration or e2e suites and every
// property below could be broken with all three suites green.
//
// Each assertion is load-bearing for the privilege-escalation gate:
//   - dropping `agentsessions/status` from resources reopens the forged
//     pendingRestart takeover path (the ORIGINAL bug: the pre-existing
//     agentsession webhook covers only `agentsessions`, so a status patch was
//     never presented to admission at all);
//   - flipping failurePolicy to Ignore makes the gate decorative — an attacker
//     who crashes the webhook or waits for an operator roll walks through it;
//   - losing the matchConditions turns `Fail` from "runners cannot patch their
//     own session during an outage" into "no principal can write an
//     AgentSession during an outage", which would wedge every session.
func TestShippedWebhookConfigEnforcesTheGate(t *testing.T) {
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

	require.NotNil(t, hook.FailurePolicy, "failurePolicy must be set explicitly, not defaulted")
	assert.Equal(t, admissionregistrationv1.Fail, *hook.FailurePolicy,
		"a gate that refuses a forgery must fail closed")

	require.NotNil(t, hook.ClientConfig.Service, "webhook must be served by the operator Service")
	require.NotNil(t, hook.ClientConfig.Service.Path, "webhook must declare a path")
	assert.Equal(t, Path, *hook.ClientConfig.Service.Path,
		"the configured path must match this package's registered route")

	require.Len(t, hook.Rules, 1, "one rule keeps the matched surface auditable")
	rule := hook.Rules[0]
	assert.ElementsMatch(t,
		[]string{"agentsessions", "agentsessions/status"}, rule.Resources,
		"the status subresource must be covered; omitting it is the original hole")
	assert.ElementsMatch(t,
		[]admissionregistrationv1.OperationType{admissionregistrationv1.Update},
		rule.Operations, "UPDATE covers PATCH; no runner Role grants create")

	require.Len(t, hook.MatchConditions, 1,
		"matchConditions are what bound the blast radius of failurePolicy: Fail")
	assert.Contains(t, hook.MatchConditions[0].Expression, runnerSASuffix,
		"the routing predicate must key off the runner SA suffix this package matches on")
	assert.Contains(t, hook.MatchConditions[0].Expression, "request.userInfo.username",
		"the predicate must select on the requester, not the object")
}
