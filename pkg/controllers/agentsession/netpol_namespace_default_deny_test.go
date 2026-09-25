package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// The whole allowlist model rests on a default-deny, and the one that exists is
// scoped to the platform namespace alone. Sessions do not live there — each is
// created in its Channel's namespace, and the built-in webchat uses `default`.
//
// NetworkPolicy is an allow-UNION with no implicit default, so a pod that
// matches no policy's selector is unrestricted in BOTH directions, even on a
// fully enforcing CNI. The four per-session builders all select on session
// labels, so they constrain the runner, sandbox and sidecars and nothing else:
// a pod created in that namespace by anything else — including a Job an agent
// was able to create — rides no rule at all.
//
// A namespace-wide default-deny is the only thing that covers a pod whose
// labels the creator chooses. It is also the one policy AP cannot stamp
// unilaterally: it selects EVERY pod in a namespace AP does not own, so
// enabling it in a shared namespace is the cluster operator's decision, not a
// default. Hence opt-in — but buildable, testable, and reaped like the rest,
// rather than absent and undocumented.

func namespaceDenyFixtureSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "sess-1", UID: "uid-1"},
	}
}

func TestBuildNamespaceDefaultDenyNetworkPolicy_SelectsEveryPodAndDeniesBothDirections(t *testing.T) {
	np := BuildNamespaceDefaultDenyNetworkPolicy(namespaceDenyFixtureSession())
	require.NotNil(t, np)

	assert.Equal(t, "tenant-a", np.Namespace, "it must land in the session's own namespace")
	assert.Empty(t, np.Spec.PodSelector.MatchLabels,
		"an empty podSelector is what makes this cover a pod whose labels the creator chose")
	assert.Empty(t, np.Spec.PodSelector.MatchExpressions)
	assert.ElementsMatch(t,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		np.Spec.PolicyTypes,
		"both directions: an unconstrained pod also ACCEPTS connections from anywhere in the cluster")
	assert.Empty(t, np.Spec.Ingress, "no rules is what makes it a deny")
	assert.Empty(t, np.Spec.Egress)
}

// It must NOT be owned by the session that happened to trigger it. A namespace
// holds many sessions, and an ownerRef would have the first one's deletion
// garbage-collect the floor out from under every other session still running
// there — turning a security control into an intermittent one.
func TestBuildNamespaceDefaultDenyNetworkPolicy_IsNotOwnedByOneSession(t *testing.T) {
	np := BuildNamespaceDefaultDenyNetworkPolicy(namespaceDenyFixtureSession())

	assert.Empty(t, np.OwnerReferences,
		"a namespace-scoped floor outlives any one session in that namespace")
	assert.NotContains(t, np.Name, "sess-1",
		"the name is per-namespace, so every session in it converges on the same object")
}

// Disabled by default, and disabled independently of the per-session policies:
// an operator who wants runner/sandbox/sidecar containment (default on) has not
// thereby asked AP to blanket-deny a namespace full of their own workloads.
func TestEnsureNamespaceDefaultDeny_OffUnlessAsked(t *testing.T) {
	assert.False(t, NetpolConfig{Enabled: true}.NamespaceDefaultDeny,
		"per-session policies being on must not imply a namespace-wide deny")
}

// The floor's empty podSelector covers EVERY pod in the namespace, this
// project's own workspace Jobs included — and with no rule naming them the
// reconcile Job loses DNS and its git remote outright.
//
// Shipping a security control that silently breaks the platform's own workloads
// the moment it is switched on is worse than not shipping it: the operator
// turns it off again and learns to distrust it. So the allow rule ships with
// the floor, selecting the label those pod templates carry.
func TestBuildWorkspaceJobNetworkPolicy_SelectsTheJobsTheFloorWouldSever(t *testing.T) {
	np := BuildWorkspaceJobNetworkPolicy(namespaceDenyFixtureSession())
	require.NotNil(t, np)

	assert.Equal(t, "tenant-a", np.Namespace)
	require.Len(t, np.Spec.PodSelector.MatchExpressions, 1,
		"selected by the marker their pod templates carry, whatever its value")
	assert.Equal(t, workspace.LabelWorkspaceJob, np.Spec.PodSelector.MatchExpressions[0].Key)
	assert.Equal(t, metav1.LabelSelectorOpExists, np.Spec.PodSelector.MatchExpressions[0].Operator)

	assert.Empty(t, np.Spec.Ingress, "nothing calls these pods, so nothing may")
	require.NotEmpty(t, np.Spec.Egress, "the reconcile Job runs git against a remote")

	var ports []int32
	for _, rule := range np.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil {
				ports = append(ports, int32(p.Port.IntValue()))
			}
		}
	}
	assert.Contains(t, ports, int32(443), "git over HTTPS")
	assert.Contains(t, ports, int32(22), "git over SSH")
	assert.Contains(t, ports, int32(53), "DNS, or the remote cannot be resolved at all")
}

// Per-namespace and unowned, for the same reason the floor is: these Jobs are
// not per-session objects, and a policy garbage-collected with whichever
// session happened to stamp it would come and go under the others.
func TestBuildWorkspaceJobNetworkPolicy_IsNamespaceScopedAndUnowned(t *testing.T) {
	np := BuildWorkspaceJobNetworkPolicy(namespaceDenyFixtureSession())

	assert.Empty(t, np.OwnerReferences)
	assert.NotContains(t, np.Name, "sess-1")
}
