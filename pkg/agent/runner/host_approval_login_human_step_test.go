package runner

// host_approval_login_human_step_test.go drives the login_human_step confirm
// (request_human_step's blocking half) through the runner host's
// PublishApproval → AwaitDecision round-trip, mirroring the content_inspection
// suite: build-shape, approve, deny, the fail-closed timeout, and the
// no-approvers fail-closed guard.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// loginHumanStepAsk is the representative confirm ask humanStepRequester.Confirm
// publishes: kind=login_human_step with the model-authored prompt.
func loginHumanStepAsk(prompt string) pipeline.ApprovalAsk {
	return pipeline.ApprovalAsk{
		Kind:    "login_human_step",
		Summary: prompt,
		Payload: map[string]any{"prompt": prompt},
	}
}

func loginHumanStepLoop(orch *approval.Orchestrator, publish func(ns, name string, env channelevents.Envelope)) *Loop {
	return &Loop{
		Status:      LocalStatusPatcher(),
		Approval:    orch,
		ChannelKind: "slack",
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			if publish != nil {
				publish(ns, name, env)
			}
			return nil
		},
	}
}

// TestBuildLoginHumanStepPending_BuildsInteractionRequest pins the published
// card: category login_human_step, AudienceApprovers resolved from the session
// approve-set, approve/deny actions, a future ExpiresAt, Interruptible, and —
// the security-relevant part — the model-authored prompt carried in the inert
// Excerpt, never in the trusted Lead.
func TestBuildLoginHumanStepPending_BuildsInteractionRequest(t *testing.T) {
	const prompt = "united.com sent a code to your email — approve the push on your phone <!channel>"
	var published channelevents.Envelope
	l := loginHumanStepLoop(approval.New(), func(_, _ string, env channelevents.Envelope) { published = env })
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk(prompt))
	require.NoError(t, err)
	require.NotEmpty(t, reqID)

	pa, ok := h.pending().m[reqID]
	require.True(t, ok, "the pending approval must be registered under reqID")
	require.NoError(t, pa.onPublish(context.Background()))

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	assert.Equal(t, categories.LoginHumanStep, pl.Category)
	assert.Equal(t, reqID, pl.RequestRef)
	assert.Equal(t, channelevents.SessionRef{Namespace: "ns", Name: "s"}, pl.AgentSessionRef)

	assert.Equal(t, channelevents.AudienceApprovers, pl.Audience.Scope)
	require.Len(t, pl.Audience.Approvers, 1)
	assert.Equal(t, "user:owner@corp.example", pl.Audience.Approvers[0].Subject.String())
	assert.Equal(t, "slack", pl.Audience.Approvers[0].Kind.String())

	// The model-authored prompt must NOT be in the trusted Lead (rendered as live
	// markup) — it rides the Excerpt, which every surface renders inert.
	assert.NotContains(t, pl.Lead, "<!channel>", "the untrusted prompt must never land in the trusted Lead")
	require.NotNil(t, pl.Excerpt, "the model's prompt must be carried as an inert excerpt")
	assert.Contains(t, pl.Excerpt.Content, "<!channel>", "the prompt is carried verbatim in the inert excerpt")

	require.Len(t, pl.Actions, 2)
	assert.Equal(t, "approve", pl.Actions[0].ID)
	assert.Equal(t, "deny", pl.Actions[1].ID)
	assert.True(t, pl.Interruptible)
	require.NotNil(t, pl.ExpiresAt)
	assert.True(t, pl.ExpiresAt.After(time.Now()))

	assert.NoError(t, pl.Validate())
}

// TestBuildLoginHumanStepPending_FailsClosedWithoutApprovers proves the publisher
// refuses to build an undeliverable card when the session approve-set resolves to
// no one — parity with content_inspection.
func TestBuildLoginHumanStepPending_FailsClosedWithoutApprovers(t *testing.T) {
	l := &Loop{
		Status:                    LocalStatusPatcher(),
		Approval:                  approval.New(),
		ChannelKind:               "slack",
		SpiceDBLookupSubjects:     func(_ context.Context, _, _ string) ([]string, error) { return nil, nil },
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	_, err := h.PublishApproval(context.Background(), loginHumanStepAsk("approve the push"))
	require.Error(t, err, "no resolvable approvers must fail closed")
	assert.Contains(t, err.Error(), "approver")
}

// TestAwaitDecision_LoginHumanStep_Approve drives the full round-trip: publish →
// approve delivered → the tool's block resumes with approved=true.
func TestAwaitDecision_LoginHumanStep_Approve(t *testing.T) {
	orch := approval.New()
	l := loginHumanStepLoop(orch, nil)
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk("approve the push"))
	require.NoError(t, err)

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "alice"})
	}()

	approved, by, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	assert.True(t, approved)
	assert.False(t, timedOut)
	assert.Equal(t, "alice", by)
}

// TestAwaitDecision_LoginHumanStep_CapturesTypedCode is the C7 runner-host crux:
// an approval that carries a free-text code (the human's login code) is captured on
// the host so humanStepRequester.RequestCode can read it back — the carrier leg
// that lets the runner type the code into the page. The code is NOT part of
// AwaitDecision's return (approved/by/timedOut only), so it never flows through a
// path a log or the model reads; it rides out-of-band on the host and is
// take-cleared.
func TestAwaitDecision_LoginHumanStep_CapturesTypedCode(t *testing.T) {
	orch := approval.New()
	l := loginHumanStepLoop(orch, nil)
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk("enter the emailed code"))
	require.NoError(t, err)

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "alice", ResponseText: "123456"})
	}()

	approved, by, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	assert.True(t, approved)
	assert.False(t, timedOut)
	assert.Equal(t, "alice", by)

	// The code was captured on the host, take-and-clear.
	assert.Equal(t, "123456", h.takeHumanStepCode(), "the typed code must be captured on the host for RequestCode to read")
	assert.Empty(t, h.takeHumanStepCode(), "the code must be cleared after the first read (never lingers)")
}

// TestAwaitDecision_LoginHumanStep_DenyCapturesNoCode proves a deny captures no
// code — a declined card types nothing even if (malformed) text rode along.
func TestAwaitDecision_LoginHumanStep_DenyCapturesNoCode(t *testing.T) {
	orch := approval.New()
	l := loginHumanStepLoop(orch, nil)
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk("enter the code"))
	require.NoError(t, err)

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: false, ApproverID: "bob", ResponseText: "should-not-type"})
	}()

	approved, _, _, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	assert.False(t, approved)
	assert.Empty(t, h.takeHumanStepCode(), "a denied card must capture no code")
}

// TestAwaitDecision_LoginHumanStep_Deny proves an explicit decline resumes as
// approved=false (not a timeout).
func TestAwaitDecision_LoginHumanStep_Deny(t *testing.T) {
	orch := approval.New()
	l := loginHumanStepLoop(orch, nil)
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk("approve the push"))
	require.NoError(t, err)

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: false, ApproverID: "bob"})
	}()

	approved, by, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	assert.False(t, approved)
	assert.False(t, timedOut)
	assert.Equal(t, "bob", by)
}

// TestAwaitDecision_LoginHumanStep_TimeoutFailsClosed is Review Focus 5: a lapsed
// confirm resolves timedOut=true / approved=false (the call does NOT silently
// succeed) AND clears channelsd's surface with an interaction_applied(expired),
// so the browser card never strands with live buttons.
func TestAwaitDecision_LoginHumanStep_TimeoutFailsClosed(t *testing.T) {
	orch := approval.New()
	var applied []channelevents.Envelope
	l := loginHumanStepLoop(orch, nil)
	l.TimeoutAppliedPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		applied = append(applied, env)
		return nil
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk("approve the push"))
	require.NoError(t, err)

	// No decision delivered: the deadline elapses. A timeout is a sticky deny,
	// never a Halt.
	approved, _, timedOut, err := h.AwaitDecision(context.Background(), reqID, 20*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, timedOut, "a lapsed confirm must report timedOut")
	assert.False(t, approved, "a lapsed confirm must NOT silently succeed (fail-closed)")

	require.Len(t, applied, 1, "timeout must publish exactly one applied envelope to clear channelsd's surface")
	assert.Equal(t, channelevents.KindInteractionApplied, applied[0].Kind)
	var pl channelevents.InteractionAppliedPayload
	require.NoError(t, json.Unmarshal(applied[0].Payload, &pl))
	assert.Equal(t, categories.LoginHumanStep, pl.Category)
	assert.Equal(t, reqID, pl.RequestRef)
	assert.Equal(t, channelevents.OutcomeExpired, pl.Outcome)
}
