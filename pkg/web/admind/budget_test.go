package admind_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/cost"
)

// priceCatalogSettings builds a ClusterAgentSettings whose modelCatalog prices
// the two models used by the budget test fixtures. The catalog prices here
// (opus 15/75, sonnet 3/15) deliberately differ from the built-in defaults so
// the assertions prove the catalog overlay OVERRIDES the base, not just that
// some price exists.
func priceCatalogSettings() *spiceboxv1alpha1.ClusterAgentSettings {
	cs := &spiceboxv1alpha1.ClusterAgentSettings{}
	cs.Name = spiceboxv1alpha1.ClusterAgentSettingsName
	cs.Spec.ModelCatalog = &[]spiceboxv1alpha1.ModelCatalogEntry{
		{Name: "claude-opus-4-8", Provider: "anthropic", InputPerMTok: 15, OutputPerMTok: 75},
		{Name: "claude-sonnet-4-6", Provider: "anthropic", InputPerMTok: 3, OutputPerMTok: 15},
	}
	return cs
}

// budgetSession builds an AgentSession with a resolved model (stamped on
// status.effectiveSettings, the same field the aggregator reads) and an
// optional started-by-canonical-id annotation (empty starter => kubectl-driven,
// no annotation). Provider is set (the realistic shape — pkg/platform/settings/resolve.go
// sets Provider on essentially every resolution path) so these fixtures
// exercise the SAME uniform "<provider>/<model>" display id production code
// actually produces, rather than the unrealistic bare-model shape that let
// the provider-prefix/price-lookup regression hide.
func budgetSession(ns, name, class, provider, model, starter string, in, out int64) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace, s.Name = ns, name
	s.Spec.Class = class
	s.Status.Phase = "Running"
	s.Status.StartedAt = &metav1.Time{Time: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	s.Status.EffectiveSettings = &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{Provider: provider, Name: model},
	}
	s.Status.Progress = &spiceboxv1alpha1.AgentSessionProgress{InputTokens: in, OutputTokens: out}
	if starter != "" {
		s.Annotations = map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: starter}
	}
	return s
}

// budgetSessionWithBuckets builds a routed (OpenRouter-style) session whose
// status.estimatedCost.byModel carries the actually-served-model breakdown —
// the aggregator must attribute spend/tokens to EACH bucket's Model, not the
// session's blended configured Model (Provider/Name below).
func budgetSessionWithBuckets(ns, name, class, provider, model string, buckets []spiceboxv1alpha1.ModelCostBucket) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace, s.Name = ns, name
	s.Spec.Class = class
	s.Status.Phase = "Completed"
	s.Status.StartedAt = &metav1.Time{Time: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	s.Status.EffectiveSettings = &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{Provider: provider, Name: model},
	}
	var in, out int64
	for _, b := range buckets {
		in += b.InputTokens
		out += b.OutputTokens
	}
	s.Status.Progress = &spiceboxv1alpha1.AgentSessionProgress{InputTokens: in, OutputTokens: out}
	s.Status.EstimatedCost = &spiceboxv1alpha1.EstimatedSessionCost{ByModel: buckets}
	return s
}

// rowsByKey indexes a breakdown slice by row key for assertion.
func rowsByKey(rows []admind.BudgetRow) map[string]admind.BudgetRow {
	out := make(map[string]admind.BudgetRow, len(rows))
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}

func TestAdmindBudget(t *testing.T) {
	// Two models × two classes × two starters:
	//   s1: opus,   support-bot, alice,     in=1000 out=200
	//   s2: sonnet, support-bot, alice,     in=2000 out=500
	//   s3: sonnet, triage-bot,  (kubectl), in=500  out=100
	s1 := budgetSession("default", "s1", "support-bot", "anthropic", "claude-opus-4-8", "user:alice", 1000, 200)
	s2 := budgetSession("default", "s2", "support-bot", "anthropic", "claude-sonnet-4-6", "user:alice", 2000, 500)
	s3 := budgetSession("team", "s3", "triage-bot", "anthropic", "claude-sonnet-4-6", "", 500, 100)

	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(s1, s2, s3, priceCatalogSettings()).Build()
	a := newTestAdmind(t, k8s)
	for _, s := range []*spiceboxv1alpha1.AgentSession{s1, s2, s3} {
		a.Aggregator().UpsertSession(s)
	}
	h := a.Handler()

	// Per-session estimated cost, priced by ClusterAgentSettings.spec.modelCatalog
	// (opus 15/75, sonnet 3/15 USD per MTok) — the catalog overlay overrides the
	// built-in base prices for these two models:
	const (
		costS1 = 1000.0/1e6*15 + 200.0/1e6*75 // 0.0300
		costS2 = 2000.0/1e6*3 + 500.0/1e6*15  // 0.0135
		costS3 = 500.0/1e6*3 + 100.0/1e6*15   // 0.0030
		eps    = 1e-9
	)

	// Auth gate: budget needs view_overview — a non-admin subject is forbidden.
	w := do(t, h, http.MethodGet, "/admin/v1/budget", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "budget needs view_overview")

	w = do(t, h, http.MethodGet, "/admin/v1/budget", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var bd admind.BudgetBreakdown
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bd))
	assert.True(t, bd.Estimated, "costs are flagged estimated")

	// byModel: opus = s1; sonnet = s2 + s3. Keys are the uniform
	// "<provider>/<model>" display id (Task 9), NOT the bare catalog name —
	// effectivePrices' catalog overrides stay keyed bare internally, but
	// pricing must still resolve via bareModel() against this prefixed key.
	model := rowsByKey(bd.ByModel)
	require.Len(t, bd.ByModel, 2)
	assert.Equal(t, int64(1000), model["anthropic/claude-opus-4-8"].InputTokens)
	assert.Equal(t, int64(200), model["anthropic/claude-opus-4-8"].OutputTokens)
	assert.InDelta(t, costS1, float64(model["anthropic/claude-opus-4-8"].EstimatedCostUSD), eps)
	assert.Equal(t, int64(2500), model["anthropic/claude-sonnet-4-6"].InputTokens)
	assert.Equal(t, int64(600), model["anthropic/claude-sonnet-4-6"].OutputTokens)
	assert.InDelta(t, costS2+costS3, float64(model["anthropic/claude-sonnet-4-6"].EstimatedCostUSD), eps)
	// Sorted by cost desc: opus (0.030) outranks sonnet (0.0165).
	assert.Equal(t, "anthropic/claude-opus-4-8", bd.ByModel[0].Key, "byModel sorted by cost desc")

	// byAgentClass: support-bot = s1 + s2; triage-bot = s3.
	class := rowsByKey(bd.ByAgentClass)
	require.Len(t, bd.ByAgentClass, 2)
	assert.Equal(t, int64(3000), class["support-bot"].InputTokens)
	assert.Equal(t, int64(700), class["support-bot"].OutputTokens)
	assert.InDelta(t, costS1+costS2, float64(class["support-bot"].EstimatedCostUSD), eps)
	assert.InDelta(t, costS3, float64(class["triage-bot"].EstimatedCostUSD), eps)
	assert.Equal(t, "support-bot", bd.ByAgentClass[0].Key, "byAgentClass sorted by cost desc")

	// bySession: one row per session, keyed ns/name.
	sess := rowsByKey(bd.BySession)
	require.Len(t, bd.BySession, 3)
	assert.InDelta(t, costS1, float64(sess["default/s1"].EstimatedCostUSD), eps)
	assert.InDelta(t, costS2, float64(sess["default/s2"].EstimatedCostUSD), eps)
	assert.InDelta(t, costS3, float64(sess["team/s3"].EstimatedCostUSD), eps)

	// byUser: alice = s1 + s2 (from the started-by annotation); s3 has no
	// starter → "(unknown)".
	user := rowsByKey(bd.ByUser)
	require.Len(t, bd.ByUser, 2)
	assert.Equal(t, int64(3000), user["user:alice"].InputTokens)
	assert.InDelta(t, costS1+costS2, float64(user["user:alice"].EstimatedCostUSD), eps)
	assert.InDelta(t, costS3, float64(user["(unknown)"].EstimatedCostUSD), eps)
	assert.Equal(t, "user:alice", bd.ByUser[0].Key, "byUser sorted by cost desc")
}

// TestAdmindBudget_CatalogPriceOverridesBase proves effectivePrices layers the
// live model-catalog price OVER a pre-existing base price for the same model
// (not just filling gaps for unpriced models) — a base seeded via
// ADMIND_PRICE_MAP_PATH prices "test-model-a" at 1/1, and the catalog re-prices
// it at 10/20; the estimate must reflect the catalog price.
func TestAdmindBudget_CatalogPriceOverridesBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	base := map[string]cost.ModelPrice{"test-model-a": {InputPerMTok: 1, OutputPerMTok: 1}}
	data, err := json.Marshal(base)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	t.Setenv("ADMIND_PRICE_MAP_PATH", path)

	cs := &spiceboxv1alpha1.ClusterAgentSettings{}
	cs.Name = spiceboxv1alpha1.ClusterAgentSettingsName
	cs.Spec.ModelCatalog = &[]spiceboxv1alpha1.ModelCatalogEntry{
		{Name: "test-model-a", Provider: "test", InputPerMTok: 10, OutputPerMTok: 20},
	}

	sess := budgetSession("default", "s1", "support-bot", "test", "test-model-a", "", 1_000_000, 1_000_000)
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess, cs).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)
	h := a.Handler()

	w := do(t, h, http.MethodGet, "/admin/v1/budget", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var bd admind.BudgetBreakdown
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bd))

	// byModel key is the uniform "test/test-model-a" display id; the catalog
	// override map stays keyed by the bare "test-model-a" (ModelCatalogEntry.Name)
	// internally, resolved via bareModel() at the Estimate call site.
	model := rowsByKey(bd.ByModel)
	require.Contains(t, model, "test/test-model-a")
	// Catalog price (10 in / 20 out) wins over the base (1/1):
	// 1M*10/1M + 1M*20/1M = 30.
	assert.InDelta(t, 30.0, float64(model["test/test-model-a"].EstimatedCostUSD), 1e-9,
		"catalog price overrides the base ADMIND_PRICE_MAP_PATH price")
}

// TestAdmindBudget_ByModelBuckets proves the byModel axis attributes spend to
// the ACTUALLY-served model (Task 8's status.estimatedCost.byModel), not the
// session's blended configured model: a routed session with two priced
// buckets contributes a row per bucket — using each bucket's OWN priced
// AmountMicroUSD, not a re-estimate — and never a row for its own blended
// "openrouter/auto". A session with no buckets keeps today's behavior. The
// other three axes (byAgentClass/bySession/byUser) are unaffected: they still
// attribute the routed session's whole-session estimate.
func TestAdmindBudget_ByModelBuckets(t *testing.T) {
	buckets := []spiceboxv1alpha1.ModelCostBucket{
		{Model: "openrouter/anthropic/claude-3.5-sonnet", InputTokens: 700, OutputTokens: 300, AmountMicroUSD: 12_000, PricingKnown: true},
		{Model: "openrouter/openai/gpt-5", InputTokens: 300, OutputTokens: 100, AmountMicroUSD: 5_000, PricingKnown: true},
	}
	routed := budgetSessionWithBuckets("default", "s-routed", "router-bot", "openrouter", "auto", buckets)
	direct := budgetSession("default", "s-direct", "direct-bot", "anthropic", "claude-opus-4-8", "", 1000, 200)

	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(routed, direct).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(routed)
	a.Aggregator().UpsertSession(direct)
	h := a.Handler()

	w := do(t, h, http.MethodGet, "/admin/v1/budget", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var bd admind.BudgetBreakdown
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bd))

	model := rowsByKey(bd.ByModel)
	require.Len(t, bd.ByModel, 3, "2 served-model buckets + 1 direct-session row, never a 4th blended row")

	_, ok := model["openrouter/auto"]
	assert.False(t, ok, "blended configured-model row must NOT appear when buckets are present")

	require.Contains(t, model, "openrouter/anthropic/claude-3.5-sonnet")
	sonnet := model["openrouter/anthropic/claude-3.5-sonnet"]
	assert.Equal(t, int64(700), sonnet.InputTokens)
	assert.Equal(t, int64(300), sonnet.OutputTokens)
	assert.InDelta(t, 0.012, float64(sonnet.EstimatedCostUSD), 1e-9, "bucket's own priced AmountMicroUSD, not a re-estimate")

	require.Contains(t, model, "openrouter/openai/gpt-5")
	gptRow := model["openrouter/openai/gpt-5"]
	assert.Equal(t, int64(300), gptRow.InputTokens)
	assert.Equal(t, int64(100), gptRow.OutputTokens)
	assert.InDelta(t, 0.005, float64(gptRow.EstimatedCostUSD), 1e-9)

	require.Contains(t, model, "anthropic/claude-opus-4-8", "session with no buckets falls back to its own Model")
	assert.Equal(t, int64(1000), model["anthropic/claude-opus-4-8"].InputTokens)

	// byAgentClass/bySession still attribute the routed session's WHOLE totals
	// as one row, unaffected by the per-model split.
	class := rowsByKey(bd.ByAgentClass)
	require.Contains(t, class, "router-bot")
	assert.Equal(t, int64(1000), class["router-bot"].InputTokens, "700+300 whole-session total")
	assert.Equal(t, int64(400), class["router-bot"].OutputTokens, "300+100 whole-session total")

	sess := rowsByKey(bd.BySession)
	require.Contains(t, sess, "default/s-routed")
	assert.Equal(t, int64(1000), sess["default/s-routed"].InputTokens)
}

// TestAdmindBudget_ProviderPrefixedModel_PricesAgainstBareID is the
// regression test for the display-id/price-lookup split: PriceMap.Estimate
// does an EXACT map-key lookup against BARE model ids (the built-in
// llmpricing tables AND the catalog overrides are both keyed bare), but
// SessionState.Model is the uniform "<provider>/<model>" display id (Task 9).
// A session with Provider set — the REALISTIC shape; pkg/platform/settings/resolve.go
// sets Provider on essentially every model-resolution path — must still
// resolve to a real, non-NaN estimate on every axis touched by
// prices.Estimate: byAgentClass/bySession (budget.go's shared `est`) and the
// Overview handler's aggregate (handlers.go). Deliberately uses NO
// ClusterAgentSettings catalog, so it exercises the built-in llmpricing
// table directly and can't hide behind catalog wiring.
func TestAdmindBudget_ProviderPrefixedModel_PricesAgainstBareID(t *testing.T) {
	// claude-opus-4-8's built-in price (pkg/agent/llm/models.Anthropic) is
	// $5/$25 per MTok; 1M in + 1M out -> $5 + $25 = $30.00.
	const wantCost = 1_000_000.0/1e6*5 + 1_000_000.0/1e6*25

	sess := budgetSession("default", "s1", "support-bot", "anthropic", "claude-opus-4-8", "user:alice", 1_000_000, 1_000_000)
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)
	h := a.Handler()

	// --- Budget: byModel/byAgentClass/bySession ---
	w := do(t, h, http.MethodGet, "/admin/v1/budget", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.NotContains(t, body, `"estimatedCostUSD":null`,
		"a Provider-set, table-priced session must never estimate as unknown (regression: prefixed Model broke the bare-keyed price lookup)")

	var bd admind.BudgetBreakdown
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bd))

	model := rowsByKey(bd.ByModel)
	require.Contains(t, model, "anthropic/claude-opus-4-8", "byModel key stays the uniform display id")
	assert.InDelta(t, wantCost, float64(model["anthropic/claude-opus-4-8"].EstimatedCostUSD), 1e-9)

	class := rowsByKey(bd.ByAgentClass)
	require.Contains(t, class, "support-bot")
	assert.InDelta(t, wantCost, float64(class["support-bot"].EstimatedCostUSD), 1e-9,
		"byAgentClass must price against the BARE model id, not the \"anthropic/...\" display id")

	sessRows := rowsByKey(bd.BySession)
	require.Contains(t, sessRows, "default/s1")
	assert.InDelta(t, wantCost, float64(sessRows["default/s1"].EstimatedCostUSD), 1e-9,
		"bySession must price against the bare model id")

	// --- Overview: the handler's aggregate estCost sum over ov.ByModel ---
	w = do(t, h, http.MethodGet, "/admin/v1/overview", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	ovBody := w.Body.String()
	assert.NotContains(t, ovBody, `"estimatedCostUSD":null`,
		"Overview's budget estimate must never go unknown for a table-priced Provider-set model")

	var ov struct {
		Budget struct {
			EstimatedCostUSD float64 `json:"estimatedCostUSD"`
		} `json:"budget"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ov))
	assert.InDelta(t, wantCost, ov.Budget.EstimatedCostUSD, 1e-9,
		"Overview's budget.estimatedCostUSD must price against the bare model id")
}

// rawCostRow mirrors BudgetRow but decodes EstimatedCostUSD as *float64 so a
// wire "null" (cost.USD's NaN encoding) round-trips as a nil pointer instead
// of silently zeroing out — unmarshaling JSON null into a plain float64 field
// leaves it at its zero value, which is indistinguishable from a genuine
// $0.00 and would make an unknown-price regression invisible to this test.
type rawCostRow struct {
	Key              string   `json:"key"`
	EstimatedCostUSD *float64 `json:"estimatedCostUSD"`
}

func rawRowsByKey(rows []rawCostRow) map[string]*float64 {
	out := make(map[string]*float64, len(rows))
	for _, r := range rows {
		out[r.Key] = r.EstimatedCostUSD
	}
	return out
}

// TestAdmindBudget_OpenRouterAutoModel_UnknownNotZero is the FIX A regression
// pin: "openrouter/auto" carries the zero Pricing value in
// pkg/agent/llm/models (its real cost is per-response provider-reported, not
// fixed — see pkg/x/llmpricing.projectPricing), so a session resolved to it
// with NO per-model cost buckets stamped (status.estimatedCost.byModel empty)
// must estimate as UNKNOWN (NaN, serialized as JSON null) on every axis
// prices.Estimate touches, never a fabricated $0.00. Deliberately uses NO
// ClusterAgentSettings catalog override, so it exercises the built-in
// llmpricing table directly (mirrors
// TestAdmindBudget_ProviderPrefixedModel_PricesAgainstBareID's harness).
func TestAdmindBudget_OpenRouterAutoModel_UnknownNotZero(t *testing.T) {
	sess := budgetSession("default", "s1", "router-bot", "openrouter", "openrouter/auto", "user:alice", 1000, 200)
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(sess)
	h := a.Handler()

	w := do(t, h, http.MethodGet, "/admin/v1/budget", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.NotContains(t, body, `"estimatedCostUSD":0`,
		"an unpriced routed model must never estimate as a fabricated $0.00")
	assert.Contains(t, body, `"estimatedCostUSD":null`,
		"an unpriced routed model must serialize as unknown (null), not omitted")

	var bd struct {
		ByModel      []rawCostRow `json:"byModel"`
		ByAgentClass []rawCostRow `json:"byAgentClass"`
		BySession    []rawCostRow `json:"bySession"`
		ByUser       []rawCostRow `json:"byUser"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &bd))

	model := rawRowsByKey(bd.ByModel)
	require.Contains(t, model, "openrouter/openrouter/auto")
	assert.Nil(t, model["openrouter/openrouter/auto"], "byModel must be unknown (null), not a fabricated $0")

	class := rawRowsByKey(bd.ByAgentClass)
	require.Contains(t, class, "router-bot")
	assert.Nil(t, class["router-bot"], "byAgentClass must be unknown (null), not a fabricated $0")

	sessRows := rawRowsByKey(bd.BySession)
	require.Contains(t, sessRows, "default/s1")
	assert.Nil(t, sessRows["default/s1"], "bySession must be unknown (null), not a fabricated $0")

	user := rawRowsByKey(bd.ByUser)
	require.Contains(t, user, "user:alice")
	assert.Nil(t, user["user:alice"], "byUser must be unknown (null), not a fabricated $0")

	// --- Overview: the handler's aggregate estCost sum over ov.ByModel ---
	w = do(t, h, http.MethodGet, "/admin/v1/overview", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	ovBody := w.Body.String()
	assert.NotContains(t, ovBody, `"estimatedCostUSD":0`,
		"Overview's budget estimate must never fabricate a $0 for an unpriced routed model")
	assert.Contains(t, ovBody, `"estimatedCostUSD":null`,
		"Overview's budget estimate must serialize as unknown (null) for an unpriced routed model")
}

func TestAdmindBudget_EmptyIsNonNilSlices(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/budget", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	// Every axis must serialize as [] (not null) with no tracked sessions.
	body := w.Body.String()
	for _, axis := range []string{"byModel", "byAgentClass", "bySession", "byUser"} {
		assert.Contains(t, body, "\""+axis+"\":[]", axis+" must be an empty array, not null")
	}
}
