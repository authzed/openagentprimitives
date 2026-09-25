// Package cost provides a per-model token price map and cost estimator for the
// admin dashboard. Prices are best-effort estimates labelled as such in the UI.
//
// There are three price sources, layered lowest-to-highest precedence:
//
//  1. The built-in defaultPrices table below, projected from the canonical
//     per-provider tables in pkg/x/llmpricing (Anthropic + OpenAI list-price
//     estimates). Display-only — it carries no credentials and no governance,
//     so every deployment gets sensible cost estimates out of the box without
//     an operator authoring anything.
//  2. A JSON file pointed to by ADMIND_PRICE_MAP_PATH (operator base override).
//  3. The model catalog (ClusterAgentSettings.spec.modelCatalog), overlaid at
//     request time by admind via PriceMap.Overlay — catalog prices win.
//
// A model with no price in any layer is *unknown*: Estimate returns NaN (not
// 0.0) so the dashboard can render "NaN" rather than imply $0 of real spend.
package cost

import (
	"encoding/json"
	"errors"
	"math"
	"os"

	"github.com/authzed/openagentprimitives/pkg/x/llmpricing"
)

// USD is a monetary amount in US dollars whose JSON encoding represents an
// unknown/unpriced value (NaN) as null. encoding/json cannot marshal NaN — it
// returns an error — so a plain float64 field carrying an unpriced Estimate
// would fail the whole response. USD makes "no price known" survive to the wire
// as null, which the admin UI renders as "NaN" instead of a misleading $0.00.
type USD float64

// MarshalJSON emits null for a NaN (unpriced) amount and the plain number
// otherwise. ±Inf can never arise from Estimate, but is likewise mapped to null
// to keep the encoder from erroring.
func (u USD) MarshalJSON() ([]byte, error) {
	f := float64(u)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return json.Marshal(f)
}

// ModelPrice holds the USD list prices per million tokens for a single model.
type ModelPrice struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// PriceMap is an immutable snapshot of per-model prices. Construct one via
// DefaultPriceMap or LoadPriceMap; the zero value is valid but empty (all
// models unknown).
type PriceMap struct {
	m map[string]ModelPrice
}

// defaultPrices holds current list-price estimates (USD per million tokens) for
// the models this platform runs or plans to run. These are best-effort display
// values, labelled "estimated" in the UI and overridable per-model via the
// model catalog. Every entry derives from the shared canonical tables
// (pkg/x/llmpricing) so admind holds no price literals of its own and cannot
// drift from the providers; add new models there, not here.
var defaultPrices = buildDefaultPrices()

func buildDefaultPrices() map[string]ModelPrice {
	m := map[string]ModelPrice{}
	// Project every provider table from the canonical llmpricing leaf down to
	// admind's input/output view. admind ignores cache buckets, so unmodeled
	// cache pricing (e.g. OpenAI) is irrelevant here.
	for _, table := range llmpricing.Tables() {
		for id, p := range table {
			m[id] = ModelPrice{InputPerMTok: p.InputPerMTok, OutputPerMTok: p.OutputPerMTok}
		}
	}
	return m
}

// DefaultPriceMap returns a PriceMap pre-populated with the built-in price table.
func DefaultPriceMap() *PriceMap {
	m := make(map[string]ModelPrice, len(defaultPrices))
	for k, v := range defaultPrices {
		m[k] = v
	}
	return &PriceMap{m: m}
}

// LoadPriceMap loads a PriceMap starting from defaults and merging the JSON
// file at path on top. An empty path or a missing file are treated as
// "no override" — the default map is returned without error. A file that
// exists but cannot be parsed returns an error.
func LoadPriceMap(path string) (*PriceMap, error) {
	pm := DefaultPriceMap()
	if path == "" {
		return pm, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pm, nil
		}
		return pm, err
	}

	var override map[string]ModelPrice
	if err := json.Unmarshal(data, &override); err != nil {
		return nil, err
	}
	for k, v := range override {
		pm.m[k] = v
	}
	return pm, nil
}

// Known reports whether the model has a price entry in the map.
func (p *PriceMap) Known(model string) bool {
	if p == nil || p.m == nil {
		return false
	}
	_, ok := p.m[model]
	return ok
}

// Overlay returns a new PriceMap with per-model overrides merged on top of p
// (overrides win). p is unchanged. Used to layer live catalog prices over the
// base (default table / JSON-override) map.
func (p *PriceMap) Overlay(overrides map[string]ModelPrice) *PriceMap {
	out := make(map[string]ModelPrice, len(overrides))
	if p != nil {
		out = make(map[string]ModelPrice, len(p.m)+len(overrides))
		for k, v := range p.m {
			out[k] = v
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return &PriceMap{m: out}
}

// Estimate returns the estimated USD cost for the given token counts. A model
// with no price in the map is *unknown* and yields NaN (not 0.0), so callers
// can surface "NaN" rather than imply $0 of real spend. NaN propagates through
// summation, so any unpriced model in an aggregate makes that aggregate NaN.
func (p *PriceMap) Estimate(model string, inTok, outTok int64) USD {
	if p == nil || p.m == nil {
		return USD(math.NaN())
	}
	price, ok := p.m[model]
	if !ok {
		return USD(math.NaN())
	}
	return USD(float64(inTok)/1e6*price.InputPerMTok + float64(outTok)/1e6*price.OutputPerMTok)
}
