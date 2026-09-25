package plangate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// FuzzFold drives arbitrary record sequences through the fold and asserts the
// one property that matters: no sequence of records can conjure reach the
// human never approved.
//
// The fold reads an append-only log that a compromised or buggy writer could
// have filled with anything — out-of-range indices, foreign digests, missing
// fields, absurd orderings. Its job is to be uninteresting under all of it:
// either it names a phase from the frozen plan, or it names none at all.
//
// Two invariants, checked on every input:
//
//  1. Never panics. The fold runs on the dispatch path; a panic there takes
//     down a live session.
//  2. The active ceiling is always a SUBSET of the frozen plan's union. This is
//     the safety property the whole design rests on — the log can move the
//     agent between approved ceilings, and can strand it with none, but it can
//     never produce authority the frozen plan does not contain.
func FuzzFold(f *testing.F) {
	f.Add(0, 0, "", 3)
	f.Add(1, 2, "wrong-digest", 2)
	f.Add(-1, 99, "", 5)
	f.Add(2, 1, "", 0)

	f.Fuzz(func(t *testing.T, evSeed, idxSeed int, digest string, nRecords int) {
		plan := fuzzPlan(t)
		union := map[string]struct{}{}
		for _, ph := range plan.Phases {
			for _, h := range ph.Permissions {
				union[h.String()] = struct{}{}
			}
		}

		// Bound the record count so the fuzzer explores shapes rather than
		// spending its budget on length.
		if nRecords < 0 {
			nRecords = -nRecords
		}
		nRecords %= 32

		events := []string{
			plangateaudit.EventPlanApproved,
			plangateaudit.EventPhaseSelected,
			plangateaudit.EventDenied,
			plangateaudit.EventSuperseded,
			plangateaudit.EventGateAllowed,
			"totally_unknown_event",
			"",
		}

		recs := make([]plangateaudit.Content, 0, nRecords)
		for i := 0; i < nRecords; i++ {
			ev := events[abs(evSeed+i)%len(events)]
			rec := plangateaudit.Content{Event: ev}

			// Alternate between this plan's digest, a caller-supplied one, and
			// empty, so foreign and malformed records both get explored.
			switch (evSeed + i) % 3 {
			case 0:
				rec.PlanDigest = plan.Digest()
			case 1:
				rec.PlanDigest = digest
			}

			// Every third record omits the phase index entirely.
			if (idxSeed+i)%3 != 0 {
				v := int32((idxSeed + i) % 7) // deliberately overruns the plan
				if (idxSeed+i)%5 == 0 {
					v = -v
				}
				rec.PhaseIndex = &v
			}
			recs = append(recs, rec)
		}

		st, err := Fold(plan, recs)
		require.NoError(t, err, "Fold must not return a Go error for malformed records")

		ceiling, cerr := st.ActiveCeiling()
		if cerr != nil {
			// A doubtful fold must yield nothing at all — never a partial or
			// stale ceiling.
			require.Empty(t, ceiling, "a fold that errored must hand back no authority")
			return
		}
		for h := range ceiling {
			if _, ok := union[h.String()]; !ok {
				t.Fatalf("fold produced handle %q, which is not in the frozen plan's union", h.String())
			}
		}
	})
}

func fuzzPlan(t *testing.T) Plan {
	t.Helper()
	surface := surfaceWith(t, "perm:read:tracker_issue", "perm:write:tracker_issue")
	p, _ := FreezeFrom([]AuthoredPhase{
		authored("a", "A", "perm:read:tracker_issue"),
		authored("b", "B", "perm:write:tracker_issue"),
	}, surface, nil)
	return p
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
