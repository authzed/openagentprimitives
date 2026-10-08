package authz

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
)

// PromoteExtractedSlots binds the instances the extractor found in this turn's
// user message, completing the query/extract fill source.
//
// The split of duties across processes is the security shape, not an accident
// of where the code grew. authzd runs the extractor LLM over the user's
// untrusted text and records CANDIDATES — it makes no authorization decision and
// holds no SpiceDB client. This runs in the runner, which does: it re-derives
// every candidate against the class's own declarations and Checks it for the
// requester. So a compromised or confused extractor can propose anything and
// still cannot widen the session, because a proposal is not a binding.
//
// Three filters, each closing a different hole:
//
//   - A candidate whose resourceType the class does not DECLARE is dropped. The
//     extractor is told which types to look for, but "told" is not "constrained".
//   - A declared type whose fillFrom excludes query/extract is dropped, so the
//     declaration governs this source the way it governs the others.
//   - What survives is Checked for the SUBJECT, so an instance binds only where
//     the requester already had standing. Binding is the session catching up to
//     authority the user held, never an escalation — which is why no approval is
//     raised here and a refused candidate is simply dropped.
//
// Idempotent: grants are TOUCHed and the scope write appends uniquely, so
// re-running a turn rebinds the same set.
func PromoteExtractedSlots(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	sess SessionRef,
	entities []BoundEntitySpec,
	chk Checker,
	w RelWriter,
	subject string,
	turnIndex int,
	now func() time.Time,
	sessionExpiration time.Duration,
) error {
	if chk == nil || subject == "" || mem == nil || len(entities) == 0 || turnIndex < 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	logger := log.FromContext(ctx).WithName("authz.promote_extracted")

	found, err := extracted_entity.ForTurn(ctx, mem, memScope, turnIndex)
	if err != nil {
		return fmt.Errorf("PromoteExtractedSlots: read extracted_entity turn %d: %w", turnIndex, err)
	}
	if len(found) == 0 {
		return nil
	}

	// Index the class's declarations so an undeclared type cannot bind.
	declared := make(map[string]BoundEntitySpec, len(entities))
	for _, et := range entities {
		declared[et.ResourceType] = et
	}

	var cands []slotCandidate
	for _, f := range found {
		et, ok := declared[f.ResourceType]
		if !ok {
			logger.Info("extracted candidate names a type this class does not declare; dropped",
				"resourceType", f.ResourceType, "resourceID", f.ResourceID, "turn", turnIndex)
			continue
		}
		if !AllowsExtractedBinding(et.FillFrom) {
			logger.Info("extracted candidate for a slot whose fillFrom admits no extractor binding; dropped",
				"resourceType", f.ResourceType, "resourceID", f.ResourceID,
				"fillFrom", et.FillFrom, "turn", turnIndex)
			continue
		}
		if f.ResourceID == "" {
			continue // names no instance; nothing to grant or revoke later
		}
		id, err := NewObjectID(f.ResourceID, et.ValueTransforms)
		if err != nil {
			// The extractor's candidate cannot become an object id, so there is
			// no instance to bind. Log rather than drop silently: a confused or
			// adversarial extractor producing undrivable candidates is worth
			// seeing, even though it gates nothing (the candidate is dropped
			// either way).
			logger.Info("extracted candidate could not be derived into an object id; dropped",
				"resourceType", f.ResourceType, "resourceID", f.ResourceID, "turn", turnIndex, "err", err.Error())
			continue
		}
		cands = append(cands, slotCandidate{
			ResourceType: f.ResourceType,
			Permission:   et.Permission,
			ResourceID:   id,
			// The extractor's own pre-transform value: what a fact about this
			// instance would have been keyed by, had one been recorded.
			RawID:     f.ResourceID,
			Requires:  et.Requires,
			Occupancy: et.Occupancy,
			Rebind:    et.Rebind,
		})
	}

	bound := admissibleCandidates(ctx, mem, memScope, chk, subject, string(scope.SourceExtracted), logger, cands)
	return bindSlots(ctx, mem, memScope, sess, w, scope.SourceExtracted, logger, now, SlotGrantExpiry(now(), sessionExpiration), bound)
}
