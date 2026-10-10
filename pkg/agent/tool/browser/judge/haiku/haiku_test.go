package haiku_test

// haiku_test.go is the COMMITTED, network-free test suite for the Haiku
// consequence-judge backend: every test here drives haiku.Judge over a
// scripted pkg/agent/llm/fake.Provider, never a real Anthropic call. It
// covers three things:
//
//  1. Plumbing correctness: for every one of the 54 labeled corpus cases
//     (testdata/corpus.json, loaded via corpus_test.go), a fake Provider
//     scripted to return a JSON distribution peaked on that case's GOLD tier
//     round-trips to a Verdict carrying the right Tier and Probs. This
//     proves the prompt-building / response-parsing path, NOT real Haiku
//     accuracy -- that is haiku_corpus_eval_test.go's job, env-gated off.
//  2. The threshold POLICY: "ask if P(mutate) >= DefaultMutateThreshold"
//     yields zero escapes (a gold-mutate case classified not-ask) on the
//     scripted distributions, with over-asks reported via t.Logf.
//  3. Fail-closed error paths: a provider Send error, an unparseable
//     response, a missing/unrecognized tier, and an incomplete probs object
//     each surface as a Classify error -- never a silently-defaulted Verdict.
import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/browser/judge/haiku"
)

// scriptedProbs builds a plausible (not one-hot) probability distribution
// peaked on gold, simulating a model that is confident but not absolutely
// certain. "ADVERSARIAL-benign" cases (gold-benign actions deliberately
// disguised to LOOK mutating -- a filter chip reading "x Nonstop only", a
// modal "Cancel") get a deliberately larger mutate-hedge, crossing
// haiku.DefaultMutateThreshold: this is what makes the policy test's
// over-ask count non-trivial while still asserting zero escapes.
func scriptedProbs(note string, gold judge.Tier) map[string]float64 {
	goldMass := 0.80
	mutateHedge := 0.10
	if strings.Contains(note, "ADVERSARIAL-benign") {
		goldMass = 0.55
		mutateHedge = 0.25
	}

	others := []judge.Tier{judge.TierObserve, judge.TierNavigate, judge.TierSession, judge.TierMutate}
	probs := make(map[string]float64, len(others))

	if gold == judge.TierMutate {
		share := (1.0 - goldMass) / 3
		for _, t := range others {
			if t == gold {
				probs[string(t)] = goldMass
			} else {
				probs[string(t)] = share
			}
		}
		return probs
	}

	probs[string(judge.TierMutate)] = mutateHedge
	leftover := 1.0 - goldMass - mutateHedge
	share := leftover / 2
	for _, t := range others {
		if t == gold || t == judge.TierMutate {
			continue
		}
		probs[string(t)] = share
	}
	probs[string(gold)] = goldMass
	return probs
}

// scriptedResponseText marshals a verdictJSON-shaped payload for the fake
// provider to return, peaked on gold per scriptedProbs.
func scriptedResponseText(t *testing.T, note string, gold judge.Tier) string {
	t.Helper()
	payload := map[string]any{
		"tier":      string(gold),
		"probs":     scriptedProbs(note, gold),
		"rationale": "scripted for test: " + note,
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return string(raw)
}

func scriptedResponse(t *testing.T, note string, gold judge.Tier) llm.Response {
	t.Helper()
	return llm.Response{
		Content:    []llm.ContentBlock{{Type: "text", Text: scriptedResponseText(t, note, gold)}},
		StopReason: "end_turn",
	}
}

// TestClassify_MapsScriptedResponseToGoldTier is the per-case plumbing test:
// for every corpus case, a fake Provider scripted to return a distribution
// peaked on that case's gold tier must round-trip through Classify to a
// Verdict carrying that tier and the scripted Probs.
func TestClassify_MapsScriptedResponseToGoldTier(t *testing.T) {
	corpus := loadCorpus(t)
	require.Len(t, corpus, 54, "the corpus must carry the 51 ported spike_corpus.py cases plus the 3 task-52 httpbin form-fill/commit cases")

	for _, tc := range corpus {
		t.Run(tc.Note, func(t *testing.T) {
			prov := fake.New([]fake.Step{{Resp: scriptedResponse(t, tc.Note, tc.Gold)}})
			j := haiku.New(prov, "")

			v, err := j.Classify(context.Background(), tc.Action, tc.Page)
			require.NoError(t, err)
			assert.Equal(t, tc.Gold, v.Tier)

			wantProbs := scriptedProbs(tc.Note, tc.Gold)
			require.Len(t, v.Probs, 4, "Verdict.Probs must carry all four tiers")
			for tier, want := range wantProbs {
				assert.InDelta(t, want, v.Probs[judge.Tier(tier)], 1e-9, "probs[%s]", tier)
			}
			assert.Contains(t, v.Rationale, tc.Note)

			// Sanity on what we actually sent: the model-facing request must
			// name the resolved model id and carry the action's facts.
			reqs := prov.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, haiku.DefaultModelID, reqs[0].Model)
			require.Len(t, reqs[0].System, 1)
			assert.Contains(t, reqs[0].System[0].Text, "mutate")
			require.Len(t, reqs[0].Messages, 1)
			require.Len(t, reqs[0].Messages[0].Content, 1)
			if tc.Action.PageURL != "" {
				assert.Contains(t, reqs[0].Messages[0].Content[0].Text, tc.Action.PageURL)
			}
		})
	}
}

// TestMutateThresholdPolicy_ZeroEscapes applies the "ask if P(mutate) >= T"
// policy (E8's eventual gate) to the scripted distributions at
// haiku.DefaultMutateThreshold, mirroring spike_gate2.py's policy_eval: a
// gold-mutate case classified not-ask is an ESCAPE (never acceptable); a
// gold-benign case classified ask is an OVER-ASK (acceptable, just a UX
// cost). This tests the threshold LOGIC against scripted confidence, not
// real Haiku accuracy -- see haiku_corpus_eval_test.go for that.
func TestMutateThresholdPolicy_ZeroEscapes(t *testing.T) {
	corpus := loadCorpus(t)

	steps := make([]fake.Step, len(corpus))
	for i, tc := range corpus {
		steps[i] = fake.Step{Resp: scriptedResponse(t, tc.Note, tc.Gold)}
	}
	prov := fake.New(steps)
	j := haiku.New(prov, "")

	var escapes, overAsks, autoAllowed int
	for _, tc := range corpus {
		v, err := j.Classify(context.Background(), tc.Action, tc.Page)
		require.NoError(t, err, "case %q", tc.Note)

		ask := v.Probs[judge.TierMutate] >= haiku.DefaultMutateThreshold
		switch {
		case tc.Gold == judge.TierMutate && !ask:
			escapes++
			t.Errorf("ESCAPE: gold-mutate case %q classified not-ask (P(mutate)=%.3f < %.3f)",
				tc.Note, v.Probs[judge.TierMutate], haiku.DefaultMutateThreshold)
		case tc.Gold != judge.TierMutate && ask:
			overAsks++
		case !ask:
			autoAllowed++
		}
	}

	assert.Zero(t, escapes, "the mutate threshold must never let a gold-mutate case through unasked")
	t.Logf("policy eval @ T=%.2f: auto-allowed %d/%d, escapes %d, over-asks %d",
		haiku.DefaultMutateThreshold, autoAllowed, len(corpus), escapes, overAsks)
}

func TestClassify_ProviderSendError(t *testing.T) {
	prov := fake.New([]fake.Step{{Err: fake.ErrInjected}})
	j := haiku.New(prov, "")

	_, err := j.Classify(context.Background(), judge.Action{Op: "click", Role: "button", Text: "Delete account"}, judge.PageContext{})
	require.Error(t, err)
	assert.ErrorIs(t, err, fake.ErrInjected)
}

func TestClassify_NoTextContent(t *testing.T) {
	prov := fake.New([]fake.Step{{Resp: llm.Response{StopReason: "max_tokens"}}})
	j := haiku.New(prov, "")

	_, err := j.Classify(context.Background(), judge.Action{Op: "click"}, judge.PageContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no text content")
}

func TestClassify_UnparseableJSON(t *testing.T) {
	prov := fake.New([]fake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: "that action looks fine, proceed"}},
	}}})
	j := haiku.New(prov, "")

	_, err := j.Classify(context.Background(), judge.Action{Op: "click"}, judge.PageContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid JSON")
}

func TestClassify_MissingTier(t *testing.T) {
	prov := fake.New([]fake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: `{"probs":{"observe":1,"navigate":0,"session":0,"mutate":0}}`}},
	}}})
	j := haiku.New(prov, "")

	_, err := j.Classify(context.Background(), judge.Action{Op: "click"}, judge.PageContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing or unrecognized tier")
}

func TestClassify_UnrecognizedTier(t *testing.T) {
	prov := fake.New([]fake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: `{"tier":"delete","probs":{"observe":1,"navigate":0,"session":0,"mutate":0}}`}},
	}}})
	j := haiku.New(prov, "")

	_, err := j.Classify(context.Background(), judge.Action{Op: "click"}, judge.PageContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing or unrecognized tier")
}

func TestClassify_ProbsMissingATier(t *testing.T) {
	prov := fake.New([]fake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: `{"tier":"mutate","probs":{"observe":0.5,"navigate":0.5}}`}},
	}}})
	j := haiku.New(prov, "")

	_, err := j.Classify(context.Background(), judge.Action{Op: "click"}, judge.PageContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `probs missing tier`)
}

// TestClassify_ProbsOutOfRange proves a probability outside [0,1] is a
// model-misbehavior ERROR, not a value to clamp-and-trust — same fail-closed
// discipline as a fifth tier or a missing key. (NaN/Inf cannot arrive through
// valid JSON, so the json parser rejects those first; parseVerdict's
// math.IsNaN/IsInf guard is defensive for any non-JSON future caller.)
func TestClassify_ProbsOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "mutate > 1 is rejected", body: `{"tier":"observe","probs":{"observe":0.5,"navigate":0,"session":0,"mutate":1.5}}`},
		{name: "negative prob is rejected", body: `{"tier":"observe","probs":{"observe":1.1,"navigate":-0.1,"session":0,"mutate":0}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := fake.New([]fake.Step{{Resp: llm.Response{
				Content: []llm.ContentBlock{{Type: "text", Text: tc.body}},
			}}})
			j := haiku.New(prov, "")

			_, err := j.Classify(context.Background(), judge.Action{Op: "click"}, judge.PageContext{})
			require.Error(t, err, "an out-of-range probability must fail closed, never clamp")
			assert.Contains(t, err.Error(), "out of range")
		})
	}
}

func TestClassify_StripsMarkdownFences(t *testing.T) {
	fenced := "```json\n" + `{"tier":"observe","probs":{"observe":0.9,"navigate":0.05,"session":0.03,"mutate":0.02},"rationale":"fenced"}` + "\n```"
	prov := fake.New([]fake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: fenced}},
	}}})
	j := haiku.New(prov, "")

	v, err := j.Classify(context.Background(), judge.Action{Op: "click"}, judge.PageContext{})
	require.NoError(t, err)
	assert.Equal(t, judge.TierObserve, v.Tier)
}

// TestRegisteredLazyJudge_NoAPIKey exercises this backend's self-registration
// (register.go's init(), not called directly by this test) end to end
// through judge.Get: it must be registered under haiku.Name regardless of
// whether ANTHROPIC_API_KEY happens to be set in the test environment, and
// Classify must fail closed with a clear error when the key is unset --
// never panic, never silently construct a broken provider.
func TestRegisteredLazyJudge_NoAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	j, ok := judge.Get(haiku.Name)
	require.True(t, ok, "the haiku judge must self-register at package init")

	_, err := j.Classify(context.Background(), judge.Action{Op: "click", Role: "button", Text: "Delete account"}, judge.PageContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ANTHROPIC_API_KEY")
}

func TestName(t *testing.T) {
	j := haiku.New(fake.New(nil), "")
	assert.Equal(t, "anthropic-haiku", j.Name())
	assert.Equal(t, haiku.Name, j.Name())
}
