package kindtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// PromptShape is one of a kind's declared wizard inputs as an operator meets
// it: the key its answer lands under, the widget its type resolves to, THE
// PROMPT TEXT shown above it, and whether it must be answered.
//
// THE PROMPT TEXT IS THE POINT of this type, not decoration. Everything else
// about a question is checked somewhere else — its Name by whatever reads the
// answer, its Type by the renderer, its required-ness by the run that fails
// without it — but the words an operator reads are asserted here and NOWHERE
// ELSE in the tree. A reworded prompt is otherwise invisible.
type PromptShape struct {
	Name     string
	Type     oap.QuestionType
	Prompt   string
	Required bool
	// AskWhen is the gate deciding whether the operator meets this question at
	// all, and it belongs here for the same reason the prompt text does:
	// nothing else in the tree asserts it, and a question whose gate was
	// dropped keeps its Name, Type, Prompt and Required exactly as pinned while
	// silently being asked on every route. Its zero value is "asked always",
	// which is what an unbranched question pins as.
	AskWhen oap.AskWhen
}

// AssertPromptShapes is the per-kind gate on what a kind asks: it checks that
// qs is renderable at all, and then that it is exactly want, in order.
//
// Both halves matter. ValidateInputs is what refuses a question no client
// could render (an unknown Type, a QEnum with no values, a bundle-install
// Binding, a Validation nothing on this side evaluates) — it is the channel
// contract's own validator, deliberately narrower than
// oap.Manifest.ValidateQuestions, which additionally demands a Binding on
// every non-secret question and a Secret block on every secret one, both of
// which every channel input legitimately lacks. Comparing the whole slice in
// one assert.Equal, rather than field by field, is what makes a question
// ADDED, DROPPED or REORDERED fail as loudly as one whose prompt changed.
func AssertPromptShapes(t *testing.T, qs []oap.Question, want []PromptShape) {
	t.Helper()

	require.NoError(t, channelkinds.ValidateInputs(qs),
		"a kind must not declare an input no client could render")

	got := make([]PromptShape, 0, len(qs))
	for _, q := range qs {
		got = append(got, PromptShape{
			Name:     q.Name,
			Type:     q.Type,
			Prompt:   q.Prompt,
			Required: q.IsRequired(),
			AskWhen:  q.AskWhen,
		})
	}
	// Normalized so a kind that asks nothing compares equal to a want written
	// as either nil or an empty literal; every other shape is compared as-is.
	if len(got) == 0 && len(want) == 0 {
		return
	}
	assert.Equal(t, want, got, "the questions this kind declares, in order")
}
