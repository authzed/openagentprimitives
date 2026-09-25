package authz

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
)

// PromoteObservedSlots binds the instances an observation NAMED, completing the
// observed fill source.
//
// Without it the source is inert in the quietest possible way. A slot declaring
// `fillFrom: [observed]` cannot be occupied by a class default, by an instance
// the extractor found in a message, or by a thread seed — those are exactly the
// routes the declaration closes — so if nothing proposes the objects the facts
// were written about, the slot is permanently empty, every permissioned call
// against it fails closed, and no test anywhere goes red. This function is the
// only producer of candidates for that source.
//
// The split of duties across processes is the security shape, not an accident
// of where the code grew — the same shape PromoteExtractedSlots documents.
// Facts are recorded by whoever observed them: channelsd, from a delivery it
// verified the signature of, and the runner's own tool dispatch, from a
// declared CEL expression over a successful tool result. Neither writer makes
// an authorization decision or holds a SpiceDB client. THIS runs in the runner,
// which does: it re-derives every subject against the class's own declarations
// and Checks it for the requester. A recorded fact is a proposal; binding is a
// decision.
//
// Three filters, each closing a different hole:
//
//   - A subject whose resourceType the class does not DECLARE is never even
//     proposed: the read is handed exactly the declared type list and returns
//     nothing outside it, so a fact about a git_commit cannot put an instance
//     in a github_pr slot. `observes` blocks are class config rather than
//     model output, but "authored by the same person" is not "constrained",
//     and one payload legitimately names several types (§3's co-derivation
//     binds both the head commit AND the pull request).
//   - A declared type whose fillFrom excludes `observed` is dropped, so the
//     declaration governs this source the way it governs the others. This is
//     the filter that makes `fillFrom` mean something here rather than
//     decorate it.
//   - What survives is Checked for the SUBJECT, so an instance binds only where
//     the requester already had standing. Observing an object is not a way to
//     acquire authority over it — which is why no approval is raised here and a
//     refused candidate is simply dropped.
//
// # Both fact Kinds propose, and the trust grade is not lost by that
//
// envelope_fact and observed_fact are separate Kinds because provenance must be
// structural rather than asserted, and the design says plainly that a signed
// delivery's facts are an observation too — not a second mechanism, differing
// only in trust grade. So both are read here: a slot gated on
// `facts.envelope.*` whose candidate nobody proposed would be inert in exactly
// the way this function exists to prevent. The grade survives because it is
// carried by the Kind, and the precondition addresses it by Kind
// (`facts.envelope.x` is not `facts.observed.x`); which namespace a candidate's
// facts must satisfy is the predicate's business, not this walk's.
//
// # Facts are keyed RAW, and read back that way
//
// A subject id comes back pre-transform, as Record wrote it. The slot's
// ValueTransforms run HERE, on the way out, for the SpiceDB call only. The
// reverse — looking a fact up by the derived authz.ObjectID — cannot ever match
// (spicedb_escape exists because `#` is illegal in an object id) and fails as
// an empty result, which is indistinguishable from "nothing was observed".
//
// Idempotent, and re-run after every dispatch round rather than once: grants
// are TOUCHed and the scope write appends uniquely, so re-promoting a fact
// already bound is a no-op. Re-running is what makes the source MID-TURN — the
// tool call that recorded the fact and the call that needs the binding are
// normally two dispatches of the same turn, with no user message between them.
func PromoteObservedSlots(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	sess SessionRef,
	entities []BoundEntitySpec,
	chk Checker,
	w RelWriter,
	subject string,
	now func() time.Time,
	sessionExpiration time.Duration,
) error {
	if chk == nil || subject == "" || mem == nil || len(entities) == 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	logger := log.FromContext(ctx).WithName("authz.promote_observed")

	// Index the class's declarations, then read ONCE for the whole list.
	// Promotion runs after every dispatch round, against the operator's memory
	// API in production, so the cost is two queries per round whatever the slot
	// count — and two even in the overwhelming case where nothing has ever been
	// observed.
	//
	// The undeclared-type filter stays STRUCTURAL: SubjectsOfTypes is given
	// exactly the declared list and returns nothing outside it, so a fact about
	// a type no slot declares is never a candidate. It is not filtered out
	// afterwards, it is never proposed.
	declared := make(map[string]BoundEntitySpec, len(entities))
	types := make([]string, 0, len(entities))
	for _, et := range entities {
		if et.ResourceType == "" {
			continue // a slot naming no type can hold nothing
		}
		if _, dup := declared[et.ResourceType]; dup {
			// Admission rejects a class declaring two slots of one type
			// (pkg/controllers/agentclass/slot_declaration.go), so this is
			// unreachable from a validated class. Logged rather than silently
			// resolved because if it ever became reachable, the second slot's
			// permission would go ungranted with no trace.
			logger.Info("two declared slots name the same resource type; the first governs the observed source",
				"resourceType", et.ResourceType)
			continue
		}
		declared[et.ResourceType] = et
		types = append(types, et.ResourceType)
	}
	if len(types) == 0 {
		return nil
	}

	subjects, err := observedSubjects(ctx, mem, memScope, types)
	if err != nil {
		return fmt.Errorf("PromoteObservedSlots: %w", err)
	}

	var cands []slotCandidate
	// Keyed on (resourceType, RAW id): the two Kinds may both have recorded
	// facts about the same object — the same pull request arriving in a signed
	// delivery and then being observed by a tool call is the ordinary case, not
	// an edge one — and a duplicate candidate would write the same grant twice
	// and log every drop twice.
	seen := make(map[[2]string]bool, len(subjects))
	for _, s := range subjects {
		key := [2]string{s.ResourceType, s.ResourceID}
		if seen[key] {
			continue
		}
		seen[key] = true
		// Present by construction — SubjectsOfTypes was given these very keys —
		// but read with comma-ok so a future change to that contract surfaces
		// as a dropped candidate with a log line rather than as a zero-valued
		// BoundEntitySpec binding an empty permission.
		et, ok := declared[s.ResourceType]
		if !ok {
			logger.Info("observed subject names a type this class does not declare; dropped",
				"resourceType", s.ResourceType, "resourceID", s.ResourceID)
			continue
		}
		if !et.AllowsFill(FillObserved) {
			logger.Info("observed subject names a slot whose fillFrom admits no observed binding; dropped",
				"resourceType", s.ResourceType, "resourceID", s.ResourceID, "fillFrom", et.FillFrom)
			continue
		}
		id, err := NewObjectID(s.ResourceID, et.ValueTransforms)
		if err != nil {
			// The recorded subject cannot become an object id, so there is no
			// instance to bind. Logged rather than dropped in silence: an
			// `observes` block deriving ids the slot's own transform chain
			// refuses is a class-authoring bug that would otherwise present
			// only as a gate that never opens.
			logger.Info("observed subject could not be derived into an object id; dropped",
				"resourceType", s.ResourceType, "resourceID", s.ResourceID, "err", err.Error())
			continue
		}
		cands = append(cands, slotCandidate{
			ResourceType: s.ResourceType,
			Permission:   et.Permission,
			ResourceID:   id,
			// The RAW subject id, as Record wrote it — the key the slot's
			// preconditions read their facts back by. The transform chain ran
			// only for `id` above, for the SpiceDB call.
			RawID:    s.ResourceID,
			Requires: et.Requires,
		})
	}
	if len(cands) == 0 {
		return nil
	}

	bound := admissibleCandidates(ctx, mem, memScope, chk, subject, string(scope.SourceObserved), logger, cands)
	return bindSlots(ctx, mem, memScope, sess, w, scope.SourceObserved, logger, now, SlotGrantExpiry(now(), sessionExpiration), bound)
}

// observedSubjects returns every object of any declared type that either fact
// Kind has recorded something about, envelope facts first. Two queries, whatever
// the slot count.
//
// Both reads must succeed or the whole promotion fails. A partial answer here
// would silently under-propose — the slot stays empty and reads exactly like a
// session in which nothing was ever observed — so "could not read one of the
// two" is returned rather than degraded into "there were fewer".
func observedSubjects(ctx context.Context, mem memory.Memory, memScope memory.Scope, types []string) ([]factcontent.Subject, error) {
	env, err := envelopefact.SubjectsOfTypes(ctx, mem, memScope, types)
	if err != nil {
		return nil, fmt.Errorf("read envelope_fact subjects of %v: %w", types, err)
	}
	obs, err := observedfact.SubjectsOfTypes(ctx, mem, memScope, types)
	if err != nil {
		return nil, fmt.Errorf("read observed_fact subjects of %v: %w", types, err)
	}
	return append(env, obs...), nil
}
