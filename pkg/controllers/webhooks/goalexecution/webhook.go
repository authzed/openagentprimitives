// Package goalexecution protects operator-created goal session references.
package goalexecution

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/validate-goal-execution"

type Handler struct {
	Decoder         admission.Decoder
	OperatorSubject string
	// ChannelsdSubject is channelsd's service account, the one writer of a
	// group run's thread anchor. Empty refuses every anchor write-back.
	ChannelsdSubject string
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
		if r.Operation == admissionv1.Create && goals.IsSessionName(sess.Name) {
			return admission.Denied("session names of the form goal-<digest> are reserved for goal executions")
		}
		return admission.Allowed("ordinary session")
	}
	if r.Operation == admissionv1.Create {
		if h.OperatorSubject == "" || r.UserInfo.Username != h.OperatorSubject || sess.Spec.Parent != nil || sess.Spec.ForkedFrom != "" || sess.Spec.AgentIdentity != "" {
			return admission.Denied("goal sessions require an operator-created root with user passthrough")
		}
		return admission.Allowed("operator goal session")
	}
	if !reflect.DeepEqual(old.Spec, sess.Spec) && !h.anchorWriteBack(r, &old, &sess) {
		return admission.Denied("goal session spec and identity annotations are immutable")
	}
	for _, key := range []string{v1.AnnotationStartedByCanonicalID, v1.AnnotationStartedByEmail, v1.AnnotationStartedByExternalID} {
		if old.Annotations[key] != sess.Annotations[key] {
			return admission.Denied("goal session owner is immutable")
		}
	}
	return admission.Allowed("goal session status update")
}

// anchorWriteBack reports whether r is the one spec change a group goal's
// run admits: channelsd writing back, on the run's first post, the thread
// anchor its kind's sender opened. A group run is the one goal session started
// by no one (the dispatcher writes no started-by annotation for it, and those
// annotations are immutable here), and it is validated by the group its
// bindings address rather than their bytes. A private run's binding is still
// compared byte for byte, so its spec stays wholly immutable.
//
// The rule names no kind's fields; the kind's own contract decides:
//   - before has no anchor yet: RunAnchor(before) is before itself;
//   - after differs from before only in anchor fields: RunAnchor(after) is
//     before (so the Channel, conversation and everything else are unchanged);
//   - after's anchor is complete and self-consistent: RunAnchorWritten(after);
//   - nothing else in the spec changed, the input binding included.
func (h *Handler) anchorWriteBack(r admission.Request, old, sess *v1.AgentSession) bool {
	if h.ChannelsdSubject == "" || r.UserInfo.Username != h.ChannelsdSubject {
		return false
	}
	for _, key := range []string{v1.AnnotationStartedByCanonicalID, v1.AnnotationStartedByEmail, v1.AnnotationStartedByExternalID} {
		if old.Annotations[key] != "" {
			return false
		}
	}
	before, after := old.Spec.OutputChannel, sess.Spec.OutputChannel
	if before == nil || after == nil || before.Kind != after.Kind {
		return false
	}
	host, ok := registry.GroupGoalHostFor(after.Kind)
	if !ok {
		return false
	}
	unanchored, err := host.RunAnchor(before)
	if err != nil || !reflect.DeepEqual(unanchored, before) {
		return false
	}
	stripped, err := host.RunAnchor(after)
	if err != nil || !reflect.DeepEqual(stripped, before) || !host.RunAnchorWritten(after) {
		return false
	}
	want := old.Spec.DeepCopy()
	want.OutputChannel = after.DeepCopy()
	return reflect.DeepEqual(*want, sess.Spec)
}
