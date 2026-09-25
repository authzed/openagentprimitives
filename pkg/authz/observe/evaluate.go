package observe

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/google/cel-go/cel"

	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
)

// Evaluate turns one Block plus one call's (args, result, ...) bindings — see
// the package doc comment for the recognized vars keys — into observations,
// one per resolved item.
//
// Structure mirrors relwrites.Evaluate deliberately, and for the reason its
// own doc comment gives: both share relwrites.ResolveItems for the When gate
// and ForEach fan-out (RULING R2), and both compile each expression once,
// then evaluate it per item, rather than recompiling on every iteration.
//
// The ONE rule that is NOT shared, and the reason this cannot simply reuse
// relwrites.Evaluate wholesale: subjects and facts for a given iteration are
// evaluated together, against the SAME item, inside the SAME loop body, and
// emitted into ONE Observation. There is deliberately no path that evaluates
// one item's facts against a different item's subjects — that pairing is the
// laundering attack this whole design exists to prevent. See
// TestEvaluateGroupsEachItemSeparately, which pins the grouping by asserting
// that PR #6's subjects always carry PR #6's fact, never PR #5's.
func Evaluate(b Block, vars map[string]any) ([]factcontent.Observation, error) {
	items, err := relwrites.ResolveItems(b.When, b.ForEach, vars)
	if err != nil {
		return nil, fmt.Errorf("observe: resolve items: %w", err)
	}
	if items == nil {
		// ResolveItems reserves a nil, error-free result for exactly one case:
		// When evaluated false. The block does not fire for this call — not an
		// error, and nothing left to compile.
		return nil, nil
	}

	// Compile every subject/fact expression once, before the per-item loop —
	// the same shape relwrites.Evaluate uses for its Tuple fields, and for the
	// same reason: a forEach block can resolve many items, and recompiling an
	// identical expression per item would be pure waste on the hot dispatch
	// path (a tool call already ran; this runs on every successful one).
	subjectTypePrgs := make([]cel.Program, len(b.Subjects))
	subjectIDPrgs := make([]cel.Program, len(b.Subjects))
	for i, s := range b.Subjects {
		subjectTypePrgs[i], err = relwrites.CompileStringExpr(s.ResourceType)
		if err != nil {
			return nil, fmt.Errorf("observe: subject %d resourceType: %w", i, err)
		}
		subjectIDPrgs[i], err = relwrites.CompileStringExpr(s.ResourceID)
		if err != nil {
			return nil, fmt.Errorf("observe: subject %d resourceID: %w", i, err)
		}
	}
	factPrgs := make(map[string]cel.Program, len(b.Facts))
	for name, expr := range b.Facts {
		prg, err := relwrites.CompileAnyExpr(expr)
		if err != nil {
			return nil, fmt.Errorf("observe: fact %q: %w", name, err)
		}
		factPrgs[name] = prg
	}

	out := make([]factcontent.Observation, 0, len(items))
	for _, item := range items {
		obsID, err := newObservationID()
		if err != nil {
			return nil, fmt.Errorf("observe: mint observation id: %w", err)
		}
		obs := factcontent.Observation{
			Facts:         make(map[string]any, len(b.Facts)),
			ObservationID: obsID,
		}

		// Both halves of a subject flow through EvalStringWithItem, which
		// already fails closed on an empty string, a CEL eval error, or a
		// non-string result (see its doc comment). That matters for either
		// branch below: a subject that resolved to "" on either half would
		// file a fact under an object nobody can name — unreachable by any
		// gate, so recording it would look like success and behave like
		// silence. This evaluator exists to make that unrepresentable, not
		// merely to inherit it.
		for i := range b.Subjects {
			rt, err := relwrites.EvalStringWithItem(subjectTypePrgs[i], vars, item)
			if err != nil {
				return nil, fmt.Errorf("observe: subject %d resourceType: %w", i, err)
			}
			rid, err := relwrites.EvalStringWithItem(subjectIDPrgs[i], vars, item)
			if err != nil {
				return nil, fmt.Errorf("observe: subject %d resourceID: %w", i, err)
			}
			obs.Subjects = append(obs.Subjects, factcontent.Subject{ResourceType: rt, ResourceID: rid})
		}

		for name, prg := range factPrgs {
			v, err := relwrites.EvalAnyWithItem(prg, vars, item)
			if err != nil {
				return nil, fmt.Errorf("observe: fact %q: %w", name, err)
			}
			obs.Facts[name] = v
		}

		out = append(out, obs)
	}
	return out, nil
}

// newObservationID mints a bare correlation handle for the group of entries
// one resolved item's subjects and facts produce — factcontent.Record fans
// each Observation out into one entry per (subject, fact) pair, and this is
// what lets an auditor join those entries back to one payload (see
// factcontent.Observation.ObservationID's doc comment). Minting it here,
// rather than leaving it blank for factcontent.Record to mint on write,
// means the id is stable from the moment the observation is derived, not
// just from the moment it happens to be recorded.
//
// Package-local rather than reusing factcontent's own newObservationID: that
// helper is unexported to its package. Not a security value — 16 random
// bytes only needs to avoid an accidental collision within one observation's
// lifetime, not resist an adversary.
func newObservationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "obs-" + hex.EncodeToString(b[:]), nil
}
