package registry_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// conditionReasonRe is the API server's own validation pattern for
// metav1.Condition.Reason. A kind returning a reason that does not match makes
// Status().Update fail on EVERY reconcile of every session bound to the
// channel — a rejection loop, not a cosmetic problem.
var conditionReasonRe = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

// channelForKind builds the minimal Channel a membership answer is computed
// against. Deliberately not a fixture from examples/ and not a real workspace.
func channelForKind(t *testing.T, kindName string) *spiceboxv1alpha1.Channel {
	t.Helper()
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       kindName,
			AgentClass: "demo-class",
		},
	}
}

// TestEveryRegisteredKindReportsWellFormedMetaagentMembership is the guard that
// keeps the seam load-bearing.
//
// Deciding this by kind name in the reconciler would be the "if kind == x
// outside the kind's own package" AGENTS.md §Pluggability forbids, and would be
// correct only by luck of the current roster: a transport added tomorrow with
// its own bot identity would fall into the default arm and never have its
// membership tracked at all.
//
// Unlike TextFormatter, NOT every kind must implement this one — a transport
// with no bot roster genuinely has no answer, and "not applicable" is a
// legitimate, distinguishable outcome. What every kind IS held to is that when
// it DOES answer, the answer is one the API server will accept: the reconciler
// copies these three fields verbatim onto a Channel status condition.
//
// Asserting the property registry-wide — rather than naming the kinds that
// answer today — is what makes the sweep worth having.
func TestEveryRegisteredKindReportsWellFormedMetaagentMembership(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by init")

	reporters := 0
	for _, k := range kinds {
		m, ok := channelkinds.MetaagentMembershipFor(k, channelForKind(t, k.Name()))
		if !ok {
			// "Not applicable": the reconciler leaves the condition absent.
			// A kind that does not report must not smuggle a value out.
			assert.Equal(t, channelkinds.MetaagentMembership{}, m,
				"kind %q reports ok=false but returned a non-zero membership; "+
					"the reconciler discards it, so the value is a lie", k.Name())
			continue
		}
		reporters++
		t.Run(k.Name()+": membership answer is a condition the API server accepts", func(t *testing.T) {
			assert.Contains(t, []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse}, m.Status,
				"kind %q must answer True or False; Unknown/empty fails CRD validation "+
					"on every reconcile", k.Name())
			assert.Regexp(t, conditionReasonRe, m.Reason,
				"kind %q returned a reason the API server rejects; prefer the "+
					"ChannelReasonMetaagent* constants in pkg/apis/v1alpha1", k.Name())
			assert.NotEmpty(t, strings.TrimSpace(m.Message),
				"kind %q returned an empty message; it is the only place the cause "+
					"of the condition is written down for an operator", k.Name())
		})
	}

	assert.Positive(t, reporters,
		"no registered kind reports metaagent channel membership — either the seam "+
			"has been unwired or every implementation was dropped, and this sweep "+
			"is now passing vacuously")
}

// TestMetaagentMembershipForFallsBackWhenTheKindCannotAnswer pins the
// documented degrade path: the "not applicable" fallback lives in
// pkg/channels/channelkinds, never in a consumer, so no caller needs a kind name to
// decide whether the condition applies.
func TestMetaagentMembershipForFallsBackWhenTheKindCannotAnswer(t *testing.T) {
	m, ok := channelkinds.MetaagentMembershipFor(nil, channelForKind(t, "not-a-registered-kind"))
	assert.False(t, ok,
		"a nil Kind (an unregistered channel-kind name) must report not-applicable, not panic")
	assert.Equal(t, channelkinds.MetaagentMembership{}, m,
		"a not-applicable answer must carry no condition fields")
}

// operatorContainerEnvNames returns the names of the environment variables the
// SHIPPED operator container declares — from the embedded install bundle `oap
// install` actually applies, not from the kustomize sources it is built from.
//
// A `valueFrom.secretKeyRef` counts: what matters is whether the container has
// a channel through which an admin-supplied value can reach the process at all.
func operatorContainerEnvNames(t *testing.T) map[string]bool {
	t.Helper()

	objs, err := manifests.Split(manifests.Install)
	require.NoError(t, err, "split the embedded install bundle")

	for _, o := range objs {
		if o.GetKind() != "Deployment" || o.GetName() != "spicebox-operator" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		require.NoError(t, err, "read operator Deployment containers")
		require.True(t, found, "operator Deployment must declare containers")

		names := map[string]bool{}
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			require.True(t, ok, "container entry must be a mapping")
			env, _ := cm["env"].([]any)
			for _, e := range env {
				em, ok := e.(map[string]any)
				require.True(t, ok, "env entry must be a mapping")
				if n, ok := em["name"].(string); ok {
					names[n] = true
				}
			}
		}
		return names
	}
	t.Fatalf("no Deployment named spicebox-operator in the embedded install bundle")
	return nil
}

// TestMetaagentInvitedIsReachableInTheShippedOperatorDeployment is the test the
// audit finding was really about.
//
// The operator is the only writer of Channel.status's
// MetaagentChannelMembership condition, and the answer it copies there depends
// on the metaagent bot user-id reaching the operator PROCESS. It never did:
// METAAGENT_SLACK_BOT_USER_ID was wired into the authzd and channelsd
// Deployments only, so in every shipped install the operator saw "" and this
// condition could stamp nothing but False/MetaagentAppNotInstalled. The
// True/Invited arm was reachable exclusively from an integration test that
// substituted the value through an export shim — a test pinning a state
// production could not produce.
//
// This asserts the CONDITION is reachable, not that a string appears in YAML:
// it gives the process exactly the variables the shipped Deployment declares,
// then asks the kind the same way the reconciler does, through the registry.
func TestMetaagentInvitedIsReachableInTheShippedOperatorDeployment(t *testing.T) {
	const botUserID = "UMETAAGENTBOT"

	// Simulate the operator process: only variables the shipped Deployment
	// declares can carry a value into it. Blanking it otherwise keeps the test
	// hermetic — a developer whose own shell exports the variable must not see
	// this pass while the cluster's operator would not.
	value := ""
	if operatorContainerEnvNames(t)[clikit.EnvMetaagentSlackBotUserID] {
		value = botUserID
	}
	t.Setenv(clikit.EnvMetaagentSlackBotUserID, value)

	k, ok := registry.Get("slack")
	require.True(t, ok, "slack kind must be registered")

	m, ok := channelkinds.MetaagentMembershipFor(k, channelForKind(t, "slack"))
	require.True(t, ok, "slack must report metaagent channel membership")
	assert.Equal(t, spiceboxv1alpha1.ChannelReasonMetaagentInvited, m.Reason,
		"the shipped operator Deployment must give the operator process the metaagent "+
			"bot user-id (%s, from the agentprimitives-system-metaagent-config Secret); "+
			"without it MetaagentChannelMembership can only ever stamp False/%s and the "+
			"True arm is dead in every install",
		clikit.EnvMetaagentSlackBotUserID, spiceboxv1alpha1.ChannelReasonMetaagentAppNotInstalled)
	assert.Equal(t, metav1.ConditionTrue, m.Status,
		"a configured metaagent bot must produce MetaagentChannelMembership=True")
}
