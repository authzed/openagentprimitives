package initpipeline

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// plainAsk is the presentation every test here uses: the line-oriented driver,
// reading a scripted script and writing to a buffer. Nothing in this package
// may reach for the process's own stdin/stdout, so a test that supplies neither
// would be exercising the terminal the test runner happens to have.
func plainAsk(t *testing.T, script string, interactive bool) (AskOptions, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return AskOptions{
		Interactive: interactive,
		In:          strings.NewReader(script),
		Out:         &out,
		Theme:       tui.NewTheme(tui.Caps{}),
	}, &out
}

func TestResolve_FlagOverDefault(t *testing.T) {
	comps := []Component{{Name: "gw", Inputs: []Input{{Flag: "trusted-hostname", Default: "d.example.com"}}}}
	opts, _ := plainAsk(t, "", false)
	r, err := Resolve(context.Background(), comps, map[string]string{"trusted-hostname": "x.example.com"}, opts)
	require.NoError(t, err)
	assert.Equal(t, "x.example.com", r["trusted-hostname"])
}

func TestResolve_DefaultWhenUnsetNonInteractive(t *testing.T) {
	comps := []Component{{Name: "gw", Inputs: []Input{{Flag: "trusted-hostname", Default: "d.example.com"}}}}
	opts, _ := plainAsk(t, "", false)
	r, err := Resolve(context.Background(), comps, map[string]string{}, opts)
	require.NoError(t, err)
	assert.Equal(t, "d.example.com", r["trusted-hostname"])
}

// A non-interactive run with nothing to fall back on must refuse — and it must
// refuse in the operator's words. It presents NO screen at all, so the
// fail-closed driver is never reached; its refusal names a screen ID and points
// at "flags" in the abstract, neither of which this phase's operator can act
// on, and both of which would replace the message naming the actual flag.
func TestResolve_RequiredUnsetNonInteractive_RefusesNamingTheFlagNotAScreen(t *testing.T) {
	comps := []Component{{Name: "gw", Inputs: []Input{{Flag: "acme-email", Required: true}}}}
	opts, _ := plainAsk(t, "", false)

	_, err := Resolve(context.Background(), comps, map[string]string{}, opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--acme-email")
	assert.NotErrorIs(t, err, tui.ErrUnanswered, "nothing was asked, so nothing can be unanswered")
	// The literal, not tui's own constant: an assertion sharing the constant
	// with the code it checks moves in lockstep with it and stops failing.
	assert.NotContains(t, err.Error(), "tui: ", "the sequencer's framing is ours, not the operator's")
	assert.NotContains(t, err.Error(), "gw/acme-email", "a screen ID is our vocabulary, not the operator's")
}

// The State-first rule, stated as behaviour: an input a flag already answered
// asks nothing, so the NEXT input is what consumes the first scripted line. A
// screen that built its group regardless would feed "typed" to the answered
// input and leave the unanswered one reading EOF — which huh's accessible
// renderer reports as a silently-defaulted value and a nil error.
func TestResolve_SeededInputConsumesNoInput(t *testing.T) {
	comps := []Component{{Name: "gw", Inputs: []Input{
		{Flag: "one", Default: "default-one"},
		{Flag: "two", Default: "default-two"},
	}}}
	opts, _ := plainAsk(t, "typed\n", true)

	r, err := Resolve(context.Background(), comps, map[string]string{"one": "flagged"}, opts)

	require.NoError(t, err)
	assert.Equal(t, "flagged", r["one"], "a flag-answered input must not be asked again")
	assert.Equal(t, "typed", r["two"], "the unanswered input gets the scripted line")
}

// The interactive path end to end: every screen is answered from the script,
// and the assertion is on the Resolved map rather than on err — huh's
// accessible renderer has no error channel, so a short script produces a fully
// defaulted result with a nil error.
func TestResolve_InteractivePromptsEachUnansweredInput(t *testing.T) {
	comps := []Component{
		{Name: "gw", Inputs: []Input{{Flag: "trusted-hostname", Default: "d.example.com"}}},
		{Name: "certs", Inputs: []Input{{Flag: "acme-email", Required: true}}},
	}
	opts, _ := plainAsk(t, "host.example.com\nops@example.com\n", true)

	r, err := Resolve(context.Background(), comps, map[string]string{}, opts)

	require.NoError(t, err)
	assert.Equal(t, "host.example.com", r["trusted-hostname"])
	assert.Equal(t, "ops@example.com", r["acme-email"])
}

// A bare Enter on a pre-filled field is how an operator accepts the default.
// huh hands its validator the TYPED line rather than the bound value, so a
// validator that rejected "" would reject that gesture — and, under the
// accessible renderer, would re-prompt and eat the NEXT input's line.
func TestResolve_EmptyLineKeepsTheDefaultAndDoesNotEatTheNextAnswer(t *testing.T) {
	comps := []Component{{Name: "gw", Inputs: []Input{
		{Flag: "one", Default: "default-one"},
		{Flag: "two", Default: "default-two"},
	}}}
	opts, _ := plainAsk(t, "\nsecond\n", true)

	r, err := Resolve(context.Background(), comps, map[string]string{}, opts)

	require.NoError(t, err)
	assert.Equal(t, "default-one", r["one"], "an empty line accepts the pre-filled default")
	assert.Equal(t, "second", r["two"], "the next input still has its own line to read")
}

// An interactive run whose input runs out must not report success for a
// required value nobody supplied: the accessible renderer returns a nil error
// on EOF, so the Required check is the only thing standing between that and a
// pipeline installed with an empty answer.
func TestResolve_InteractiveExhaustedScriptStillRefusesARequiredInput(t *testing.T) {
	comps := []Component{{Name: "certs", Inputs: []Input{{Flag: "acme-email", Required: true}}}}
	opts, _ := plainAsk(t, "", true)

	_, err := Resolve(context.Background(), comps, map[string]string{}, opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--acme-email")
}

// Resolved is keyed by flag, so two components naming the same flag are
// declaring one input — and the second must not re-ask a question the first
// already resolved.
func TestResolve_SameFlagInTwoComponentsIsAskedOnce(t *testing.T) {
	comps := []Component{
		{Name: "gw", Inputs: []Input{{Flag: "shared"}}},
		{Name: "certs", Inputs: []Input{{Flag: "shared"}}},
	}
	opts, _ := plainAsk(t, "once\ntwice\n", true)

	r, err := Resolve(context.Background(), comps, map[string]string{}, opts)

	require.NoError(t, err)
	assert.Equal(t, "once", r["shared"], "the second declaration must reuse the first answer")
}

// A conditional Input reads answers given EARLIER IN THE SAME RUN, which is
// the whole reason it exists — the condition cannot be settled before the run
// starts. Both rows script two lines; the second is consumed only by a
// question that was actually asked, so the assertion on "two" discriminates
// between the conditional firing and not.
func TestResolve_AskWhen(t *testing.T) {
	cases := []struct {
		name    string
		askWhen func(Resolved) bool
		script  string
		wantOne string
		wantTwo string
	}{
		{
			name:    "predicate true on the earlier answer: asked, and takes the second line",
			askWhen: func(a Resolved) bool { return a["one"] != "" },
			script:  "typed-one\ntyped-two\n",
			wantOne: "typed-one",
			wantTwo: "typed-two",
		},
		{
			name:    "predicate false on the earlier answer: never asked, second line untouched",
			askWhen: func(a Resolved) bool { return a["one"] == "sentinel-never-typed" },
			script:  "typed-one\ntyped-two\n",
			wantOne: "typed-one",
			wantTwo: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comps := []Component{{Name: "gw", Inputs: []Input{
				{Flag: "one"},
				{Flag: "two", AskWhen: tc.askWhen},
			}}}
			opts, _ := plainAsk(t, tc.script, true)

			r, err := Resolve(context.Background(), comps, map[string]string{}, opts)

			require.NoError(t, err)
			assert.Equal(t, tc.wantOne, r["one"])
			assert.Equal(t, tc.wantTwo, r["two"])
		})
	}
}

// A predicate may read a flag no Input declares. That is how "--tls-issuer
// makes --acme-email unnecessary" is expressed: the issuer is never asked for,
// but it decides whether the email is.
func TestResolve_AskWhenReadsAFlagNoInputDeclares(t *testing.T) {
	comps := []Component{{Name: "certs", Inputs: []Input{{
		Flag:    "acme-email",
		AskWhen: func(a Resolved) bool { return a["tls-issuer"] == "" },
	}}}}
	opts, _ := plainAsk(t, "ops@example.com\n", true)

	r, err := Resolve(context.Background(), comps, map[string]string{"tls-issuer": "existing-issuer"}, opts)

	require.NoError(t, err)
	assert.Empty(t, r["acme-email"], "a seeded flag the predicate reads must be able to call the question off")
}

// Required and AskWhen compose the only way they can: an input the run decided
// against asking for is not one it can then refuse to run without.
func TestResolve_RequiredIsSuppressedForAnInputAskWhenDeclined(t *testing.T) {
	comps := []Component{{Name: "certs", Inputs: []Input{{
		Flag:     "acme-email",
		Required: true,
		AskWhen:  func(Resolved) bool { return false },
	}}}}
	opts, _ := plainAsk(t, "", true)

	r, err := Resolve(context.Background(), comps, map[string]string{}, opts)

	require.NoError(t, err)
	assert.Empty(t, r["acme-email"])
}

// …and it still refuses when the condition admits the input. Without this the
// row above could pass by Required having been dropped altogether.
func TestResolve_RequiredStillRefusesWhenAskWhenAdmitsTheInput(t *testing.T) {
	comps := []Component{{Name: "certs", Inputs: []Input{{
		Flag:     "acme-email",
		Required: true,
		AskWhen:  func(Resolved) bool { return true },
	}}}}
	opts, _ := plainAsk(t, "", true)

	_, err := Resolve(context.Background(), comps, map[string]string{}, opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--acme-email")
}

// Inline is invisible in every driver's rendered output, so the decision is
// only readable where it is declared. See askRunOptions for why this phase
// draws in the terminal's ordinary buffer.
func TestAskRunOptions_DrawsInline(t *testing.T) {
	o := askRunOptions(AskOptions{Interactive: true}, tui.NewTheme(tui.Caps{}))
	assert.True(t, o.Inline, "the install's questions are asked from the middle of its own output")
	assert.False(t, o.NonInteractive, "an interactive run must not resolve the fail-closed driver")
	assert.Equal(t, askTitle, o.Title)
}

func TestResolve_NoInputsDeclared(t *testing.T) {
	opts, out := plainAsk(t, "", true)
	r, err := Resolve(context.Background(), []Component{{Name: "gw"}}, map[string]string{}, opts)
	require.NoError(t, err)
	assert.Empty(t, r)
	assert.Empty(t, out.String(), "a pipeline that asks nothing must render nothing")
}

// A nil Theme is an incomplete caller, not a reason to abort an install: the
// uncolored theme is the safe substitute, the same one every driver
// constructor falls back to.
func TestResolve_NilThemeFallsBackToTheUncoloredOne(t *testing.T) {
	comps := []Component{{Name: "gw", Inputs: []Input{{Flag: "one", Default: "d"}}}}
	r, err := Resolve(context.Background(), comps, map[string]string{}, AskOptions{Out: &bytes.Buffer{}})
	require.NoError(t, err)
	assert.Equal(t, "d", r["one"])
}
