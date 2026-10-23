package goalexecution

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	registrationv1 "k8s.io/api/admissionregistration/v1"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestOperatorCreationAndImmutableAuthority(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	h := &Handler{Decoder: admission.NewDecoder(scheme), OperatorSubject: "system:serviceaccount:system:operator"}
	root := v1.AgentSession{Spec: v1.AgentSessionSpec{Class: "assistant", GoalExecution: &v1.GoalExecutionReference{GoalID: "goal", OccurrenceID: "occurrence", GoalRevision: 3, ConsentDigest: "digest"}}}
	for _, tc := range []struct {
		name      string
		operation admissionv1.Operation
		user      string
		mutate    func(*v1.AgentSession)
		allowed   bool
	}{
		{"operator root", admissionv1.Create, h.OperatorSubject, func(*v1.AgentSession) {}, true},
		{"user forgery", admissionv1.Create, "employee", func(*v1.AgentSession) {}, false},
		{"runner forgery", admissionv1.Create, "system:serviceaccount:team:session-runner-sa", func(*v1.AgentSession) {}, false},
		{"identity override", admissionv1.Create, h.OperatorSubject, func(s *v1.AgentSession) { s.Spec.AgentIdentity = "shared" }, false},
		{"fork", admissionv1.Create, h.OperatorSubject, func(s *v1.AgentSession) { s.Spec.ForkedFrom = "other" }, false},
		{"status progress", admissionv1.Update, h.OperatorSubject, func(s *v1.AgentSession) { s.Status.Phase = v1.AgentSessionPhaseIdle }, true},
		{"revision change", admissionv1.Update, h.OperatorSubject, func(s *v1.AgentSession) { s.Spec.GoalExecution.GoalRevision++ }, false},
		{"remove reference", admissionv1.Update, h.OperatorSubject, func(s *v1.AgentSession) { s.Spec.GoalExecution = nil }, false},
		{"owner takeover", admissionv1.Update, h.OperatorSubject, func(s *v1.AgentSession) {
			s.Annotations = map[string]string{v1.AnnotationStartedByCanonicalID: "forged"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := root.DeepCopy()
			tc.mutate(sess)
			raw, err := json.Marshal(sess)
			require.NoError(t, err)
			old, err := json.Marshal(root)
			require.NoError(t, err)
			request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: tc.operation, UserInfo: authv1.UserInfo{Username: tc.user}, Object: runtime.RawExtension{Raw: raw}, OldObject: runtime.RawExtension{Raw: old}}}
			assert.Equal(t, tc.allowed, h.Handle(context.Background(), request).Allowed)
		})
	}
}
func TestShippedGoalGateFailsClosed(t *testing.T) {
	docs, err := manifests.Split(manifests.Install)
	require.NoError(t, err)
	var hook *registrationv1.ValidatingWebhook
	for _, doc := range docs {
		if doc.GetKind() != "ValidatingWebhookConfiguration" {
			continue
		}
		var config registrationv1.ValidatingWebhookConfiguration
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(doc.Object, &config))
		for i := range config.Webhooks {
			if config.Webhooks[i].Name == "goalexecution.agentprimitives.authzed.com" {
				hook = &config.Webhooks[i]
			}
		}
	}
	require.NotNil(t, hook)
	require.NotNil(t, hook.FailurePolicy)
	assert.Equal(t, registrationv1.Fail, *hook.FailurePolicy)
	require.NotNil(t, hook.ClientConfig.Service)
	assert.Equal(t, Path, *hook.ClientConfig.Service.Path)
	require.Len(t, hook.MatchConditions, 1)
	assert.Contains(t, hook.MatchConditions[0].Expression, "oldObject.spec.goalExecution")
	assert.ElementsMatch(t, []string{"agentsessions", "agentsessions/status"}, hook.Rules[0].Resources)
}
