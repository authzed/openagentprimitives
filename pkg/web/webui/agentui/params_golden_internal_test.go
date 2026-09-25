package agentui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// goldenParamsPath is the ONE artifact both halves of the current-parameter
// mirror (J5) read. uicomponents.ParamStates answers "what is a control's
// current binding-parameter key and value" for read_view; @ap/agentui's
// collectDefaultParams answers the identical question for the browser's
// initial parameter map — both from the SAME declaration, walked by the
// SAME rule (a control's registered ParamProp/ParamValues), independently
// implemented per language.
//
// A literal written into each language's own test is NOT a pin: this branch
// has already shipped that exact gap twice (the bootstrap-props seam and the
// BindingPath mirror — see the standing agent-ui briefing's §3). With this
// file in place there is no edit to one side alone that keeps both suites
// green: change ParamStates' expansion rule and TestParamStatesMatchTheGoldenTheBrowserSeeds
// fails; regenerate this file to match and ui/paramsGolden.test.tsx fails
// instead, because it re-derives its expectation through collectDefaultParams
// applied to the golden's OWN declaration, not by echoing golden.parameters
// back at itself.
//
// To change the contract deliberately: update walk.go's ParamStates/
// collectParamKeys, this file's expectation, the golden, AND
// web/packages/agentui/src/params.tsx's collectDefaultParams together.
const goldenParamsPath = "ui/testdata/params.golden.json"

// goldenParams is the golden's own shape. Both fields stay json.RawMessage:
// this file compares uicomponents.ParamStates' marshaled output against the
// file's bytes, and decoding either through a Go struct on the way would
// launder exactly the tag renames the comparison exists to catch.
type goldenParams struct {
	Declaration json.RawMessage `json:"declaration"`
	Parameters  json.RawMessage `json:"parameters"`
}

func readParamsGolden(t *testing.T) goldenParams {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(goldenParamsPath))
	require.NoError(t, err, "the golden the frontend suite renders must exist")
	var g goldenParams
	require.NoError(t, json.Unmarshal(raw, &g), "the golden must be well-formed JSON")
	require.NotEmpty(t, g.Parameters, "a golden with no parameters would pin nothing")
	return g
}

// TestParamStatesMatchTheGoldenTheBrowserSeeds pins the Go↔TS
// current-parameter mirror. uicomponents.ParamStates answers it for
// read_view; web/packages/agentui/src/params.tsx's collectDefaultParams
// answers it for the browser's parameter map, from the SAME declaration.
// They must agree or read_view confidently tells the agent the dashboard is
// showing a span it is not.
//
// The pin is this one FILE, read by both suites — see
// ui/paramsGolden.test.tsx. A literal in each language's own test is not a
// pin: a coordinated format change left both suites green with every binding
// stuck on placeholders once already on this branch (the BindingPath
// mirror); this is the same gap for ParamStates.
func TestParamStatesMatchTheGoldenTheBrowserSeeds(t *testing.T) {
	g := readParamsGolden(t)

	decl, err := uicomponents.ParseDeclaration(g.Declaration)
	require.NoError(t, err, "the golden's declaration must parse")

	got, err := json.Marshal(uicomponents.ParamStates(decl))
	require.NoError(t, err, "ParamStates' output must marshal")

	assert.JSONEq(t, string(g.Parameters), string(got),
		"ParamStates' output changed: update walk.go, %s, and @ap/agentui's collectDefaultParams together", goldenParamsPath)
}

// TestParamsGoldenCarriesEveryValue guards the standing briefing's omitempty
// trap: a golden regenerated after a field quietly picked up `omitempty`
// still LOOKS like a pin if every value in the fixture happened to already be
// non-empty. Every parameter this golden carries has a declared, non-empty
// value (see the fixture's own comment in ParamStates' doc), so this asserts
// the "value" key is not merely equal but PRESENT, decoding into a pointer so
// an absent key and an empty string are distinguishable.
func TestParamsGoldenCarriesEveryValue(t *testing.T) {
	g := readParamsGolden(t)

	var params []struct {
		Key   string  `json:"key"`
		Value *string `json:"value"`
	}
	require.NoError(t, json.Unmarshal(g.Parameters, &params))
	require.NotEmpty(t, params)
	for _, p := range params {
		require.NotNilf(t, p.Value, "parameter %q is missing its \"value\" key entirely — the golden no longer pins it", p.Key)
		assert.NotEmptyf(t, *p.Value, "parameter %q's value must be a non-empty literal for this golden to pin anything", p.Key)
	}
}
