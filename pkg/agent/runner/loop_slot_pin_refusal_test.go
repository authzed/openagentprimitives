package runner_test

// Verifies that a mid-turn slot promotion refused by a single-occupancy pin
// does not die in an operator log: the refusal (naming the pinned instance and
// the route out) reaches the MODEL, appended to the denied tool result for the
// call that named the refused instance.
//
// The scenario the test reconstructs:
//   - the session is already pinned to github_repo:demo-org/pinned-repo,
//   - the user's message proposed a DIFFERENT repo, which authzd recorded as an
//     extracted candidate (demo-org/from-the-message),
//   - the agent then calls a slot-gated tool naming that different repo.
//
// The extracted-slot promotion admits the candidate (the requester has standing
// on it) but GrantSlots refuses to bind it — the pin holds a different instance
// — so the slot stays empty, the per-tool Check denies the call, and the denied
// tool result must carry the pin's own explanation.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// pinScenarioChecker allows the promotion's admissibility Check (which names the
// candidate under the synthetic {__resourceID__} template authz.checkBindable
// uses) so the candidate reaches GrantSlots and the pin refuses it, but DENIES
// the dispatch Check (which names the repo via the tool's own {repo} template)
// because the slot never bound that instance. Keying on the arg name is how the
// one fake tells the two checks apart; both flow through CheckToolCall.
type pinScenarioChecker struct{}

func (pinScenarioChecker) CheckToolCall(_ context.Context, _ authz.Permission, in authz.Inputs) authz.Result {
	if _, isPromotionAdmissibility := in.Args["__resourceID__"]; isPromotionAdmissibility {
		return authz.Result{Outcome: authz.OutcomeAllowed}
	}
	return authz.Result{
		Outcome: authz.OutcomeDenied,
		Message: "permission denied: alice does not have read on github_repo:demo-org/from-the-message",
	}
}

// TestAutofill_ExtractedCandidate_PinRefusalReachesModel is the surfacing proof
// for the extracted-slot promotion path.
func TestAutofill_ExtractedCandidate_PinRefusalReachesModel(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "gh_pr_view",
		seedExtracted:   "demo-org/from-the-message",
		seedPin:         "demo-org/pinned-repo",
		inputJSON:       `{"repo":"demo-org/from-the-message","prNumber":13}`,
		toolChecker:     pinScenarioChecker{},
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	// The call was denied, so the tool never ran.
	assert.Nil(t, capTool.gotJSON.Load(), "a denied call must not execute")

	// The denied tool result fed back to the model must carry the pin's own
	// refusal text: the pinned instance and the route out.
	provider, ok := l.Provider.(*llmfake.Provider)
	require.True(t, ok, "expected the fake provider")
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2, "expected at least 2 LLM requests")

	var denyMsg string
	for _, msg := range reqs[1].Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError {
				denyMsg = blk.ToolResult.Content
			}
		}
	}
	require.NotEmpty(t, denyMsg, "no IsError tool_result reached the model")
	assert.Contains(t, denyMsg, "demo-org/pinned-repo",
		"the model must be told which instance the session is pinned to")
	assert.Contains(t, denyMsg, "propose an updated plan",
		"the model must be given the route out the pin refusal names")
}

// TestAutofill_NoPinRefusal_PlainDenialUnchanged guards against the explanation
// leaking onto an ordinary denial. With no pin seeded (nothing refused a
// promotion), the same denied call carries only the checker's own message —
// the pin text is appended ONLY when a refusal was actually recorded.
func TestAutofill_NoPinRefusal_PlainDenialUnchanged(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "gh_pr_view",
		seedExtracted:   "demo-org/from-the-message",
		inputJSON:       `{"repo":"demo-org/from-the-message","prNumber":13}`,
		toolChecker:     pinScenarioChecker{},
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	assert.Nil(t, capTool.gotJSON.Load(), "a denied call must not execute")

	provider := l.Provider.(*llmfake.Provider)
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2)
	var denyMsg string
	for _, msg := range reqs[1].Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError {
				denyMsg = blk.ToolResult.Content
			}
		}
	}
	require.NotEmpty(t, denyMsg)
	assert.Contains(t, denyMsg, "permission denied", "the plain checker denial still reaches the model")
	assert.NotContains(t, denyMsg, "pinned to",
		"with nothing refused, no pin explanation must be appended")
}

// TestAutofill_DenialOfThePinnedInstanceItself_NoMoveAdvice guards the
// mislabel case: a refusal about instance B is recorded, but the DENIED call
// names the PINNED instance A — some other gate refused it, not the pin — so
// appending "this session is pinned to A; propose a plan to move" would send
// the model chasing a move it does not need. The seam resolves the call's own
// resource id (authz.ResolveResourceID over the same args the checker saw) and
// skips the append when it equals the pinned instance read back at record time.
func TestAutofill_DenialOfThePinnedInstanceItself_NoMoveAdvice(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "gh_pr_view",
		seedExtracted:   "demo-org/from-the-message",
		seedPin:         "demo-org/pinned-repo",
		// The call names the PINNED instance, not the refused candidate.
		inputJSON:   `{"repo":"demo-org/pinned-repo","prNumber":14}`,
		toolChecker: pinScenarioChecker{},
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	assert.Nil(t, capTool.gotJSON.Load(), "a denied call must not execute")

	provider := l.Provider.(*llmfake.Provider)
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2)
	var denyMsg string
	for _, msg := range reqs[1].Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError {
				denyMsg = blk.ToolResult.Content
			}
		}
	}
	require.NotEmpty(t, denyMsg)
	assert.Contains(t, denyMsg, "permission denied", "the gate's own denial still reaches the model")
	assert.NotContains(t, denyMsg, "propose an updated plan",
		"a denial of the pinned instance itself must not carry move advice")
}
