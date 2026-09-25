//go:build e2e

// TestReviewbot_* proves the dedup contract end to end: GitHub holds all
// dedup state (as a Check Run on the head SHA), AP stores none, and a PR
// re-keys to the same AgentSession — so a webhook redelivery re-enters the
// same conversation rather than fanning out into a second review.
//
// The mechanism under test is NOT a transport-level dedup: pl.Deliver (real
// production code, see reviewbot_dedup.go's webhookInboundNATSHandler)
// happily delivers a redelivered event as a new turn on the existing
// session, exactly like a genuine second push would (TestReviewbot_
// SecondPushToTheSamePRJoinsTheSameSession proves that second case
// produces a SECOND post). What actually prevents the duplicate is the
// review loop itself: it calls check_review_status BEFORE doing anything
// else, and a completed "reviewbot" Check Run for that head SHA — read from
// FakeGitHub over a real HTTP round trip — makes it end the round silently,
// never calling respond_to_user. See ApplyGitHubChannel's AgentClass system
// prompt in reviewbot_dedup.go for the same ordering, stated for a real
// Claude Code loop rather than this package's ScriptedLLM stand-in.
package e2e_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// alreadyReviewed/notYetReviewed are OnToolResult predicates over
// check_review_status's parsed JSON payload ({"already_reviewed": bool,
// "sha": "..."}), shared by every test below so the two scripts can't drift
// on what "reviewed" means.
func alreadyReviewed(parsed any) bool {
	m, ok := parsed.(map[string]any)
	if !ok {
		return false
	}
	b, _ := m["already_reviewed"].(bool)
	return b
}

func notYetReviewed(parsed any) bool { return !alreadyReviewed(parsed) }

// scriptFullReview registers the five-rule sequence a NOT-yet-reviewed
// pull_request event drives: check status → (not reviewed) → respond_to_user
// → (delivered) → post_review_check_run → (recorded) → conclude_trigger_status
// → (answered) → end turn. Both writes come after the tool_result for
// respond_to_user has been observed, holding the ordering constraint the real
// AgentClass prompt states (see ApplyGitHubChannel).
//
// The two writes are not redundant and neither substitutes for the other.
// post_review_check_run is this fixture's OWN dedup marker — a check run it
// names itself, written straight to the provider through a tool AP has no
// contract with, and therefore invisible to the framework. The conclude call
// answers the trigger on the App's own check run and leaves the record the
// class's `trigger-status-concluded` requirement reads. A round that wrote only
// the first ended with the trigger unanswered by every measure the framework
// has, which is exactly what the requirement is declared to prevent.
func scriptFullReview(h *e2e.Harness, sha, replyText, checkRunArgsReason string) {
	h.LLM.OnUserMessage("demo-org/platform#42").
		Reply(e2e.ToolUse("check_review_status", map[string]any{"sha": sha, "_reason": "new pull_request event"}))
	h.LLM.OnToolResult("check_review_status", notYetReviewed).
		Reply(e2e.RespondToUser(replyText))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("post_review_check_run", map[string]any{
			"sha": sha, "conclusion": "success", "_reason": checkRunArgsReason,
		}))
	h.LLM.OnToolResult("post_review_check_run", e2e.AnyResult()).
		Reply(e2e.ToolUse("conclude_trigger_status", map[string]any{
			"outcome": "clean",
			"summary": "Reviewed the pull request at " + sha + "; nothing blocking.",
		}))
	// ResultContains, not AnyResult: a conclusion that failed to publish comes
	// back as an error result, and matching it anyway would let the script sail
	// past the one write this chain exists to make.
	h.LLM.OnToolResult("conclude_trigger_status", e2e.ResultContains("recorded on")).
		Reply(e2e.EndTurn())
}

// scriptAlreadyReviewed registers the two-rule sequence an ALREADY-reviewed
// redelivery drives: check status → (reviewed) → end turn, with no
// respond_to_user, no post_review_check_run and no conclude_trigger_status
// call at all. This is the mechanism the dedup contract rests on.
//
// It stays a bare end-turn under the class's `trigger-status-concluded`
// requirement, and that is a claim rather than an oversight: the answer this
// session owes was already given in the round that reviewed the commit, and it
// is recorded on the session, so the completion gate has nothing outstanding to
// refuse. A redelivery that did nothing is not a round that left a debt, and
// the gate must not charge it for one.
func scriptAlreadyReviewed(h *e2e.Harness, sha string) {
	h.LLM.OnUserMessage("demo-org/platform#42").
		Reply(e2e.ToolUse("check_review_status", map[string]any{"sha": sha, "_reason": "redelivered pull_request event"}))
	h.LLM.OnToolResult("check_review_status", alreadyReviewed).
		Reply(e2e.EndTurn())
}

// TestReviewbot_RedeliveryProducesNoSecondSessionAndNoSecondPost is the
// central claim: GitHub holds the dedup state, so an identical webhook
// redelivery — GitHub retrying, a network blip, an operator replaying a
// delivery — produces no second session and no second Slack post.
func TestReviewbot_RedeliveryProducesNoSecondSessionAndNoSecondPost(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")
	// The head commit the pull-request read reports. The trigger surface
	// resolves the commit it answers for from here and from nowhere the agent
	// touched, so a scenario whose round answers its trigger has to state one.
	h.FakeGitHub.SetHeadSHA("abc123")

	body := h.LoadFixture(t, "pull_request_opened.json")
	sig := h.SignWebhook(t, ch, body)

	// First delivery: not yet reviewed → full review, delivered to Slack,
	// THEN the completed Check Run is recorded and the trigger answered.
	scriptFullReview(h, "abc123", "Reviewed PR #42: looks good.", "record the completed review")
	// Redelivery: check_review_status now finds the completed run → silent
	// end, no second Slack post.
	scriptAlreadyReviewed(h, "abc123")

	resp := h.PostWebhook(t, ch, body, sig)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	sessions := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, sessions, 1)
	posts := h.EventuallySlackPosts(t, 2, 60*time.Second)
	require.Len(t, posts, 2)
	// Wait for the ROUND to settle, not just for its Slack output: the last two
	// tool calls of the review land after the post, and the redelivery below
	// must arrive at a session that has finished answering rather than race it.
	h.WaitForSessionPhase(sessions[0].Namespace, sessions[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)

	// What the first delivery left on the pull request, which is what makes the
	// redelivery's silence correct rather than merely quiet.
	first := h.FakeGitHub.CheckRunFor("abc123", triggerCheckName)
	require.NotNil(t, first, "the review that ran must have answered the pull request it ran for")
	assert.Equal(t, "completed", first.Status)
	assert.Equal(t, "success", first.Conclusion)
	createsBefore, updatesBefore := h.FakeGitHub.CheckRunCalls()

	// GitHub now reports a completed reviewbot check run for this SHA — the
	// ONLY place the "already reviewed" fact is stored. (post_review_check_run
	// already wrote the same fact above; this call is an idempotent
	// reinforcement, matching the brief's literal step and making the test's
	// intent explicit regardless of exactly when the write landed.)
	h.FakeGitHub.SetCheckRunConclusion("abc123", "reviewbot", "success")

	// Identical redelivery, exactly as GitHub retries.
	resp = h.PostWebhook(t, ch, body, sig)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	h.ConsistentlySessions(t, "pr:demo-org/platform#42", 1, 20*time.Second)
	h.ConsistentlySlackPosts(t, 2, 20*time.Second)

	// The other half of "no second post", and the half a completion requirement
	// could plausibly have broken: the redelivery round ends without writing
	// anything back to the pull request. The answer the session already gave is
	// recorded on it and replays across the runner restart between deliveries,
	// so the gate lets a do-nothing round end rather than demanding a second
	// conclusion for a commit that already has one.
	createsAfter, updatesAfter := h.FakeGitHub.CheckRunCalls()
	assert.Equal(t, createsBefore, createsAfter, "a redelivery must not open a second check run")
	assert.Equal(t, updatesBefore, updatesAfter, "nor re-answer the one the first delivery concluded")

	// No WaitForSessionPhase here, unlike every other AssertAllRulesConsumed in
	// these files, and deliberately: this is a SECOND round on a session the
	// first round already left at Idle, so waiting for Idle can be satisfied by
	// the phase the redelivery has not moved off yet and would be a barrier in
	// appearance only. The forty seconds of Consistently above are the real one,
	// and they are load-bearing for the assertions rather than incidental.
	h.AssertAllRulesConsumed()
}

// TestReviewbot_PRAuthorOwnsTheSessionOnceIdentityLinks proves the whole
// additional-owner chain against real production code: the verified delivery
// names the PR author's numeric account (channelsd's TriggerOwnerProvider
// derivation → the trigger-owner annotation), the operator writes
// agentsession#owner@github_user:<id>#user, and the guardian's composed
// schema admits it (the github kind's SessionRelationLinks) — so the write
// SUCCEEDS but resolves to NOBODY until the author links a verified GitHub
// credential, at which point standing lights up retroactively with no
// rewrite. The attested edge here is written directly, standing in for the
// useridentity reconciler that mints it in production.
func TestReviewbot_PRAuthorOwnsTheSessionOnceIdentityLinks(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")
	h.FakeGitHub.SetHeadSHA("abc123")

	body := h.LoadFixture(t, "pull_request_opened.json")
	scriptFullReview(h, "abc123", "Reviewed PR #42: looks good.", "record the completed review")

	resp := h.PostWebhook(t, ch, body, h.SignWebhook(t, ch, body))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	sessions := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, sessions, 1)
	h.WaitForSessionPhase(sessions[0].Namespace, sessions[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)

	// The annotation channelsd stamped from the verified payload — the fixture's
	// pull_request.user.id, an account GitHub named, not the model and not the
	// submitter-authored text.
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(t.Context(),
		client.ObjectKey{Namespace: sessions[0].Namespace, Name: sessions[0].Name}, &sess))
	assert.Equal(t, "github_user:9906581#user",
		sess.Annotations[spiceboxv1alpha1.AnnotationTriggerOwnerSubject])

	// Before the identity link: the author's platform user has NO standing —
	// the owner tuple exists but its subject-set is empty. FullyConsistent so
	// the reads cannot lag the operator's write.
	author, err := identity.EmailReference("demo-contributor@example.test").Canonical()
	require.NoError(t, err)
	approves, err := h.SpiceDB.CheckApprove(t.Context(), sess.Namespace, sess.Name, author, true)
	require.NoError(t, err)
	require.False(t, approves,
		"an unlinked GitHub account must confer nothing; if this holds standing already, the "+
			"owner write bound a user directly instead of the github_user subject-set")

	// The identity link — in production, minted by the useridentity reconciler
	// from a verified GitHub credential on the author's own UserIdentity.
	require.NoError(t, h.SpiceDB.TouchAttestedIdentity(t.Context(), "github_user", "9906581", author))

	approves, err = h.SpiceDB.CheckApprove(t.Context(), sess.Namespace, sess.Name, author, true)
	require.NoError(t, err)
	assert.True(t, approves,
		"linking the account must light up the session standing retroactively: "+
			"owner ← github_user:9906581#user ← the attested edge just written")

	interacts, err := h.SpiceDB.CheckInteract(t.Context(), sess.Namespace, sess.Name, author, true)
	require.NoError(t, err)
	assert.True(t, interacts, "interact — what the artifact-view page gates on — must follow the same chain")

	h.AssertAllRulesConsumed()
}

// TestReviewbot_BadSignatureIsRejectedAndCreatesNothing proves the
// authentication boundary: an invalid HMAC never reaches the pipeline at
// all, so it can never spawn a session — distinguishing "cannot review" at
// the transport layer from a review that ran and failed.
func TestReviewbot_BadSignatureIsRejectedAndCreatesNothing(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")

	resp := h.PostWebhook(t, ch, h.LoadFixture(t, "pull_request_opened.json"), "sha256=deadbeef")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	h.ConsistentlySessions(t, "pr:demo-org/platform#42", 0, 15*time.Second)
}

// TestReviewbot_SecondPushToTheSamePRJoinsTheSameSession proves the other
// half of the contract: a synchronize event with a NEW head SHA is NOT a
// redelivery, and must add a turn to the EXISTING session rather than
// spawning a second one — the PR-scoped channelKey is what makes one Slack
// thread per pull request work, and this is the negative space the dedup
// gate above must NOT swallow (a genuinely new push must still get reviewed
// and posted).
func TestReviewbot_SecondPushToTheSamePRJoinsTheSameSession(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-reviewbot-gh", "demo-reviewbot")
	// The pull request's head as the provider reports it. It moves when the
	// second push lands, below — which is the whole reason the trigger surface
	// reads it per round instead of trusting the commit the session opened on.
	h.FakeGitHub.SetHeadSHA("abc123")

	opened := h.LoadFixture(t, "pull_request_opened.json")
	sync := h.LoadFixture(t, "pull_request_synchronize.json")

	// opened (sha=abc123): not yet reviewed → full review.
	scriptFullReview(h, "abc123", "Reviewed PR #42 at abc123: looks good.", "record the completed review")
	// synchronize (sha=def456): a genuinely NEW head SHA — also not yet
	// reviewed, so it drives the SAME full sequence a second time.
	scriptFullReview(h, "def456", "Reviewed PR #42 at def456: still looks good.", "record the completed review")

	require.Equal(t, http.StatusAccepted,
		h.PostWebhook(t, ch, opened, h.SignWebhook(t, ch, opened)).StatusCode)
	first := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, first, 1)
	h.EventuallySlackPosts(t, 2, 60*time.Second)
	h.WaitForSessionPhase(first[0].Namespace, first[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)

	// A new push: same PR, new head SHA. The provider's head moves with it, so
	// the second round's answer addresses def456 and not the commit already
	// answered for.
	h.FakeGitHub.SetHeadSHA("def456")
	require.Equal(t, http.StatusAccepted,
		h.PostWebhook(t, ch, sync, h.SignWebhook(t, ch, sync)).StatusCode)

	second := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, second, 1, "a second push must not spawn a second session")
	assert.Equal(t, first[0].Name, second[0].Name, "it is the same session, one turn later")
	h.EventuallySlackPosts(t, 3, 60*time.Second)
	assert.Equal(t, h.SlackThreadTS(t, 1), h.SlackThreadTS(t, 2),
		"both reviews land in the same thread")
	h.WaitForSessionPhase(second[0].Namespace, second[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)

	// Each push got its OWN answer on the pull request. Asserting both is what
	// distinguishes "the session answered once and the gate was satisfied
	// thereafter" from "every reviewed commit is answered for": the completion
	// requirement is per-session, so only the second check run proves the round
	// that reviewed def456 did more than inherit the first round's record.
	for _, sha := range []string{"abc123", "def456"} {
		run := h.FakeGitHub.CheckRunFor(sha, triggerCheckName)
		require.NotNil(t, run, "the round that reviewed %s left the pull request unanswered for it", sha)
		assert.Equal(t, "completed", run.Status, "check run on %s", sha)
		assert.Equal(t, "success", run.Conclusion, "check run on %s", sha)
	}

	h.AssertAllRulesConsumed()
}
