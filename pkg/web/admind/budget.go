package admind

import (
	"context"
	"math"
	"net/http"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/cost"
)

// BudgetRow is one aggregated token/cost rollup along one breakdown axis.
type BudgetRow struct {
	// Key is the axis value — a model name, AgentClass, "ns/name" session key,
	// or starter subject; budgetUnknownKey when the axis value was absent.
	Key string `json:"key"`
	// InputTokens is the summed prompt tokens across this key's sessions.
	InputTokens int64 `json:"inputTokens"`
	// OutputTokens is the summed completion tokens across this key's sessions.
	OutputTokens int64 `json:"outputTokens"`
	// EstimatedCostUSD is a LIST-PRICE estimate, never a metered bill; NaN
	// when any contributing spend had no known price.
	EstimatedCostUSD cost.USD `json:"estimatedCostUSD"`
}

// BudgetBreakdown is the GET /admin/v1/budget payload: token and
// estimated-cost rollups along four axes. Every axis is a non-nil slice, so an
// empty one serializes as [] rather than null.
type BudgetBreakdown struct {
	// ByModel splits an auto-routed session across the models it actually used.
	ByModel []BudgetRow `json:"byModel"`
	// ByAgentClass rolls up per AgentClass.
	ByAgentClass []BudgetRow `json:"byAgentClass"`
	// BySession rolls up per "ns/name" session key.
	BySession []BudgetRow `json:"bySession"`
	// ByUser rolls up per starter; EMPTY when the starter lookup failed, never
	// fabricated from sessions whose starter is merely unread.
	ByUser []BudgetRow `json:"byUser"`
	// Estimated is always true — costs are list prices, and the UI must label
	// them as estimates.
	Estimated bool `json:"estimated"`
}

// budgetUnknownKey labels rows whose axis value is absent — an unresolved model,
// a session with no AgentClass, or a session with no recorded starter.
const budgetUnknownKey = "(unknown)"

// budgetSession is one live session paired with the per-request facts every
// axis is priced against, so a dimension's Contribute needs nothing else.
type budgetSession struct {
	// State is the aggregator's live snapshot of the session.
	State SessionState
	// Est is the whole-session cost estimate, computed once per session
	// against its own model — see handleBudget for why pricing happens before
	// grouping, not after.
	Est cost.USD
	// Starters maps "ns/name" to the session's started-by canonical subject.
	// nil means the lookup FAILED (not "no starters"), which is why the byUser
	// axis contributes nothing rather than a fabricated "(unknown)" row.
	Starters map[string]string
}

// budgetDimension is one breakdown axis of the GET /admin/v1/budget payload.
// The axes differ only in which rows a session contributes and where the
// finished rows land, so a new axis is one entry in budgetDimensions plus its
// field on BudgetBreakdown — no new accumulator, no second sort, no third emit
// site.
type budgetDimension struct {
	// Name is the axis's identity, matching the JSON field Assign fills. It
	// keys the accumulator map in handleBudget, so it must be unique.
	Name string
	// Contribute reports this session's rows for the axis, calling emit once
	// per row. Most axes emit exactly one row; byModel emits one per
	// served-model bucket, and byUser emits none when the starter lookup
	// failed. A row's tokens and cost are the axis's own, not the session's:
	// the per-bucket axis carries each bucket's already-priced spend.
	Contribute func(s budgetSession, emit func(key string, inTok, outTok int64, c cost.USD))
	// Assign stores the axis's finished, sorted rows on the response.
	Assign func(b *BudgetBreakdown, rows []BudgetRow)
}

// budgetDimensions is the registry of breakdown axes. Registration is this
// entry: nothing dispatches on an axis name, and handleBudget runs one
// accumulate-and-emit loop over whatever is here.
var budgetDimensions = []budgetDimension{
	{
		// byModel is the one axis with a second path: a session carrying
		// per-served-model buckets (status.estimatedCost.byModel) attributes
		// its spend to EACH bucket's own Model using that bucket's own
		// already-priced amount, so an OpenRouter session that routed or fell
		// back across models shows where the spend actually went instead of one
		// blended total. Exactly one of the two branches runs per session, so
		// tokens and spend are never double-counted.
		Name: "byModel",
		Contribute: func(s budgetSession, emit func(string, int64, int64, cost.USD)) {
			if len(s.State.ByModel) > 0 {
				for _, b := range s.State.ByModel {
					emit(budgetKeyOrUnknown(b.Model), b.InputTokens, b.OutputTokens, bucketCostUSD(b))
				}
				return
			}
			emit(budgetKeyOrUnknown(s.State.Model), s.State.InputTokens, s.State.OutputTokens, s.Est)
		},
		Assign: func(b *BudgetBreakdown, rows []BudgetRow) { b.ByModel = rows },
	},
	{
		Name: "byAgentClass",
		Contribute: func(s budgetSession, emit func(string, int64, int64, cost.USD)) {
			emit(budgetKeyOrUnknown(s.State.Class), s.State.InputTokens, s.State.OutputTokens, s.Est)
		},
		Assign: func(b *BudgetBreakdown, rows []BudgetRow) { b.ByAgentClass = rows },
	},
	{
		Name: "bySession",
		Contribute: func(s budgetSession, emit func(string, int64, int64, cost.USD)) {
			emit(sessionKey(s.State), s.State.InputTokens, s.State.OutputTokens, s.Est)
		},
		Assign: func(b *BudgetBreakdown, rows []BudgetRow) { b.BySession = rows },
	},
	{
		// byUser contributes only when the starter source is available: on a
		// List error Starters is nil and the axis stays empty ([]), never
		// fabricated from sessions whose starter is merely unread.
		Name: "byUser",
		Contribute: func(s budgetSession, emit func(string, int64, int64, cost.USD)) {
			if s.Starters == nil {
				return
			}
			emit(budgetKeyOrUnknown(s.Starters[sessionKey(s.State)]),
				s.State.InputTokens, s.State.OutputTokens, s.Est)
		},
		Assign: func(b *BudgetBreakdown, rows []BudgetRow) { b.ByUser = rows },
	},
}

// sessionKey is the "ns/name" identity a session is grouped and looked up by.
func sessionKey(s SessionState) string { return s.Namespace + "/" + s.Name }

// budgetKeyOrUnknown labels an absent axis value so it groups into one visible
// row instead of an empty-string key.
func budgetKeyOrUnknown(key string) string {
	if key == "" {
		return budgetUnknownKey
	}
	return key
}

// effectivePrices layers the live model-catalog prices (ClusterAgentSettings.
// spec.modelCatalog) over the base price map (a.prices). Catalog prices win;
// models with no catalog price fall through to the base (typically empty →
// "est. (no price)"). A missing/errored catalog read degrades to the base
// (logged, except NotFound which is normal) and never fails the caller.
func (a *Admind) effectivePrices(ctx context.Context) *cost.PriceMap {
	var cs spiceboxv1alpha1.ClusterAgentSettings
	if err := a.cfg.K8s.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cs); err != nil {
		if !apierrors.IsNotFound(err) {
			a.cfg.Logger.Info("admind: catalog price overlay skipped; get cluster settings failed", "err", err.Error())
		}
		return a.prices
	}
	if cs.Spec.ModelCatalog == nil {
		return a.prices
	}
	overrides := map[string]cost.ModelPrice{}
	for _, e := range *cs.Spec.ModelCatalog {
		if e.HasPrice() {
			overrides[e.Name] = cost.ModelPrice{InputPerMTok: e.InputPerMTok, OutputPerMTok: e.OutputPerMTok}
		}
	}
	if len(overrides) == 0 {
		return a.prices
	}
	return a.prices.Overlay(overrides)
}

// handleBudget computes cost/token breakdowns across every registered axis
// (budgetDimensions) from the live tracked-session snapshot — the SAME scope as
// the Overview budget panel, so the two views are consistent (both read
// a.agg.Snapshot()).
//
// Cost is computed PER SESSION and then grouped, which is accurate because a
// session's resolved model is constant: the effective (catalog-overlaid) prices
// from effectivePrices are computed once per handler invocation, then
// Estimate is evaluated once per session against that session's model and
// summed into every axis it belongs to. Grouping first and pricing the
// aggregate would be wrong for byAgentClass / byUser rows that span multiple
// models.
func (a *Admind) handleBudget(w http.ResponseWriter, r *http.Request) {
	// The live SessionState does not carry the session's creating user, so List
	// the AgentSessions and index each one's started-by canonical subject by
	// "ns/name". Best-effort: a List error degrades byUser to an empty slice
	// (logged) rather than failing the whole breakdown — the other three axes
	// need no API call. starters==nil signals "no starter data" downstream.
	starters, err := a.sessionStarters(r.Context())
	if err != nil {
		a.cfg.Logger.Info("admind: budget starter lookup failed; byUser degraded to empty",
			"err", err.Error())
	}

	// One accumulator per registered axis, keyed by the axis name.
	accs := make(map[string]map[string]*BudgetRow, len(budgetDimensions))
	for _, d := range budgetDimensions {
		accs[d.Name] = map[string]*BudgetRow{}
	}

	prices := a.effectivePrices(r.Context())
	for _, s := range a.agg.Snapshot() {
		// s.Model is the uniform "<provider>/<model>" display id;
		// PriceMap.Estimate does an exact map-key lookup against BARE model ids
		// (the built-in tables and the catalog overrides are both keyed bare),
		// so it must be stripped of its provider prefix before pricing.
		bs := budgetSession{
			State:    s,
			Est:      prices.Estimate(bareModel(s.Model), s.InputTokens, s.OutputTokens) + toolCostUSD(s.ByTool),
			Starters: starters,
		}
		for _, d := range budgetDimensions {
			acc := accs[d.Name]
			d.Contribute(bs, func(key string, inTok, outTok int64, c cost.USD) {
				addBudgetRow(acc, key, inTok, outTok, c)
			})
		}
	}

	breakdown := BudgetBreakdown{Estimated: true}
	for _, d := range budgetDimensions {
		d.Assign(&breakdown, sortedBudgetRows(accs[d.Name]))
	}
	writeJSON(w, http.StatusOK, breakdown)
}

// addBudgetRow folds one contribution into an axis's accumulator, creating the
// row on first sight of its key.
func addBudgetRow(acc map[string]*BudgetRow, key string, inTok, outTok int64, c cost.USD) {
	row := acc[key]
	if row == nil {
		row = &BudgetRow{Key: key}
		acc[key] = row
	}
	row.InputTokens += inTok
	row.OutputTokens += outTok
	row.EstimatedCostUSD += c
}

// bucketCostUSD converts one ModelCostBucket's micro-USD amount to the
// admin-dashboard USD type, preserving the "unknown price never shows as a
// fabricated $0" contract: an unpriced bucket (PricingKnown=false) yields
// NaN, which — same as PriceMap.Estimate — propagates through the row's
// running sum, so a byModel row with any unpriced bucket surfaces as unknown
// rather than understating real spend.
func bucketCostUSD(b spiceboxv1alpha1.ModelCostBucket) cost.USD {
	if !b.PricingKnown {
		return cost.USD(math.NaN())
	}
	return cost.USD(float64(b.AmountMicroUSD) / 1e6)
}

// toolCostUSD sums a session's provider-reported interactive-toolkit spend.
// Nil/empty -> 0. Tool cost is always priced, so this never yields NaN.
func toolCostUSD(buckets []spiceboxv1alpha1.ToolCostBucket) cost.USD {
	var micro int64
	for _, b := range buckets {
		micro += b.AmountMicroUSD
	}
	return cost.USD(float64(micro) / 1e6)
}

// sortedBudgetRows flattens the accumulator into a non-nil slice ordered by
// estimated cost desc, key asc as the tie-break — a stable dashboard order and
// a JSON [] (never null) when empty.
func sortedBudgetRows(m map[string]*BudgetRow) []BudgetRow {
	out := make([]BudgetRow, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		ci, cj := float64(out[i].EstimatedCostUSD), float64(out[j].EstimatedCostUSD)
		// Unpriced rows (NaN cost) sort to the bottom; among equal costs (or two
		// unpriced rows) the key asc is the stable tie-break. Comparing NaN with
		// > directly is always false, which would skip the tie-break, so branch
		// on IsNaN explicitly.
		ni, nj := math.IsNaN(ci), math.IsNaN(cj)
		if ni != nj {
			return nj // a priced row precedes an unpriced one
		}
		if !ni && ci != cj {
			return ci > cj
		}
		return out[i].Key < out[j].Key
	})
	return out
}
