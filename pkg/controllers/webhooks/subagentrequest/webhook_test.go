package subagentrequest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newTestWebhook(t *testing.T) *Webhook {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	return New(admission.NewDecoder(scheme))
}

// req builds a CREATE AdmissionRequest for a SubagentRequest in ns claiming
// parent (namespace/name), submitted by username.
func req(t *testing.T, op admissionv1.Operation, ns, username, parentNS, parentName string) admission.Request {
	t.Helper()
	sr := &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sr1"},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: parentNS, Name: parentName},
			Class:  "demo-coder",
			Task:   "fix the failing auth middleware test",
		},
	}
	raw, err := json.Marshal(sr)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op,
		Namespace: ns,
		UserInfo:  authenticationv1.UserInfo{Username: username},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func TestHandle_ParentClaim(t *testing.T) {
	cases := []struct {
		name     string
		username string
		ns       string
		parentNS string
		parent   string
		allowed  bool
		contains string
	}{
		{
			name:     "runner claiming its own session: allowed",
			username: "system:serviceaccount:default:lead-1-runner-sa",
			ns:       "default", parentNS: "default", parent: "lead-1",
			allowed: true,
		},
		{
			// §2.10 exploit 1 and 2: a bigger pooled budget, or a roster that
			// admits agents the claimant's own class does not.
			name:     "runner claiming a DIFFERENT session in its namespace: denied",
			username: "system:serviceaccount:default:lead-1-runner-sa",
			ns:       "default", parentNS: "default", parent: "lead-2",
			allowed: false, contains: "lead-2",
		},
		{
			// §2.10 exploit 3, the one that defeats monotonic identity: an
			// agent-identity session claiming a userPassthrough parent. The
			// webhook does not need to know either session's identity mode —
			// refusing every foreign parent refuses this case too.
			name:     "runner claiming a session in ANOTHER namespace: denied",
			username: "system:serviceaccount:default:lead-1-runner-sa",
			ns:       "default", parentNS: "other", parent: "lead-1",
			allowed: false, contains: "other",
		},
		{
			name:     "principal that is not a runner ServiceAccount: allowed, RBAC is its control",
			username: "system:serviceaccount:agentprimitives-system:spicebox-operator",
			ns:       "default", parentNS: "default", parent: "lead-2",
			allowed: true,
		},
		{
			name:     "human user: allowed, RBAC is its control",
			username: "kubernetes-admin",
			ns:       "default", parentNS: "default", parent: "lead-2",
			allowed: true,
		},
		{
			// Fail closed on a principal that LOOKS like a runner but whose
			// name cannot be parsed into a session. A guard whose unrecognized
			// direction is permissive is the Ruling 13 defect.
			name:     "malformed runner principal: denied",
			username: "system:serviceaccount:default:-runner-sa",
			ns:       "default", parentNS: "default", parent: "lead-1",
			allowed: false, contains: "cannot be attributed",
		},
		{
			// Same fail-closed property, one input shape earlier: no colon at
			// all separating a namespace from a name. The prefix and suffix
			// still both match — the same two checks a matchConditions filter
			// would use — so this principal reaches the handler; it must not
			// fall through to the permissive "not a runner at all" branch.
			name:     "runner-shaped principal with no namespace/name colon: denied",
			username: "system:serviceaccount:foo-runner-sa",
			ns:       "default", parentNS: "default", parent: "lead-1",
			allowed: false, contains: "cannot be attributed",
		},
	}

	w := newTestWebhook(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := w.Handle(context.Background(),
				req(t, admissionv1.Create, tc.ns, tc.username, tc.parentNS, tc.parent))
			assert.Equal(t, tc.allowed, res.Allowed, "Allowed")
			if tc.contains != "" {
				require.NotNil(t, res.Result, "a denial must carry a Result")
				assert.Contains(t, res.Result.Message, tc.contains,
					"the denial must name what was refused, not fail generically")
			}
		})
	}
}

func TestHandle_UpdateIsCheckedToo(t *testing.T) {
	// Unlike AgentSession.spec.parent, SubagentRequest.spec.parent carries NO
	// CEL immutability rule — that rule is on a different resource, so this
	// webhook cannot lean on one at the CRD level the way that other resource
	// can. RBAC is today's actual defense here: BuildRunnerRBAC grants runners
	// only create and get on subagentrequests, never update. Gating UPDATE in
	// this webhook is a cheap backstop against a FUTURE grant widening that
	// Role — not proof, on its own, that an UPDATE path is reachable today. A
	// webhook that gated only CREATE would leave that future widening
	// unchecked, which is what this test pins.
	w := newTestWebhook(t)
	res := w.Handle(context.Background(),
		req(t, admissionv1.Update, "default", "system:serviceaccount:default:lead-1-runner-sa", "default", "lead-2"))
	assert.False(t, res.Allowed, "an UPDATE repointing parent at a foreign session must be denied")
}

func TestHandle_DeleteCarriesNoObject_Allowed(t *testing.T) {
	w := newTestWebhook(t)
	res := w.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:default:lead-1-runner-sa"},
	}})
	assert.True(t, res.Allowed, "DELETE carries no object to check")
}
