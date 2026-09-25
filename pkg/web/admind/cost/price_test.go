package cost_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/admind/cost"
)

func TestDefaultPriceMap_HasBuiltinPrices(t *testing.T) {
	// The built-in table carries current list prices for the models this
	// platform runs, so every deployment gets cost estimates without an operator
	// authoring a catalog. The model catalog overlays on top of this base.
	pm := cost.DefaultPriceMap()

	assert.True(t, pm.Known("claude-opus-4-8"), "opus-4-8 is priced in the base")
	// 1M in / 1M out at 5/25 → $30.
	assert.InDelta(t, 30.00, float64(pm.Estimate("claude-opus-4-8", 1_000_000, 1_000_000)), 1e-9)
	// The planned OpenAI/Codex model is priced too.
	assert.True(t, pm.Known("gpt-5.3-codex"), "codex model is priced in the base")

	// A model absent from the base is unknown → NaN (not 0.0), so the UI can
	// render "NaN" rather than imply $0 of real spend.
	assert.False(t, pm.Known("no-such-model"))
	got := pm.Estimate("no-such-model", 1_000_000, 1_000_000)
	assert.True(t, math.IsNaN(float64(got)), "unknown model estimates to NaN")
}

func TestUSD_MarshalJSON(t *testing.T) {
	// The wire contract the admin UI depends on: a priced amount encodes as a
	// number, an unpriced (NaN) amount as null — never an encoder error, which
	// would 500 the whole /budget or /overview response.
	cases := []struct {
		name string
		in   cost.USD
		want string
	}{
		{name: "priced amount → number", in: cost.USD(30.5), want: "30.5"},
		{name: "zero → number", in: cost.USD(0), want: "0"},
		{name: "unpriced (NaN) → null", in: cost.USD(math.NaN()), want: "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			require.NoError(t, err, "USD must never fail to marshal")
			assert.Equal(t, tc.want, string(b))
		})
	}
}

func TestEstimate_NilReceiverIsNaN(t *testing.T) {
	// A nil / empty PriceMap must not price anything as $0 — it's unknown → NaN.
	var nilMap *cost.PriceMap
	assert.True(t, math.IsNaN(float64(nilMap.Estimate("claude-opus-4-8", 1, 1))))
	assert.True(t, math.IsNaN(float64((&cost.PriceMap{}).Estimate("claude-opus-4-8", 1, 1))))
}

func TestLoadPriceMap_FileOverride(t *testing.T) {
	// Write a JSON file that seeds a base for two models.
	override := map[string]cost.ModelPrice{
		"test-model-a": {InputPerMTok: 20.00, OutputPerMTok: 100.00},
		"test-model-b": {InputPerMTok: 0.50, OutputPerMTok: 2.00},
	}
	data, err := json.Marshal(override)
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	pm, err := cost.LoadPriceMap(path)
	require.NoError(t, err)

	assert.True(t, pm.Known("test-model-a"))
	got := float64(pm.Estimate("test-model-a", 1_000_000, 1_000_000))
	assert.InDelta(t, 120.00, got, 1e-9, "overridden model cost")

	assert.True(t, pm.Known("test-model-b"))
	got = float64(pm.Estimate("test-model-b", 1_000_000, 0))
	assert.InDelta(t, 0.50, got, 1e-9, "second model input cost")

	// A model absent from both the built-in defaults and the override file
	// stays unknown (test-model-c is not a built-in).
	assert.False(t, pm.Known("test-model-c"))
}

func TestLoadPriceMap_MissingFile(t *testing.T) {
	// Missing file → falls back to the built-in defaults, no error. test-model-a
	// is not a built-in, so it stays unknown.
	pm, err := cost.LoadPriceMap("/does/not/exist/prices.json")
	require.NoError(t, err)
	assert.False(t, pm.Known("test-model-a"))
}

func TestLoadPriceMap_EmptyPath(t *testing.T) {
	// Empty path → the built-in defaults, no error. test-model-a is not a
	// built-in, so it stays unknown.
	pm, err := cost.LoadPriceMap("")
	require.NoError(t, err)
	assert.False(t, pm.Known("test-model-a"))
}

func TestPriceMap_Overlay(t *testing.T) {
	base, err := cost.LoadPriceMap("")
	require.NoError(t, err)

	overridden := base.Overlay(map[string]cost.ModelPrice{
		"test-model-a": {InputPerMTok: 10, OutputPerMTok: 20},
	})

	// The base map is untouched by Overlay.
	assert.False(t, base.Known("test-model-a"), "base must be unchanged")

	// The overlaid map knows the new model.
	assert.True(t, overridden.Known("test-model-a"))
	got := float64(overridden.Estimate("test-model-a", 1_000_000, 1_000_000))
	assert.InDelta(t, 30.00, got, 1e-9)

	// A model in neither base nor overrides stays unknown → NaN.
	assert.False(t, overridden.Known("test-model-z"))
	assert.True(t, math.IsNaN(float64(overridden.Estimate("test-model-z", 1_000_000, 1_000_000))))

	// Overrides win over a pre-existing base price.
	baseWithPrice := base.Overlay(map[string]cost.ModelPrice{
		"test-model-a": {InputPerMTok: 1, OutputPerMTok: 1},
	})
	won := baseWithPrice.Overlay(map[string]cost.ModelPrice{
		"test-model-a": {InputPerMTok: 99, OutputPerMTok: 99},
	})
	got = float64(won.Estimate("test-model-a", 1_000_000, 0))
	assert.InDelta(t, 99.00, got, 1e-9, "later overlay overrides win")
}

func TestPriceMap_Overlay_NilReceiver(t *testing.T) {
	// A nil *PriceMap must not panic; Overlay should still return a usable map
	// built solely from the overrides.
	var nilMap *cost.PriceMap

	overridden := nilMap.Overlay(map[string]cost.ModelPrice{
		"test-model-a": {InputPerMTok: 10, OutputPerMTok: 20},
	})

	require.NotNil(t, overridden)
	assert.True(t, overridden.Known("test-model-a"))
	got := float64(overridden.Estimate("test-model-a", 1_000_000, 1_000_000))
	assert.InDelta(t, 30.00, got, 1e-9)
}
