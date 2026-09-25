package uigrant_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
)

func origins() []uigrant.Origin {
	return []uigrant.Origin{
		{Name: "crm", AppToolsEnabled: true, AppVisibleTools: []string{"list_leads", "advance_stage"}},
		{Name: "metrics", AppToolsEnabled: false, AppVisibleTools: []string{"query_series"}},
	}
}

func TestMaterializeIsTheFailClosedIntersection(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		// origins overrides the shared origins() fixture for cases that need
		// a different origin shape (a same-name collision, a realistically
		// prefixed tool name). Nil ⇒ origins().
		origins []uigrant.Origin
		granted []string
		want    []string
	}{
		{
			name:      "all three conditions met: callable",
			requested: []string{"list_leads"},
			granted:   []string{"list_leads"},
			want:      []string{"list_leads"},
		},
		{
			name:      "requested and granted but origin opt-in off: denied",
			requested: []string{"query_series"},
			granted:   []string{"query_series"},
			want:      nil,
		},
		{
			// granted names a DIFFERENT tool ("list_leads"), not "advance_stage",
			// so this row is denied by the per-item grantedSet check inside the
			// loop, not by any empty-input fast path. See task-4 review F6/F5:
			// the previous version used granted: nil here, which was actually
			// denied by an (now-removed) `len(granted) == 0` early return —
			// the grantedSet membership check was never exercised by this row.
			name:      "requested and origin permits but not granted: denied",
			requested: []string{"advance_stage"},
			granted:   []string{"list_leads"},
			want:      nil,
		},
		{
			name:      "granted and origin permits but UI never asked: denied",
			requested: nil,
			granted:   []string{"list_leads"},
			want:      nil,
		},
		{
			name:      "tool exists on no origin at all: denied",
			requested: []string{"ghost"},
			granted:   []string{"ghost"},
			want:      nil,
		},
		{
			name:      "nil everything: denied, no panic",
			requested: nil,
			granted:   nil,
			want:      nil,
		},
		{
			name:      "partial: only the fully-intersected tool survives",
			requested: []string{"list_leads", "advance_stage", "query_series"},
			granted:   []string{"list_leads", "query_series"},
			want:      []string{"list_leads"},
		},
		{
			// The grant vocabulary is the LLM-visible, origin-prefixed name
			// ("<ref>_<tool>"), not the bare upstream tool name — see
			// Materialize's doc comment (task-4 review F2). Pin it with a
			// realistic example so a future switch to bare names fails here
			// instead of silently matching zero keys in Loop.AppTools.
			name:      "LLM-visible prefixed tool name flows through unchanged: callable",
			requested: []string{"crm_list_leads"},
			origins: []uigrant.Origin{
				{Name: "crm", AppToolsEnabled: true, AppVisibleTools: []string{"crm_list_leads"}},
			},
			granted: []string{"crm_list_leads"},
			want:    []string{"crm_list_leads"},
		},
		{
			// Two origins claim the same tool name and disagree on app-tools
			// opt-in. The opted-out origin's veto must win rather than being
			// silently rescued by the other origin's opt-in (task-4 review
			// F1). Disabled origin listed first.
			name:      "two origins disagree on the same name, disabled origin first: denied",
			requested: []string{"delete_account"},
			origins: []uigrant.Origin{
				{Name: "crm-prod", AppToolsEnabled: false, AppVisibleTools: []string{"delete_account"}},
				{Name: "crm-sandbox", AppToolsEnabled: true, AppVisibleTools: []string{"delete_account"}},
			},
			granted: []string{"delete_account"},
			want:    nil,
		},
		{
			// Same collision, opposite slice order — the veto must be
			// order-independent, not an artifact of iterating the disabled
			// origin first.
			name:      "two origins disagree on the same name, enabled origin first: denied",
			requested: []string{"delete_account"},
			origins: []uigrant.Origin{
				{Name: "crm-sandbox", AppToolsEnabled: true, AppVisibleTools: []string{"delete_account"}},
				{Name: "crm-prod", AppToolsEnabled: false, AppVisibleTools: []string{"delete_account"}},
			},
			granted: []string{"delete_account"},
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.origins
			if o == nil {
				o = origins()
			}
			got := uigrant.Materialize(tc.requested, o, tc.granted)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMaterializeIsDeterministicallyOrdered(t *testing.T) {
	// Materialize's determinism matters for the runner's registry
	// construction. Ceiling (tested separately) shares this property for a
	// stronger reason: ITS result is what lands in AgentUI's
	// status.eligibleTools, an SSA-adjacent observation where unstable
	// ordering would rewrite the object on every reconcile.
	req := []string{"advance_stage", "list_leads"}
	grant := []string{"list_leads", "advance_stage"}
	first := uigrant.Materialize(req, origins(), grant)
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, uigrant.Materialize(req, origins(), grant),
			"Materialize must be deterministic; status churn otherwise")
	}
	assert.Equal(t, []string{"advance_stage", "list_leads"}, first, "sorted, not input order")
}

func TestExplainNamesTheFailingCondition(t *testing.T) {
	got := uigrant.Explain(
		[]string{"query_series", "advance_stage", "ghost", "list_leads"},
		origins(),
		[]string{"query_series", "ghost", "list_leads"},
	)
	assert.Contains(t, got["query_series"], "origin")
	assert.Contains(t, got["advance_stage"], "grant")
	assert.Contains(t, got["ghost"], "no origin")
	assert.NotContains(t, got, "list_leads", "a fully-granted tool needs no explanation")
}

// powerset returns every subset of items, including the empty set. Used to
// exhaustively sweep small requested/granted combinations in
// TestExplainAndMaterializeAgreeOnEveryRequestedTool.
func powerset(items []string) [][]string {
	out := [][]string{{}}
	for _, it := range items {
		n := len(out)
		for i := 0; i < n; i++ {
			next := make([]string, len(out[i]), len(out[i])+1)
			copy(next, out[i])
			out = append(out, append(next, it))
		}
	}
	return out
}

func TestExplainAndMaterializeAgreeOnEveryRequestedTool(t *testing.T) {
	// Invariant under test (task-4 review F7): for any requested/granted
	// combination, a tool Materialize kept has no Explain entry, and a tool
	// Materialize dropped has exactly one. Swept exhaustively over a small
	// tool universe rather than asserted for one hand-picked call, so a
	// future edit that lets the two functions disagree on some untested
	// combination is caught.
	universe := []string{"list_leads", "advance_stage", "query_series", "ghost"}
	subsets := powerset(universe)

	for _, requested := range subsets {
		for _, granted := range subsets {
			materialized := uigrant.Materialize(requested, origins(), granted)
			explained := uigrant.Explain(requested, origins(), granted)

			kept := map[string]struct{}{}
			for _, m := range materialized {
				kept[m] = struct{}{}
			}
			for _, r := range requested {
				_, wasKept := kept[r]
				_, hasExplanation := explained[r]
				if wasKept {
					assert.False(t, hasExplanation,
						"kept tool %q must have no Explain entry (requested=%v granted=%v)", r, requested, granted)
				} else {
					assert.True(t, hasExplanation,
						"dropped tool %q must have an Explain entry (requested=%v granted=%v)", r, requested, granted)
				}
			}
		}
	}
}

func TestAppVisibleVetoesOnOriginCollisionRegardlessOfOrder(t *testing.T) {
	// Direct unit test of the veto semantics behind the two "two origins
	// disagree" rows above, isolating appVisible's behavior from the grant
	// and request checks (task-4 review F1).
	enabled := uigrant.Origin{Name: "crm-sandbox", AppToolsEnabled: true, AppVisibleTools: []string{"delete_account"}}
	disabled := uigrant.Origin{Name: "crm-prod", AppToolsEnabled: false, AppVisibleTools: []string{"delete_account"}}

	for _, tc := range []struct {
		name    string
		origins []uigrant.Origin
	}{
		{name: "disabled origin first", origins: []uigrant.Origin{disabled, enabled}},
		{name: "enabled origin first", origins: []uigrant.Origin{enabled, disabled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uigrant.Materialize([]string{"delete_account"}, tc.origins, []string{"delete_account"})
			assert.Nil(t, got, "an opted-out origin's veto must win regardless of slice order")
		})
	}
}

func TestUnrequestedActionToolsNamesEveryActionToolTheUINeverAsked(t *testing.T) {
	cases := []struct {
		name       string
		requested  []string
		actionTool []string
		normalize  func(string) string
		want       []string
	}{
		{
			name:       "every action tool is already requested: nothing to report",
			requested:  []string{"list_leads", "advance_stage"},
			actionTool: []string{"list_leads", "advance_stage"},
			want:       nil,
		},
		{
			name:       "one action tool was never requested",
			requested:  []string{"list_leads"},
			actionTool: []string{"list_leads", "advance_stage"},
			want:       []string{"advance_stage"},
		},
		{
			// A vocabulary difference (underscore vs camelCase) that only
			// normalization reconciles — the exact shape a Binding.Ref carries,
			// per Options.NormalizeToolName's doc comment, since an
			// AgentUIAction.Tool can (pre-CRD-pattern objects) carry the same.
			name:       "a case/vocabulary difference that only normalization reconciles is NOT reported",
			requested:  []string{"crm_advancestage"},
			actionTool: []string{"crm_advanceStage"},
			normalize:  strings.ToLower,
			want:       nil,
		},
		{
			// Same pair, no normalize func: nil is the identity (never crashes),
			// but an un-normalized ref simply misses the requested set — see
			// Options.NormalizeToolName's doc comment for the same asymmetry on
			// the REJECT side.
			name:       "without normalization the SAME pair is reported",
			requested:  []string{"crm_advancestage"},
			actionTool: []string{"crm_advanceStage"},
			normalize:  nil,
			want:       []string{"crm_advanceStage"},
		},
		{
			name:       "nil everything: nothing to report, no panic",
			requested:  nil,
			actionTool: nil,
			want:       nil,
		},
		{
			name:       "a duplicate action tool is reported once",
			requested:  nil,
			actionTool: []string{"advance_stage", "advance_stage"},
			want:       []string{"advance_stage"},
		},
		{
			name:       "output is sorted, not input order",
			requested:  nil,
			actionTool: []string{"query_series", "advance_stage", "list_leads"},
			want:       []string{"advance_stage", "list_leads", "query_series"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uigrant.UnrequestedActionTools(tc.requested, tc.actionTool, tc.normalize)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCeilingIsTheTwoPartyIntersection(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		granted   []string
		want      []string
	}{
		{
			name:      "requested and granted: kept",
			requested: []string{"list_leads"},
			granted:   []string{"list_leads"},
			want:      []string{"list_leads"},
		},
		{
			// Ceiling has no origin condition to deny this on — unlike
			// Materialize, which would drop query_series because its origin
			// never opted in. Pinning this is the point: Ceiling is knowingly
			// looser than Materialize, never the other way around.
			name:      "requested and granted, no origin opinion at all: kept anyway",
			requested: []string{"query_series"},
			granted:   []string{"query_series"},
			want:      []string{"query_series"},
		},
		{
			name:      "requested but not granted: denied",
			requested: []string{"advance_stage"},
			granted:   []string{"list_leads"},
			want:      nil,
		},
		{
			name:      "granted but never requested: denied",
			requested: nil,
			granted:   []string{"list_leads"},
			want:      nil,
		},
		{
			name:      "nil everything: denied, no panic",
			requested: nil,
			granted:   nil,
			want:      nil,
		},
		{
			name:      "partial: only the intersected tool survives",
			requested: []string{"list_leads", "advance_stage", "query_series"},
			granted:   []string{"list_leads", "query_series"},
			want:      []string{"list_leads", "query_series"},
		},
		{
			name:      "duplicate requested entries dedupe",
			requested: []string{"list_leads", "list_leads"},
			granted:   []string{"list_leads"},
			want:      []string{"list_leads"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uigrant.Ceiling(tc.requested, tc.granted)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCeilingIsDeterministicallyOrdered(t *testing.T) {
	// Same rationale as TestMaterializeIsDeterministicallyOrdered, but this
	// is the function whose result actually lands in status.eligibleTools.
	req := []string{"advance_stage", "list_leads"}
	grant := []string{"list_leads", "advance_stage"}
	first := uigrant.Ceiling(req, grant)
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, uigrant.Ceiling(req, grant),
			"Ceiling must be deterministic; status churn otherwise")
	}
	assert.Equal(t, []string{"advance_stage", "list_leads"}, first, "sorted, not input order")
}

func TestExplainCeilingNamesOnlyTheGrantConditionItCanEvaluate(t *testing.T) {
	granted := []string{"list_leads"}
	got := uigrant.ExplainCeiling([]string{"list_leads", "advance_stage"}, granted)

	assert.NotContains(t, got, "list_leads", "a tool that made Ceiling's cut needs no explanation")
	require.Contains(t, got, "advance_stage")
	assert.Contains(t, got["advance_stage"], "deployment grant",
		"the only condition ExplainCeiling can evaluate is the deployment grant")
	assert.NotContains(t, got["advance_stage"], "origin",
		"ExplainCeiling must never attribute a miss to a condition it cannot evaluate")
}

func TestExplainCeilingAndCeilingAgreeOnEveryRequestedTool(t *testing.T) {
	// Same invariant as TestExplainAndMaterializeAgreeOnEveryRequestedTool,
	// swept over the two-party functions: a tool Ceiling kept has no
	// ExplainCeiling entry, and a tool Ceiling dropped has exactly one.
	universe := []string{"list_leads", "advance_stage", "query_series", "ghost"}
	subsets := powerset(universe)

	for _, requested := range subsets {
		for _, granted := range subsets {
			kept := map[string]struct{}{}
			for _, k := range uigrant.Ceiling(requested, granted) {
				kept[k] = struct{}{}
			}
			explained := uigrant.ExplainCeiling(requested, granted)
			for _, r := range requested {
				_, wasKept := kept[r]
				_, hasExplanation := explained[r]
				if wasKept {
					assert.False(t, hasExplanation,
						"kept tool %q must have no ExplainCeiling entry (requested=%v granted=%v)", r, requested, granted)
				} else {
					assert.True(t, hasExplanation,
						"dropped tool %q must have an ExplainCeiling entry (requested=%v granted=%v)", r, requested, granted)
				}
			}
		}
	}
}
