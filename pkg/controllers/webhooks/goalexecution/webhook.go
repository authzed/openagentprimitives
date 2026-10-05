// Package goalexecution protects operator-created goal session references.
package goalexecution

import (
	"context"
	"reflect"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/validate-goal-execution"

type Handler struct {
	Decoder         admission.Decoder
	OperatorSubject string
}

func (h *Handler) Handle(_ context.Context, r admission.Request) admission.Response {
	var sess v1.AgentSession
	if err := h.Decoder.Decode(r, &sess); err != nil {
		return admission.Errored(400, err)
	}
	var old v1.AgentSession
	if r.Operation == admissionv1.Update {
		if err := h.Decoder.DecodeRaw(r.OldObject, &old); err != nil {
			return admission.Errored(400, err)
		}
	}
	if old.Spec.GoalExecution == nil && sess.Spec.GoalExecution == nil {
		return admission.Allowed("ordinary session")
	}
	if r.Operation == admissionv1.Create {
		if h.OperatorSubject == "" || r.UserInfo.Username != h.OperatorSubject || sess.Spec.Parent != nil || sess.Spec.ForkedFrom != "" || sess.Spec.AgentIdentity != "" {
			return admission.Denied("goal sessions require an operator-created root with user passthrough")
		}
		return admission.Allowed("operator goal session")
	}
	if !reflect.DeepEqual(old.Spec, sess.Spec) {
		return admission.Denied("goal session spec and identity annotations are immutable")
	}
	for _, key := range []string{v1.AnnotationStartedByCanonicalID, v1.AnnotationStartedByEmail, v1.AnnotationStartedByExternalID} {
		if old.Annotations[key] != sess.Annotations[key] {
			return admission.Denied("goal session owner is immutable")
		}
	}
	return admission.Allowed("goal session status update")
}
