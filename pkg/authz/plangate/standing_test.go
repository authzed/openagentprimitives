package plangate_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

func TestCoveredByStanding(t *testing.T) {
	// base builds a two-phase standing plan; cases mutate a copy of it.
	base := func() plangate.Plan {
		p := planWith(hs(t, "perm:read:doc"), hs(t, "perm:write:doc"))
		p.Phases[0].Max.Count = 3
		p.Phases[0].Budget.Calls = 5
		p.Phases[1].Requires = []plangate.RequiresEdge{{Phase: 0}}
		p.Phases[0].Slots = []plangate.Slot{{Type: "company", ID: "acme"}}
		return p
	}
	cases := []struct {
		name     string
		standing func() plangate.Plan
		run      func(p plangate.Plan) plangate.Plan
		want     bool
	}{
		{"identical plan: covered", base, func(p plangate.Plan) plangate.Plan { return p }, true},
		{"run has fewer phases: covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases = p.Phases[:1]
			return p
		}, true},
		{"post-only single-phase standing and identical run: covered",
			func() plangate.Plan { return planWith(hs(t, "perm:write:doc")) },
			func(p plangate.Plan) plangate.Plan { return p }, true},
		{"zero-phase run: not covered (fail closed, nothing to approve)", base, func(p plangate.Plan) plangate.Plan {
			p.Phases = nil
			return p
		}, false},
		{"added permission handle: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Permissions = hs(t, "perm:read:doc", "perm:write:doc")
			return p
		}, false},
		{"added slot of a new type: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Slots = append(p.Phases[0].Slots, plangate.Slot{Type: "invoice", ID: "1"})
			return p
		}, false},
		{"same slot type, different ID: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Slots = []plangate.Slot{{Type: "company", ID: "other"}}
			return p
		}, false},
		{"raised Max.Count: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Max.Count = 4
			return p
		}, false},
		{"raised budget: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Budget.Calls = 6
			return p
		}, false},
		{"budget N to 0 (unbounded): not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Budget.Calls = 0
			return p
		}, false},
		{"negative budget against a bounded standing budget: not covered (negative is unbounded)", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Budget.Calls = -1
			return p
		}, false},
		{"negative Max.Count against a bounded standing Max: not covered (negative is unbounded)", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Max.Count = -1
			return p
		}, false},
		{"lowered budget: covered (a run may narrow)", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Budget.Calls = 2
			return p
		}, true},
		{"lowered Max.Count: covered (a run may narrow)", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Max.Count = 1
			return p
		}, true},
		{"removed permission handle: covered (a run may narrow)", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[1].Permissions = nil
			return p
		}, true},
		{"removed slot: covered (a run may narrow)", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Slots = nil
			return p
		}, true},
		{"changed consent: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0].Consents = []json.RawMessage{json.RawMessage(`{"extra":true}`)}
			return p
		}, false},
		{"dropped prerequisite: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[1].Requires = nil
			return p
		}, false},
		{"extra phase: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases = append(p.Phases, plangate.Phase{Label: "C"})
			return p
		}, false},
		{"two phases swapped: not covered", base, func(p plangate.Plan) plangate.Plan {
			p.Phases[0], p.Phases[1] = p.Phases[1], p.Phases[0]
			return p
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			standing := tc.standing()
			assert.Equal(t, tc.want, plangate.CoveredByStanding(standing, tc.run(tc.standing())))
		})
	}
}
