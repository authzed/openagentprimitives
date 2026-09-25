//go:build e2e

// Package identity_validity_chain_test is the capstone e2e scenario for the
// explicit-credential redesign. It drives the full credential-validity chain
// through the REAL controllers the harness wires — agentidentity, agentclass,
// and agentsession — without manually stamping any condition:
//
//	empty Secret  → AgentIdentity Valid=False (CredentialEmpty)
//	              → AgentClass    Valid=False (AgentIdentityInvalid)
//	              → AgentSession  ClassResolved=False (AgentClassNotValid); parked, no runner
//	populate Secret → AgentIdentity Valid=True → AgentClass Valid=True
//
// The complementary "resolved env carries the credential VALUE" leg is proven
// by scenarios/credentials (asserts GIT_TOKEN reaches the sandbox exec with the
// Secret value via the inproc broker). Together the two scenarios cover the
// redesign end to end: an empty/missing Secret invalidates and parks; a
// populated one validates and the value reaches the tool.
package identity_validity_chain_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

func TestIdentityValidityChain_EmptySecretInvalidatesAndParks_ThenPopulateRevalidates(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{string(manifests)},
		DefaultTimeout: 30 * time.Second,
	})

	ctx := context.Background()

	// The harness does not run the SpiceboxToolspec controller; stamp the
	// toolspec Valid=True so the AgentClass binding-coverage check can pass.
	// This makes credential VALIDITY (not coverage) the only thing left to
	// invalidate the class.
	stampToolspecValid(t, ctx, h.K8s, "ts-vchain-git")

	// 1) empty Secret → AgentIdentity Valid=False / CredentialEmpty.
	requireCondition(t, h.K8s,
		&spiceboxv1alpha1.AgentIdentity{}, "ai-vchain",
		spiceboxv1alpha1.AgentIdentityConditionValid,
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonCredentialEmpty,
		"empty credential Secret must drive AgentIdentity Valid=False/CredentialEmpty")

	// 2) invalid identity → AgentClass Valid=False / AgentIdentityInvalid.
	//    The credential NAME (git-token) matches the toolkit, so coverage
	//    passes; the class is invalid only because the identity is.
	requireCondition(t, h.K8s,
		&spiceboxv1alpha1.AgentClass{}, "ac-vchain",
		spiceboxv1alpha1.AgentClassConditionValid,
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentIdentityInvalid,
		"invalid AgentIdentity must propagate to AgentClass Valid=False/AgentIdentityInvalid")

	// 3) Send a user message. The channelsd pipeline materializes an
	//    AgentSession; because the class is not Valid=True, the AgentSession
	//    reconciler parks it at ClassResolved=False/AgentClassNotValid and
	//    never spawns a runner.
	h.SendUserMessage("hello")
	requireSessionParked(t, h.K8s)

	// 4) Populate the Secret → AgentIdentity re-validates to Valid=True.
	populateSecret(t, ctx, h.K8s, "vchain-git-token", "token", "ghp-vchain-real-value-9f8e7d6c")
	requireCondition(t, h.K8s,
		&spiceboxv1alpha1.AgentIdentity{}, "ai-vchain",
		spiceboxv1alpha1.AgentIdentityConditionValid,
		metav1.ConditionTrue, "",
		"populated Secret must drive AgentIdentity Valid=True")

	// 5) …which re-validates the AgentClass to Valid=True. WaitForAgentClassValid
	//    fatals if it never reaches Valid=True.
	h.WaitForAgentClassValid("ac-vchain", 30*time.Second)
}

// requireCondition polls obj (by name in the harness namespace) until its
// named condition reaches wantStatus (and wantReason, when non-empty). obj must
// be a pointer to an empty typed object whose Status carries
// []metav1.Condition at .Status.Conditions; we read it back via the typed
// client. Kept generic over the three CR types via a small type switch.
func requireCondition(
	t *testing.T, c client.Client, obj client.Object, name, condType string,
	wantStatus metav1.ConditionStatus, wantReason, msg string,
) {
	t.Helper()
	var last *metav1.Condition
	require.Eventuallyf(t, func() bool {
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: "default", Name: name}, obj); err != nil {
			return false
		}
		conds := conditionsOf(obj)
		cond := meta.FindStatusCondition(conds, condType)
		if cond == nil {
			return false
		}
		last = cond
		if cond.Status != wantStatus {
			return false
		}
		return wantReason == "" || cond.Reason == wantReason
	}, 30*time.Second, 200*time.Millisecond,
		"%s (name=%s type=%s want=%s/%s); last=%+v", msg, name, condType, wantStatus, wantReason, last)
}

// conditionsOf extracts .Status.Conditions from the three identity-chain CRs.
func conditionsOf(obj client.Object) []metav1.Condition {
	switch o := obj.(type) {
	case *spiceboxv1alpha1.AgentIdentity:
		return o.Status.Conditions
	case *spiceboxv1alpha1.AgentClass:
		return o.Status.Conditions
	case *spiceboxv1alpha1.AgentSession:
		return o.Status.Conditions
	default:
		return nil
	}
}

// requireSessionParked waits for the single AgentSession the pipeline created
// and asserts it parked at ClassResolved=False/AgentClassNotValid with no
// runner pod assigned (PodName empty).
func requireSessionParked(t *testing.T, c client.Client) {
	t.Helper()
	var parked spiceboxv1alpha1.AgentSession
	require.Eventuallyf(t, func() bool {
		var list spiceboxv1alpha1.AgentSessionList
		if err := c.List(context.Background(), &list, client.InNamespace("default")); err != nil {
			return false
		}
		if len(list.Items) != 1 {
			return false
		}
		s := list.Items[0]
		cond := meta.FindStatusCondition(s.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionClassResolved)
		if cond == nil || cond.Status != metav1.ConditionFalse ||
			cond.Reason != spiceboxv1alpha1.ReasonAgentClassNotValid {
			return false
		}
		parked = s
		return true
	}, 30*time.Second, 200*time.Millisecond,
		"AgentSession must park at ClassResolved=False/AgentClassNotValid while AgentClass is invalid")

	assert.Empty(t, parked.Status.RunnerPodName,
		"parked session must not have a runner pod assigned")
}

// stampToolspecValid sets Status.Conditions[Valid]=True on the named
// SpiceboxToolspec (the e2e harness does not run its controller). Mirrors the
// helper in scenarios/credentials.
func stampToolspecValid(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()
	var ts spiceboxv1alpha1.SpiceboxToolspec
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &ts), "get toolspec %q", name)
	ts.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             "Resolved",
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, c.Status().Update(ctx, &ts), "stamp toolspec %q Valid=True", name)
}

// populateSecret rewrites one key of an existing Secret to value. Used to flip
// the empty credential Secret to a real value mid-test.
func populateSecret(t *testing.T, ctx context.Context, c client.Client, name, key, value string) {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &sec),
		"get Secret %q", name)
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[key] = []byte(value)
	require.NoError(t, c.Update(ctx, &sec), "populate Secret %q key %q", name, key)
}
