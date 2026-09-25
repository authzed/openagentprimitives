//go:build e2e

// TestReviewbot_WebhookSessionCarriesAnActingSubject covers the half of the
// tool-call authorization subject that only the real webhook path can prove.
//
// A GitHub delivery carries no AP user: the PR author consented to nothing and
// has no identity here, so the session has neither a current-requester
// annotation nor a started_by. Before this, the runner therefore resolved the
// EMPTY subject and every governed tool call went to SpiceDB with an empty
// subject object_id — rejected as a malformed request rather than answered as
// allow or deny, which under toolCalls.mode=permissive logs "would deny in
// enforcing mode" for a check that never ran.
//
// The fix reads the Channel's declared service subject, and needs BOTH facts
// this test pins to be true of production wiring at once (see
// serviceSubjectFor in pkg/agent/runner/loop_subjects.go): the class's derived
// userlessInput, and the session's own carried subject. The unit tests own the
// consequence; nothing but the live path owns the premises.
package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// subjectAppSlug is this test's AgentClass name, its input Channel's
// spec.github.appSlug, and therefore the name of the check run the trigger
// surface answers on. One constant for all three so they cannot drift.
const subjectAppSlug = "demo-subject-bot"

func TestReviewbot_WebhookSessionCarriesAnActingSubject(t *testing.T) {
	h := e2e.Start(t, e2e.Options{})
	ch := h.ApplyGitHubChannel(t, "demo-subject-gh", subjectAppSlug)
	// The commit the trigger surface answers for, read off the pull request
	// rather than taken from anything the agent carried.
	h.FakeGitHub.SetHeadSHA("abc123")

	body := h.LoadFixture(t, "pull_request_opened.json")
	scriptFullReview(h, "abc123", "Reviewed PR #42: looks good.", "record the completed review")

	resp := h.PostWebhook(t, ch, body, h.SignWebhook(t, ch, body))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	sessions := h.EventuallySessions(t, "pr:demo-org/platform#42", 1, 30*time.Second)
	require.Len(t, sessions, 1)
	sess := &sessions[0]

	// Premise 1: the session carries the subject it acts as, canonicalized by
	// the same pipeline branch that proved it is a non-human service identity.
	assert.Equal(t, ch.Spec.AuthzSubject, spiceboxv1alpha1.AuthzServiceSubject(sess).String(),
		"the session acts as the subject its input Channel declared")

	// …and still has no human starter. The two are deliberately exclusive:
	// started_by is `relation started_by: user` in the schema, so a service
	// subject stamped there would assert a human who does not exist.
	assert.Empty(t, spiceboxv1alpha1.ResolveStartedByCanonical(sess),
		"a webhook delivery has no human starter to attribute the session to")

	// Premise 2: the class publishes the derived "this input carries no human"
	// fact the runner gates the fallback on. Reading it rather than
	// re-deriving is what keeps this one predicate answering for every rule
	// that turns on it.
	var class spiceboxv1alpha1.AgentClass
	require.NoError(t, h.K8s.Get(context.Background(),
		types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}, &class))
	assert.True(t, class.Status.UserlessInput,
		"a github role=input Channel makes the bound class's input userless")

	// AssertAllRulesConsumed samples, it does not wait, and every governed call
	// this test is about happens on the runner's goroutine — so the round has to
	// be observed finished first. Reaching Idle also proves the whole scripted
	// review ran under the service subject rather than stalling partway through
	// it, which a sample taken mid-round would not have distinguished.
	h.WaitForSessionPhase(sess.Namespace, sess.Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 60*time.Second)

	// The round answered the pull request it was started by. Asserted here
	// rather than left to the dedup tests because this is the session whose
	// every tool call went out under the Channel's service subject: an answer on
	// the check run is that subject's authority actually reaching a provider,
	// not merely resolving.
	run := h.FakeGitHub.CheckRunFor("abc123", subjectAppSlug)
	require.NotNil(t, run, "the review that ran must have answered the pull request it ran for")
	assert.Equal(t, "completed", run.Status)
	assert.Equal(t, "success", run.Conclusion)

	h.AssertAllRulesConsumed()
}
