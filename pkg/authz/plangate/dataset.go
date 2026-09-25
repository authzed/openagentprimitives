package plangate

import (
	"sort"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// Metric is the logging-mode dataset for one session and one plan.
//
// These four numbers are what enforcement is gated on. Enforcement is not switched
// on because the design is elegant; it is switched on when the log says an LLM
// can author a ceiling its own execution stays inside, and that the resulting
// approval volume is bearable. Anything not derivable here is a question the
// logging period cannot answer, which is why the shape is tested directly.
type Metric struct {
	// GatedCalls is calls the plan gate actually governed — those that
	// resolved to a permission handle. Meta and passthrough tools are excluded
	// deliberately: counting them would dilute the denial rate and make every
	// session look safer than it is.
	GatedCalls int

	// WouldDeny is how many of those enforcement would have refused.
	// Question 1.
	WouldDeny int

	// ApprovalsByTier counts the approvals enforcement would have raised, by
	// tier. Question 2.
	ApprovalsByTier map[string]int

	// DeclaredHandles and ExercisedHandles bound over-declaration.
	DeclaredHandles  int
	ExercisedHandles int

	// DeclaredNeverExercised is the reach the agent asked for and never used —
	// the concrete form of over-declaration. Question 3.
	DeclaredNeverExercised []string

	// ExercisedNotDeclared is reach the agent used that the approved plan never
	// declared. It means the surface moved between approval and execution, and
	// is the case enforcement would have blocked. Question 4.
	ExercisedNotDeclared []string
}

// WouldDenyRate is WouldDeny over GatedCalls, or 0 when nothing was gated.
func (m Metric) WouldDenyRate() float64 {
	if m.GatedCalls == 0 {
		return 0
	}
	return float64(m.WouldDeny) / float64(m.GatedCalls)
}

// Metrics computes the dataset for one plan from its records.
//
// Records naming a different plan are ignored: a session that superseded once
// would otherwise report a rate blended across two different ceilings, which
// answers no question at all.
func Metrics(plan Plan, records []plangateaudit.Content) Metric {
	m := Metric{ApprovalsByTier: map[string]int{}}
	digest := plan.Digest()

	declared := map[string]struct{}{}
	for _, ph := range plan.Phases {
		for _, h := range ph.Permissions {
			declared[h.String()] = struct{}{}
		}
	}
	m.DeclaredHandles = len(declared)

	exercised := map[string]struct{}{}
	for _, r := range records {
		if r.PlanDigest != digest {
			continue
		}

		if r.Tier != "" {
			m.ApprovalsByTier[r.Tier]++
		}

		// A record with no handle is a call the gate does not govern.
		if r.Handle == "" {
			continue
		}
		m.GatedCalls++
		exercised[r.Handle] = struct{}{}
		if r.Outcome == plangateaudit.OutcomeWouldDeny {
			m.WouldDeny++
		}
	}
	m.ExercisedHandles = len(exercised)

	for h := range declared {
		if _, used := exercised[h]; !used {
			m.DeclaredNeverExercised = append(m.DeclaredNeverExercised, h)
		}
	}
	for h := range exercised {
		if _, ok := declared[h]; !ok {
			m.ExercisedNotDeclared = append(m.ExercisedNotDeclared, h)
		}
	}

	sortStrings(m.DeclaredNeverExercised)
	sortStrings(m.ExercisedNotDeclared)
	return m
}

// sortStrings keeps metric output deterministic — map iteration order would
// otherwise make two runs over identical records disagree.
func sortStrings(ss []string) { sort.Strings(ss) }
