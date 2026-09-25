//go:build e2e

// TestReviewbotTriggerStatus_* covers the trigger-status seam as a whole
// session: a signed pull_request webhook arrives, the pipeline spawns a
// session bound to a kind=github INPUT Channel, the runner assembles its meta
// tools through the real capability registry, and the two tools it gets drive
// the REAL github kind against FakeGitHub over HTTP.
//
// What this proves that a unit test cannot: that the tools are offered at all
// for a github-triggered session (the gate is the input binding's kind, which
// only a real session has), and that a check run addressed by nothing the model
// supplied lands on the pull request the webhook named.
//
// # Why this is here and not a bronzethread bundle
//
// Bronzethread's only session-creation path is injecting into the `fake`
// channel kind's driver (e2e.Harness.SendUserMessage → fakekind.DriverFor, and
// singleChannel refuses a fixture with more than one conversational Channel).
// A bundle's INPUT channel kind can therefore only ever be `fake` — and this
// capability is gated precisely on the input kind owning a status surface,
// which `fake` does not and must not claim to. Making a bundle possible needs a
// second session-creation path in the driver (a signed delivery through the
// webd route), which is harness work rather than part of this seam.
package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// The fixture pull request. headSHA matches nothing in the webhook payload on
// purpose: the surface resolves the commit it answers for by READING the pull
// request, so a value that could only have come from that read is what proves
// the agent supplied none of it.
const (
	triggerHeadSHA   = "feedfacefeedfacefeedfacefeedfacefeedface"
	triggerCheckName = "demo-reviewbot" // the Channel's spec.github.appSlug
)

// concludedClaim / unconcludedClaim are OnToolResult predicates over
// claim_trigger_status's parsed JSON, shared so two scripts cannot drift on
// what "already answered" means.
func concludedClaim(parsed any) bool {
	m, ok := parsed.(map[string]any)
	if !ok {
		return false
	}
	b, _ := m["already_concluded"].(bool)
	return b
}

func unconcludedClaim(parsed any) bool { return !concludedClaim(parsed) }

// TestReviewbotTriggerStatus_ClaimThenConcludeLandsOnThePullRequestTheWebhookNamed
// is the seam end to end. The scripted agent calls claim_trigger_status with NO
// arguments and conclude_trigger_status with a verdict and prose — and what
// GitHub ends up holding is a completed check run on the pull request's own head
// commit, named for the App, carrying the reviewed commit in external_id.
func TestReviewbotTriggerStatus_ClaimThenConcludeLandsOnThePullRequestTheWebhookNamed(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")
	h.FakeGitHub.SetHeadSHA(triggerHeadSHA)

	h.LLM.OnUserMessage("demo-org/platform#42").
		Reply(e2e.ToolUse("claim_trigger_status", map[string]any{}))
	h.LLM.OnToolResult("claim_trigger_status", unconcludedClaim).
		Reply(e2e.RespondToUser("Reviewed PR #42: two findings in the auth path."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("conclude_trigger_status", map[string]any{
			"outcome":     "problems_found",
			"summary":     "Two findings in the auth path; see the thread for detail.",
			"details_url": "https://example.test/thread/42",
		}))
	h.LLM.OnToolResult("conclude_trigger_status", e2e.AnyResult()).
		Reply(e2e.EndTurn())

	body := h.LoadFixture(t, "pull_request_opened.json")
	require.Equal(t, http.StatusAccepted, h.PostWebhook(t, ch, body, h.SignWebhook(t, ch, body)).StatusCode)

	sessions := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, sessions, 1)
	require.Len(t, h.EventuallySlackPosts(t, 2, 60*time.Second), 2)
	// The Slack wait is NOT a settle point for this script, and cannot be made
	// into one: the second post lands inside respond_to_user, which is the
	// SECOND of three tool calls. The conclusion this test is entirely about is
	// issued afterwards, so a sample taken when the post count reaches two is
	// taken while the answer is still in flight — and the assertions below then
	// read a check run that has not been written yet. Wait for the round.
	h.WaitForSessionPhase(sessions[0].Namespace, sessions[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)
	h.AssertAllRulesConsumed()

	run := h.FakeGitHub.CheckRunFor(triggerHeadSHA, triggerCheckName)
	require.NotNil(t, run,
		"nothing was published for %s — the whole point of the seam is that the answer reaches the pull request", triggerHeadSHA)
	assert.Equal(t, "completed", run.Status, "a review that finished must not leave the check run in progress")
	assert.Equal(t, "action_required", run.Conclusion,
		"the framework's problems_found maps onto github's own vocabulary in the kind, not in the prompt")
	assert.Equal(t, triggerHeadSHA, run.ExternalID,
		"the commit answered for is recorded by the kind; the agent never saw it")
	assert.Equal(t, "https://example.test/thread/42", run.DetailsURL)
	require.NotNil(t, run.Output)
	assert.Contains(t, run.Output.Summary, "auth path")

	creates, updates := h.FakeGitHub.CheckRunCalls()
	assert.Equal(t, 1, creates, "the claim opened exactly one check run")
	assert.Equal(t, 1, updates,
		"the conclusion patched the run the claim opened; a second create would mean the claim was left behind")
}

// TestReviewbotTriggerStatus_AClaimOnAnAlreadyAnsweredCommitReportsItAndPostsNothing
// is the redelivery path, now owned by the framework rather than reconstructed
// by the agent from a hand-written list call. GitHub already holds an answer for
// this commit, so the claim says so, the scripted agent ends the round, and
// nothing is written to GitHub or to Slack.
func TestReviewbotTriggerStatus_AClaimOnAnAlreadyAnsweredCommitReportsItAndPostsNothing(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")
	h.FakeGitHub.SetHeadSHA(triggerHeadSHA)
	// The durable dedup fact, where it actually lives: on GitHub, not in AP.
	h.FakeGitHub.SetCheckRunConclusion(triggerHeadSHA, triggerCheckName, "success")

	h.LLM.OnUserMessage("demo-org/platform#42").
		Reply(e2e.ToolUse("claim_trigger_status", map[string]any{}))
	h.LLM.OnToolResult("claim_trigger_status", concludedClaim).
		Reply(e2e.EndTurn())

	body := h.LoadFixture(t, "pull_request_opened.json")
	require.Equal(t, http.StatusAccepted, h.PostWebhook(t, ch, body, h.SignWebhook(t, ch, body)).StatusCode)

	sessions := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, sessions, 1)
	// One post, not zero: the session-opening line names the trigger and roots
	// the thread BEFORE the first agent output, so it cannot be conditional on
	// the agent going on to say something. What this test is about — that an
	// already-answered commit produces no REVIEW and no second check run —
	// is unchanged.
	//
	// Waited for before it is held, because the hold fails on ANY deviation
	// from one, including the zero it starts at. EventuallySessions returns
	// when the AgentSession exists, and the opening line is published a NATS
	// hop later; on an idle box it wins that hop and on a loaded one it does
	// not, which is a coin toss rather than a claim about dedup.
	require.Len(t, h.EventuallySlackPosts(t, 1, 60*time.Second), 1)
	h.ConsistentlySlackPosts(t, 1, 20*time.Second)
	// The hold above happens to outlast this round, but it is a claim about
	// Slack and not a barrier — shorten it and the sample below silently starts
	// racing the last tool call. The settle point is stated explicitly so that
	// every AssertAllRulesConsumed in this file rests on the same thing.
	h.WaitForSessionPhase(sessions[0].Namespace, sessions[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)
	h.AssertAllRulesConsumed()

	creates, updates := h.FakeGitHub.CheckRunCalls()
	assert.Zero(t, creates, "an already-answered commit must not be re-opened")
	assert.Zero(t, updates, "nor re-concluded")

	run := h.FakeGitHub.CheckRunFor(triggerHeadSHA, triggerCheckName)
	require.NotNil(t, run)
	assert.Equal(t, "success", run.Conclusion, "the existing answer stands untouched")
}

// TestReviewbotTriggerStatus_WorkIsNotDoneWhileThePullRequestHasNoAnswer is the
// completion gate on a REAL github-triggered session — the only place the
// `trigger-status-concluded` requirement can be exercised end to end, because
// its gate is the input binding's kind and only a real webhook-spawned session
// has one.
//
// The scripted agent reproduces the observed failure exactly: it claims the
// check run, delivers its review to the thread, and then calls
// agent_work_complete one step early. What it gets back is a refusal naming the
// call it skipped — which is the only reason the next rule can match at all,
// since ResultContains would not match a successful completion — and only after
// concluding does the round actually end.
func TestReviewbotTriggerStatus_WorkIsNotDoneWhileThePullRequestHasNoAnswer(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")
	h.FakeGitHub.SetHeadSHA(triggerHeadSHA)

	h.LLM.OnUserMessage("demo-org/platform#42").
		Reply(e2e.ToolUse("claim_trigger_status", map[string]any{}))
	h.LLM.OnToolResult("claim_trigger_status", unconcludedClaim).
		Reply(e2e.RespondToUser("Reviewed PR #42: nothing blocking."))
	// One step early: the review is delivered and the agent calls it done.
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "Reviewed PR #42."}))
	h.LLM.OnToolResult("agent_work_complete", e2e.ResultContains("conclude_trigger_status")).
		Reply(e2e.ToolUse("conclude_trigger_status", map[string]any{
			"outcome": "clean",
			"summary": "Nothing blocking.",
		}))
	// The second agent_work_complete ends the round, so the loop asks the
	// provider nothing further and this is the last rule.
	h.LLM.OnToolResult("conclude_trigger_status", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "Reviewed PR #42."}))

	body := h.LoadFixture(t, "pull_request_opened.json")
	require.Equal(t, http.StatusAccepted, h.PostWebhook(t, ch, body, h.SignWebhook(t, ch, body)).StatusCode)

	sessions := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, sessions, 1)
	// The round ENDING is what this test waits on, rather than a count of what
	// landed in Slack: the gate's whole subject is when agent_work_complete is
	// allowed to take effect, and anything else the round happens to post is
	// beside the point.
	h.WaitForSessionPhase(sessions[0].Namespace, sessions[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)
	// Every rule fired, including the one that could only match a refusal.
	h.AssertAllRulesConsumed()

	run := h.FakeGitHub.CheckRunFor(triggerHeadSHA, triggerCheckName)
	require.NotNil(t, run, "the round the gate forced back is the one that answered the pull request")
	assert.Equal(t, "completed", run.Status)
	assert.Equal(t, "success", run.Conclusion)
}

// TestReviewbotTriggerStatus_ComposedPublicationKeepsModelTextOffThePullRequest:
// the class opts into publishedText: composed and the scripted model calls
// conclude_trigger_status WITH a summary and its own details link anyway —
// the shape a chatty or prompt-injected model produces. What lands on the
// check run is the composed text and the framework's link (none here): no
// model-authored byte, which is the entire guarantee. The outcome enum is
// the one model-chosen fact that still reaches the surface, by design.
func TestReviewbotTriggerStatus_ComposedPublicationKeepsModelTextOffThePullRequest(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")
	h.FakeGitHub.SetHeadSHA(triggerHeadSHA)

	// Opt the class into composed-only publication, as the shipped reviewbot
	// example does, and wait for re-validation BEFORE the webhook spawns a
	// session that assembles tools against it.
	var class spiceboxv1alpha1.AgentClass
	require.NoError(t, h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: ch.Namespace, Name: "demo-reviewbot"}, &class))
	class.Spec.Capabilities = map[string]apiextensionsv1.JSON{
		"trigger_status": {Raw: []byte(`{"publishedText":"composed"}`)},
	}
	require.NoError(t, h.K8s.Update(context.Background(), &class))
	h.WaitForAgentClassValid("demo-reviewbot", 30*time.Second)

	h.LLM.OnUserMessage("demo-org/platform#42").
		Reply(e2e.ToolUse("claim_trigger_status", map[string]any{}))
	h.LLM.OnToolResult("claim_trigger_status", unconcludedClaim).
		Reply(e2e.RespondToUser("Reviewed PR #42: an auth bypass in the login path."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("conclude_trigger_status", map[string]any{
			"outcome":     "problems_found",
			"summary":     "Auth bypass in login.go: the session check is skipped.",
			"details_url": "https://attacker.example/exfil?f=auth-bypass",
		}))
	h.LLM.OnToolResult("conclude_trigger_status", e2e.AnyResult()).
		Reply(e2e.EndTurn())

	body := h.LoadFixture(t, "pull_request_opened.json")
	require.Equal(t, http.StatusAccepted, h.PostWebhook(t, ch, body, h.SignWebhook(t, ch, body)).StatusCode)

	sessions := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, sessions, 1)
	h.WaitForSessionPhase(sessions[0].Namespace, sessions[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)
	h.AssertAllRulesConsumed()

	run := h.FakeGitHub.CheckRunFor(triggerHeadSHA, triggerCheckName)
	require.NotNil(t, run, "the conclusion must still land — composed mode changes WHOSE words, not whether")
	assert.Equal(t, "completed", run.Status)
	assert.Equal(t, "action_required", run.Conclusion,
		"the outcome enum is the one model-chosen fact allowed through")
	require.NotNil(t, run.Output)
	assert.NotEmpty(t, run.Output.Summary, "GitHub refuses an empty summary; the composed text must satisfy it")
	assert.NotContains(t, run.Output.Summary, "login.go",
		"no model-authored byte may reach the pull request")
	assert.NotContains(t, run.Output.Summary, "bypass")
	assert.NotContains(t, run.DetailsURL, "attacker.example",
		"the model's own link must never be published")
}
