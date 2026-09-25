package runner

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// BindDataSlotFor builds the appender behind send_input: the parent adding one
// data slot to a delegation it already made.
//
// It writes the SubagentRequest's SPEC, which this session owns — it created
// the object — and nothing else. Every check that decides whether the datum
// actually reaches the child runs operator-side afterwards: attenuation (can
// this parent read the tag at all?) and grading (would giving it to THIS child
// disclose, or is it untrusted?), with a disclosing one routed to a human.
//
// So the worst a compromised parent achieves here is offering something it
// already holds to a child the operator will re-judge — which is the same
// envelope delegate's `inputs` operates in, extended in time.
//
// Re-offering a slot+tag already present is a NO-OP rather than a duplicate.
// The spec is a set of offers, and a repeated one is the same offer; appending
// twice would make the controller grade it twice and, worse, could publish a
// second disclosure card for a question already in front of a person.
func BindDataSlotFor(c client.Client, namespace, sessionName string) func(context.Context, string, string, string) error {
	return func(ctx context.Context, requestName, slot, tagID string) error {
		var sr spiceboxv1alpha1.SubagentRequest
		key := types.NamespacedName{Namespace: namespace, Name: requestName}
		if err := c.Get(ctx, key, &sr); err != nil {
			return fmt.Errorf("reading delegation %q: %w", requestName, err)
		}
		// The handle is model-authored and names a request in this NAMESPACE,
		// which is not the same as naming one of THIS session's delegations.
		// reply_to_subagent makes this exact check and documents it as
		// load-bearing; send_input reached the Get and the Update without it,
		// so a parent could append a data offer to a sibling session's live
		// delegation. Two unrelated controls happen to stop the write from
		// mattering today -- the per-session Role grants no update on
		// subagentrequests, and the controller attenuates against
		// sr.Spec.Parent rather than the writer -- but neither is this check,
		// and one of them is already tracked to change.
		if sr.Spec.Parent.Namespace != namespace || sr.Spec.Parent.Name != sessionName {
			return fmt.Errorf("delegation %q is not yours to send to", requestName)
		}
		if sr.IsTerminal() {
			// Refused rather than written: a finished delegation has no child
			// to receive anything, and a silent write would leave the parent
			// believing it answered a request nobody is waiting on.
			return fmt.Errorf("delegation %q has already finished (%s); there is nobody left to send it to",
				requestName, sr.Status.Phase)
		}
		for _, existing := range sr.Spec.DataSlots {
			if existing.Slot == slot && existing.TagID == tagID {
				return nil
			}
		}
		sr.Spec.DataSlots = append(sr.Spec.DataSlots,
			spiceboxv1alpha1.DataSlotRequest{Slot: slot, TagID: tagID})
		if err := c.Update(ctx, &sr); err != nil {
			return fmt.Errorf("offering %q to delegation %q: %w", slot, requestName, err)
		}
		return nil
	}
}
