package toolcall

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func tcScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func handler(t *testing.T) *Webhook { return New(admission.NewDecoder(tcScheme(t))) }

// toolCall builds a ToolCall owned by an AgentSession in namespace ns with a
// single same-namespace credential source — the well-formed baseline. Apply
// mutators to perturb a single dimension per case.
func toolCall(ns string, muts ...func(*spiceboxv1alpha1.ToolCall)) *spiceboxv1alpha1.ToolCall {
	tc := &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sess-0-abc", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentSession", Name: "sess", UID: "uid-1",
			}},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{
			Session: "sess-bundle", Tool: "git",
			Args: []string{"clone", "https://example.com/r.git"},
			Credentials: []spiceboxv1alpha1.CredentialDescriptor{{
				Source: spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: ns, Name: "git-creds", Key: "token"},
			}},
		},
	}
	for _, m := range muts {
		m(tc)
	}
	return tc
}

func createReq(t *testing.T, tc *spiceboxv1alpha1.ToolCall) admission.Request {
	t.Helper()
	raw, err := json.Marshal(tc)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw},
	}}
}

func updateReq(t *testing.T, oldTC, newTC *spiceboxv1alpha1.ToolCall) admission.Request {
	t.Helper()
	oldRaw, err := json.Marshal(oldTC)
	require.NoError(t, err)
	newRaw, err := json.Marshal(newTC)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		Object:    runtime.RawExtension{Raw: newRaw},
		OldObject: runtime.RawExtension{Raw: oldRaw},
	}}
}

func TestToolCallWebhook_Create(t *testing.T) {
	h := handler(t)
	cases := []struct {
		name    string
		tc      *spiceboxv1alpha1.ToolCall
		allowed bool
	}{
		{"well-formed: owned + same-ns source", toolCall("default"), true},
		{"no credentials is fine", toolCall("default", func(tc *spiceboxv1alpha1.ToolCall) { tc.Spec.Credentials = nil }), true},
		{"no AgentSession owner -> denied", toolCall("default", func(tc *spiceboxv1alpha1.ToolCall) { tc.OwnerReferences = nil }), false},
		{"wrong owner kind -> denied", toolCall("default", func(tc *spiceboxv1alpha1.ToolCall) { tc.OwnerReferences[0].Kind = "ConfigMap" }), false},
		{"cross-namespace source -> denied", toolCall("default", func(tc *spiceboxv1alpha1.ToolCall) { tc.Spec.Credentials[0].Source.Namespace = "kube-system" }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.Handle(context.Background(), createReq(t, tc.tc))
			assert.Equal(t, tc.allowed, resp.Allowed, "msg=%q", resp.Result.Message)
		})
	}
}

func TestToolCallWebhook_Update_SpecImmutable(t *testing.T) {
	h := handler(t)

	// Status/label-only change with identical spec is allowed.
	old := toolCall("default")
	noSpecChange := toolCall("default", func(tc *spiceboxv1alpha1.ToolCall) {
		tc.Labels = map[string]string{"x": "y"}
	})
	assert.True(t, h.Handle(context.Background(), updateReq(t, old, noSpecChange)).Allowed,
		"a non-spec change must be allowed")

	// Each security-relevant spec edit is denied.
	for _, mut := range []struct {
		name string
		fn   func(*spiceboxv1alpha1.ToolCall)
	}{
		{"tool", func(tc *spiceboxv1alpha1.ToolCall) { tc.Spec.Tool = "rm" }},
		{"args", func(tc *spiceboxv1alpha1.ToolCall) { tc.Spec.Args = []string{"--evil"} }},
		{"credentials", func(tc *spiceboxv1alpha1.ToolCall) { tc.Spec.Credentials[0].Source.Name = "other-secret" }},
		{"stdin", func(tc *spiceboxv1alpha1.ToolCall) { tc.Spec.Stdin = "evil" }},
		{"session", func(tc *spiceboxv1alpha1.ToolCall) { tc.Spec.Session = "other-bundle" }},
	} {
		t.Run("denies "+mut.name+" change", func(t *testing.T) {
			edited := toolCall("default", mut.fn)
			assert.False(t, h.Handle(context.Background(), updateReq(t, old, edited)).Allowed)
		})
	}
}
