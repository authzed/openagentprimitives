package goalexecution

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/cel-go/cel"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
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
		{"ordinary session squatting a goal session name", admissionv1.Create, "employee", func(s *v1.AgentSession) {
			s.Name, s.Spec.GoalExecution = "goal-0123456789abcdef0123456789abcdef", nil
		}, false},
		{"ordinary session with a goal-prefixed name", admissionv1.Create, "employee", func(s *v1.AgentSession) {
			s.Name, s.Spec.GoalExecution = "goal-tracker", nil
		}, true},
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
	assert.ElementsMatch(t, []string{"agentsessions", "agentsessions/status"}, hook.Rules[0].Resources)

	// The API server only calls the handler when this condition is true, so
	// every request the handler refuses must match it. Evaluate the shipped
	// expression rather than trusting Handle alone.
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
	require.NoError(t, err)
	ast, issues := env.Compile(hook.MatchConditions[0].Expression)
	require.NoError(t, issues.Err())
	program, err := env.Program(ast)
	require.NoError(t, err)
	session := func(name string, goal bool) map[string]any {
		spec := map[string]any{"class": "assistant"}
		if goal {
			spec["goalExecution"] = map[string]any{"occurrenceID": "occurrence"}
		}
		return map[string]any{"metadata": map[string]any{"name": name}, "spec": spec}
	}
	for _, tc := range []struct {
		name      string
		operation string
		object    any
		oldObject any
		routed    bool
	}{
		{"goal session create", "CREATE", session("goal-0123456789abcdef0123456789abcdef", true), nil, true},
		{"goal session update", "UPDATE", session("goal-0123456789abcdef0123456789abcdef", true), session("goal-0123456789abcdef0123456789abcdef", true), true},
		{"reference removed on update", "UPDATE", session("s", false), session("s", true), true},
		{"ordinary create under a reserved goal name", "CREATE", session("goal-0123456789abcdef0123456789abcdef", false), nil, true},
		{"ordinary create with a goal-prefixed name", "CREATE", session("goal-tracker", false), nil, false},
		{"ordinary create", "CREATE", session("chat", false), nil, false},
		{"ordinary update of a legacy reserved name", "UPDATE", session("goal-0123456789abcdef0123456789abcdef", false), session("goal-0123456789abcdef0123456789abcdef", false), false},
	} {
		t.Run("routes "+tc.name, func(t *testing.T) {
			out, _, err := program.Eval(map[string]any{"object": tc.object, "oldObject": tc.oldObject, "request": map[string]any{"operation": tc.operation}})
			require.NoError(t, err)
			assert.Equal(t, tc.routed, out.Value())
		})
	}
	for _, name := range []string{"goal-0123456789abcdef0123456789abcdef", "goal-0123456789ABCDEF0123456789abcdef", "goal-0123", "goal-tracker", "chat"} {
		out, _, err := program.Eval(map[string]any{"object": session(name, false), "oldObject": nil, "request": map[string]any{"operation": "CREATE"}})
		require.NoError(t, err)
		assert.Equal(t, goals.IsSessionName(name), out.Value(), "the shipped name clause and goals.IsSessionName agree on %q", name)
	}
}
