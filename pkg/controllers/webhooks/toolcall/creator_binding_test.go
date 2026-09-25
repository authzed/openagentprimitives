package toolcall

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// createReqAs is createReq with an authenticated requester, which is what the
// creator-binding rule reads. The existing createReq leaves UserInfo empty.
func createReqAs(t *testing.T, tc *spiceboxv1alpha1.ToolCall, username string) admission.Request {
	t.Helper()
	raw, err := json.Marshal(tc)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: username},
	}}
}

func runnerOf(ns, session string) string {
	return "system:serviceaccount:" + ns + ":" + session + "-runner-sa"
}

// The ownerReference is metadata the CREATOR writes, so "owned by an
// AgentSession" proves only that the creator was willing to type one. Nothing
// tied it to the creator's own session.
//
// The consequence is not theoretical: the runner Role grants toolcalls
// create/get/list with no resourceNames, sessions share a namespace, and the
// controller resolves both the sandbox to exec into and the AgentSession whose
// use_token grant is consulted from spec.session — the same field. So naming a
// sibling's bundle made that sibling's own grant authorize the call, and the
// call ran in the sibling's sandbox with the sibling's credentials.
//
// Both sibling webhooks (subagentrequest, agentsession) already read
// req.UserInfo for exactly this reason.
func TestToolCallWebhook_RunnerMayNotCreateForAnotherSession(t *testing.T) {
	h := handler(t)

	// Session "victim" is the owner named on the object; the request comes
	// from session "attacker"'s runner in the same namespace.
	tc := toolCall("ns1", func(tc *spiceboxv1alpha1.ToolCall) {
		tc.OwnerReferences[0].Name = "victim"
		tc.Spec.Session = "victim-bundle"
	})

	resp := h.Handle(context.Background(), createReqAs(t, tc, runnerOf("ns1", "attacker")))
	assert.False(t, resp.Allowed,
		"a runner naming another session's AgentSession as owner must be refused")
}

// The legitimate path must keep working: a runner creating a ToolCall for its
// own session is the only way any tool runs at all.
func TestToolCallWebhook_RunnerMayCreateForItsOwnSession(t *testing.T) {
	h := handler(t)

	tc := toolCall("ns1", func(tc *spiceboxv1alpha1.ToolCall) {
		tc.OwnerReferences[0].Name = "sess"
	})

	resp := h.Handle(context.Background(), createReqAs(t, tc, runnerOf("ns1", "sess")))
	assert.True(t, resp.Allowed, "a runner creating for its own session is the ordinary case: %s", resp.Result.Message)
}

// A runner in a DIFFERENT namespace whose session name happens to match must
// still be refused. Namespace is part of the identity, not decoration.
func TestToolCallWebhook_RunnerFromAnotherNamespaceIsRefused(t *testing.T) {
	h := handler(t)

	tc := toolCall("ns1", func(tc *spiceboxv1alpha1.ToolCall) {
		tc.OwnerReferences[0].Name = "sess"
	})

	resp := h.Handle(context.Background(), createReqAs(t, tc, runnerOf("ns2", "sess")))
	assert.False(t, resp.Allowed,
		"same session name in a different namespace is a different session")
}

// A runner-shaped username that does not parse into a session must fail
// CLOSED. This is the gap the subagentrequest webhook documents and
// deliberately did not backport to its sibling; a guard whose unrecognized
// direction is permissive is not a guard.
func TestToolCallWebhook_UnparseableRunnerPrincipalFailsClosed(t *testing.T) {
	h := handler(t)
	tc := toolCall("ns1")

	// Prefix and suffix both match, but there is no colon separating the
	// namespace from the name — so a matchConditions filter would still dial
	// this webhook for it.
	resp := h.Handle(context.Background(), createReqAs(t, tc, "system:serviceaccount:ns1-runner-sa"))
	assert.False(t, resp.Allowed,
		"a runner-shaped principal that cannot be attributed to a session must be denied, not waved through")
}

// A non-runner principal — the operator's own ServiceAccount, or a human
// applying a manifest — is not subject to the binding rule, because it has no
// session to be bound to. The other CREATE checks still apply to it.
func TestToolCallWebhook_NonRunnerPrincipalIsUnaffected(t *testing.T) {
	h := handler(t)
	tc := toolCall("ns1")

	for _, u := range []string{
		"system:serviceaccount:agentprimitives-system:spicebox-operator",
		"kubernetes-admin",
	} {
		resp := h.Handle(context.Background(), createReqAs(t, tc, u))
		assert.True(t, resp.Allowed, "principal %q is not a runner and must not be bound: %s", u, resp.Result.Message)
	}
}

// The binding rule must not weaken the checks that were already there: a
// runner creating for its own session still cannot reach a cross-namespace
// credential source.
func TestToolCallWebhook_BindingDoesNotReplaceTheCredentialChecks(t *testing.T) {
	h := handler(t)

	tc := toolCall("ns1", func(tc *spiceboxv1alpha1.ToolCall) {
		tc.OwnerReferences[0].Name = "sess"
		tc.Spec.Credentials[0].Source.Namespace = "other-ns"
	})

	resp := h.Handle(context.Background(), createReqAs(t, tc, runnerOf("ns1", "sess")))
	assert.False(t, resp.Allowed, "a cross-namespace credential source stays refused")
}
