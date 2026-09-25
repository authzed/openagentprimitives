package steelthread_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// A recorded artifact_prepare result whose warnings are in the arbitrary order
// the sanitizer's map iteration produced before that order was defined.
const recordedUncanonicalResult = `{"handle":"ar-demo-agent-9999-cd752b",` +
	`"artifact_id":"artifact-1111111111111111","revision_id":"artrev-2222222222222222","seq":1,` +
	`"status":"ready","mime":"text/html","size":5291,"filename":"review.html",` +
	`"warnings":[{"kind":"tag","name":"title","action":"removed","count":1},` +
	`{"kind":"attr","name":"lang","action":"stripped","count":1},` +
	`{"kind":"attr","name":"charset","action":"stripped","count":1}]}`

// TestFold_MetaResultExpectationIsCanonical is the join between a production
// fix and the capture.
//
// The sanitizer used to emit its warnings in Go map order, so every session
// recorded before that was fixed pinned one arbitrary permutation of a set the
// tool now emits sorted. A capture that carried the recorded bytes straight
// through would emit a clean bundle whose ONE artifact step then failed at
// replay, on nothing but a sequence.
//
// So the derived expectation is what the tool's own encoder emits for the
// recorded values — the same move expectFor already makes for a canned result
// through the fake server's encoder.
func TestFold_MetaResultExpectationIsCanonical(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "artifact_prepare", `{"kind":"html","payload":"<h1>x</h1>"}`),
		toolResult(2, "tu_1", recordedUncanonicalResult, false),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"done"}`),
	}}, foldOpts())
	require.NoError(t, err)
	require.Len(t, got.LLM, 2, "one user turn plus the step whose expectation answers the call")

	claim := got.LLM[1].Expect.LastToolResultContains
	require.NotEmpty(t, claim)

	assert.Less(t,
		strings.Index(claim, `"name":"charset"`), strings.Index(claim, `"name":"lang"`),
		"warnings are re-expressed in the canonical order, not the recorded one")
	assert.Less(t,
		strings.Index(claim, `"name":"lang"`), strings.Index(claim, `"name":"title"`),
		"attrs sort before tags under kind-then-name")

	// Every VALUE survives: the normalization reorders and does nothing else,
	// or it would be quietly weakening the one claim this step makes.
	for _, want := range []string{
		`"handle":"ar-demo-agent-9999-cd752b"`,
		`"revision_id":"artrev-2222222222222222"`,
		`"size":5291`,
		`"status":"ready"`,
		`"filename":"review.html"`,
		`"seq":1`,
	} {
		assert.Contains(t, claim, want, "canonicalization must preserve every recorded value")
	}
}

// TestFold_MetaResultRefusalIsNotCanonicalized is the negative control. An
// error result is a sentence, not a result object; re-encoding is not available
// and the recorded text IS the claim.
func TestFold_MetaResultRefusalIsNotCanonicalized(t *testing.T) {
	const refusal = `artifact_prepare: kind "pdf" is not available on this channel.`
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "go"),
		assistantCall(1, "tu_1", "artifact_prepare", `{"kind":"pdf","payload":"x"}`),
		toolResult(2, "tu_1", refusal, true),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"cannot"}`),
	}}, foldOpts())
	require.NoError(t, err)
	require.Len(t, got.LLM, 2)
	assert.Equal(t, refusal, got.LLM[1].Expect.LastToolResultContains,
		"a refusal is carried byte-for-byte: it is our code's own wording and the replay re-emits it")
}
