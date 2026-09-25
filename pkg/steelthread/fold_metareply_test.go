package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// The cannable meta tool these tests drive. Named through the table rather than
// spelled as a literal everywhere, so a row leaving metaOverrides breaks the
// tests that rely on it being cannable rather than leaving them silently
// exercising a tool that now runs for real.
const cannableMeta = "search_memory"

func init() {
	if !bt.MetaToolCannable(cannableMeta) {
		panic("fold_metareply_test: " + cannableMeta + " is no longer cannable; pick another row from " +
			"bronzethread's metaOverrides for these tests")
	}
}

// metaTurns is a transcript that calls one meta tool once and then answers.
func metaTurns(name, body string, isErr bool) []memory.Turn {
	return []memory.Turn{
		userText(0, "what do you remember?"),
		assistantCall(1, "tu_1", name, `{"text":"widgets"}`),
		toolResult(2, "tu_1", body, isErr),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"here is what I found"}`),
	}
}

// TestFold_CansACannableMetaReply is the capability this file exists for.
//
// A meta tool whose own table says its answer comes from state a replay cannot
// hold contributes its recorded body to metaToolReplies, keyed by the
// LLM-facing name. Every other meta tool contributes nothing and runs for real.
func TestFold_CansACannableMetaReply(t *testing.T) {
	const body = `{"entries":[{"id":"m-1","content":"the widget rollout shipped"}]}`
	got, err := steelthread.Fold(steelthread.Records{Turns: metaTurns(cannableMeta, body, false)}, foldOpts())
	require.NoError(t, err)

	require.Contains(t, got.MetaToolReplies, cannableMeta)
	assert.Equal(t, body, got.MetaToolReplies[cannableMeta].Content,
		"the canned body is the bytes the model was handed, so the step's expectation derived from the "+
			"same bytes is satisfiable")
	assert.Empty(t, got.NonUniformMetaTools)
	assert.Empty(t, got.ToolOutputs, "a meta tool is not a transport and contributes no toolOutputs entry")
}

// TestFold_CansTheSameBytesTheStepAssertsOn pins the property that makes the
// canning worth anything: the body handed to the replay and the body the step
// asserts on are the SAME string.
//
// They are derived by two different paths — the canning branch and expectFor —
// and a normalization applied to one and not the other would fail every replay
// of a correct bundle while looking like a regression in the tool.
func TestFold_CansTheSameBytesTheStepAssertsOn(t *testing.T) {
	const body = `{"entries":[{"id":"m-1","content":"a & b < c"}]}`
	got, err := steelthread.Fold(steelthread.Records{Turns: metaTurns(cannableMeta, body, false)}, foldOpts())
	require.NoError(t, err)

	require.Len(t, got.LLM, 2)
	assert.Equal(t, cannableMeta, got.LLM[1].Expect.LastToolResult)
	assert.Equal(t, got.MetaToolReplies[cannableMeta].Content, got.LLM[1].Expect.LastToolResultContains,
		"the canned reply and the assertion derived from it must be byte-identical, or the replay "+
			"diverges on a run that took the identical path")
}

// TestFold_ARunRealMetaToolIsNeverCanned is the other half of the table's
// authority. update_plan, select_phase and the gate they drive ARE the code
// under test; canning one would hand a regression in it the recorded answer.
func TestFold_ARunRealMetaToolIsNeverCanned(t *testing.T) {
	require.False(t, bt.MetaToolCannable("update_plan"), "update_plan is the code under test")

	got, err := steelthread.Fold(steelthread.Records{
		Turns: metaTurns("update_plan", `{"operations":[{"id":"op-1"}]}`, false),
	}, foldOpts())
	require.NoError(t, err)

	assert.Empty(t, got.MetaToolReplies, "a RunReal meta tool contributes no canned reply")
	assert.Empty(t, got.NonUniformMetaTools)
}

// TestFold_NoMetaErrorIsEverCanned covers the rule in BOTH its directions, on
// the same cannable tool.
//
// A GATE refusal never reached the tool, and canning the gate's own refusal
// text would let a gate that STOPPED refusing be served that text back and
// satisfy the assertion meant to catch it. A tool's OWN refusal was composed by
// our code from our own inputs, so the replay composes it again; canning it
// would mask a regression in that composition. The fold therefore skips every
// error, which is why the third row — an error no record can explain at all —
// cans nothing either.
func TestFold_NoMetaErrorIsEverCanned(t *testing.T) {
	const refusal = "permission denied: alice does not have read on memory:s1"

	cases := []struct {
		name string
		recs func() steelthread.Records
	}{
		{
			name: "an authz gate refusal cans nothing",
			recs: func() steelthread.Records {
				return deniedFor("tu_1", refusal, metaTurns(cannableMeta, refusal, true))
			},
		},
		{
			name: "a denied approval cans nothing",
			recs: func() steelthread.Records {
				return steelthread.Records{
					Turns: metaTurns(cannableMeta, "SYSTEM_DECISION_DENIED: bob declined", true),
					Approvals: map[string]approval.Pair{
						"tu_1": {Outcome: &approval.Outcome{Decision: "denied", Approver: "bob"}},
					},
				}
			},
		},
		{
			name: "the tool's own refusal cans nothing either",
			recs: func() steelthread.Records {
				return steelthread.Records{
					Turns: metaTurns(cannableMeta, "search_memory: `text` is required", true),
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(tc.recs(), foldOpts())
			require.NoError(t, err)

			assert.Empty(t, got.MetaToolReplies,
				"an error result is never canned, whoever authored it")
			assert.Empty(t, got.NonUniformMetaTools,
				"a tool that only ever errored has nothing to be non-uniform about")

			require.Len(t, got.LLM, 2)
			require.NotNil(t, got.LLM[1].Expect.LastToolResultIsError)
			assert.True(t, *got.LLM[1].Expect.LastToolResultIsError,
				"the step still asserts the call came back an error")
		})
	}
}

// TestFold_ANonUniformMetaToolIsNamed covers the two ways one body per tool
// name cannot represent what happened.
//
// Both are named rather than dropped: canning the first body would serve it to
// a call that recorded something else, and the step derived from that other
// call would fail downstream naming the tool rather than the capture.
func TestFold_ANonUniformMetaToolIsNamed(t *testing.T) {
	cases := []struct {
		name  string
		turns []memory.Turn
	}{
		{
			name: "two successes with different bodies",
			turns: []memory.Turn{
				userText(0, "search twice"),
				assistantCall(1, "tu_1", cannableMeta, `{"text":"widgets"}`),
				toolResult(2, "tu_1", `{"entries":[{"id":"m-1"}]}`, false),
				assistantCall(3, "tu_2", cannableMeta, `{"text":"gadgets"}`),
				toolResult(4, "tu_2", `{"entries":[{"id":"m-2"}]}`, false),
				assistantCall(5, "tu_3", "respond_to_user", `{"text":"done"}`),
			},
		},
		{
			name: "a success beside an error",
			turns: []memory.Turn{
				userText(0, "search twice"),
				assistantCall(1, "tu_1", cannableMeta, `{"text":"widgets"}`),
				toolResult(2, "tu_1", `{"entries":[{"id":"m-1"}]}`, false),
				assistantCall(3, "tu_2", cannableMeta, `{}`),
				toolResult(4, "tu_2", "search_memory: `text` is required", true),
				assistantCall(5, "tu_3", "respond_to_user", `{"text":"done"}`),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(steelthread.Records{Turns: tc.turns}, foldOpts())
			require.NoError(t, err)

			assert.Equal(t, []string{cannableMeta}, got.NonUniformMetaTools)
			assert.Contains(t, got.MetaToolReplies, cannableMeta,
				"the entry is kept so a reader diffing the findings against the emitted file can see "+
					"which tool the finding meant")
		})
	}
}

// TestFold_TwoIdenticalMetaCallsStayUniform is the negative control for the
// test above: repetition is not variation, and a tool called twice with the
// same answer is representable by one body.
func TestFold_TwoIdenticalMetaCallsStayUniform(t *testing.T) {
	const body = `{"entries":[{"id":"m-1"}]}`
	got, err := steelthread.Fold(steelthread.Records{Turns: []memory.Turn{
		userText(0, "search twice"),
		assistantCall(1, "tu_1", cannableMeta, `{"text":"widgets"}`),
		toolResult(2, "tu_1", body, false),
		assistantCall(3, "tu_2", cannableMeta, `{"text":"widgets"}`),
		toolResult(4, "tu_2", body, false),
		assistantCall(5, "tu_3", "respond_to_user", `{"text":"done"}`),
	}}, foldOpts())
	require.NoError(t, err)

	assert.Empty(t, got.NonUniformMetaTools)
	assert.Equal(t, body, got.MetaToolReplies[cannableMeta].Content)
}

// TestFold_ADeclaredMCPPrefixStillWins pins the precedence the meta branch sits
// behind.
//
// The class's mcpServers[].name is a fact about THIS session; the cannable
// table is a fact about the repo. A contrived prefix collision must route the
// result to the transport that really declared it, not into a canned meta
// reply — otherwise a real MCP tool's output would vanish from toolOutputs and
// the stub would answer "unknown tool" at replay.
func TestFold_ADeclaredMCPPrefixStillWins(t *testing.T) {
	const body = `{"hits":3}`
	got, err := steelthread.Fold(
		steelthread.Records{Turns: metaTurns("search_memory", body, false)},
		steelthread.FoldOptions{MCPPrefixes: []string{"search"}},
	)
	require.NoError(t, err)

	assert.Empty(t, got.MetaToolReplies, "a declared prefix makes this an MCP call, not a meta one")
	assert.Equal(t, body, string(got.ToolOutputs["memory"]),
		"the result belongs to the declared server, keyed server-side")
}
