package main

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
)

// The two inputs the trifecta containment tripper needs that are not a SpiceDB
// lookup. Both are deliberately derived from what the OPERATOR can see, never
// from anything the runner produced — hold's doc is explicit that the runner is
// the party under suspicion, and a tripper reading runner-written records is
// defeated by a compromised runner simply writing none.

// operatorBoundTagsOf returns the pt-tags bound into a session's data slots,
// read from the SubagentRequest's STATUS.
//
// Status, not spec: spec.dataSlots is what the parent ASKED for, and the parent
// is inside the closure being judged. status.boundDataSlots is what the
// controller actually granted after attenuation and grading — the platform's
// own record of what this session holds.
//
// A root session owns no SubagentRequest and holds no delegated data, so it has
// no bound tags. That is a FACT about a root, not a missing answer, and it
// returns cleanly: legs A and B are then false because nothing is bound, which
// is exactly right rather than a gap.
func operatorBoundTagsOf(ctx context.Context, c client.Reader, sess *spiceboxv1alpha1.AgentSession) ([]string, error) {
	sr, err := spiceboxv1alpha1.OwningSubagentRequest(ctx, c, sess)
	if err != nil {
		return nil, fmt.Errorf("bound tags of %s/%s: %w", sess.Namespace, sess.Name, err)
	}
	if sr == nil {
		return nil, nil
	}
	tags := make([]string, 0, len(sr.Status.BoundDataSlots))
	for _, s := range sr.Status.BoundDataSlots {
		if s.TagID != "" {
			tags = append(tags, s.TagID)
		}
	}
	return tags, nil
}

// operatorClassCanAct answers leg C by loading the session's class and asking
// the ONE derivation the admission validator also uses.
//
// A class that cannot be read is an ERROR, not a false leg. Leg C reading false
// makes the trifecta unable to complete — the trifecta silently not firing,
// which is the failure this whole track exists to prevent, and it would happen
// on exactly the transient API blip nobody notices.
func operatorClassCanAct(ctx context.Context, c client.Reader, sess *spiceboxv1alpha1.AgentSession) (bool, error) {
	var ac spiceboxv1alpha1.AgentClass
	if err := c.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &ac); err != nil {
		return false, fmt.Errorf("class %s/%s of session %s: %w", sess.Namespace, sess.Spec.Class, sess.Name, err)
	}
	return agentclass.ClassCanAct(ctx, c, &ac)
}
