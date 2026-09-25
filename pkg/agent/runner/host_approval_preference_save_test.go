package runner

// host_approval_preference_save_test.go is the Task 12 keystone for
// buildPreferenceSavePending's audience mapping — the part of this task
// flagged for careful verification: the preference_save interaction MUST be
// addressed (Audience.Requester) to the CURRENT TURN's author, in a shape
// that survives interaction_decision.go's DecideRequester standing check
// (identity.FromExternal(...).Canonical() equality against the clicking
// decider), not merely one that passes InteractionRequestPayload.Validate.

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
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// preferenceSaveAsk is the shared representative preference_save ApprovalAsk,
// matching exactly what preferencesSaver.Save publishes.
func preferenceSaveAsk(value json.RawMessage) pipeline.ApprovalAsk {
	payload := map[string]any{
		"key":     "language",
		"display": `Save "language: de" as your default for this agent?`,
	}
	if value != nil {
		payload["value"] = value
	}
	return pipeline.ApprovalAsk{
		Kind:    "preference_save",
		Summary: payload["display"].(string),
		Payload: payload,
	}
}

// emailCanonicalSubject builds a "user:<base64(email)>" identity.Subject the
// same way a verified-email login canonicalizes — mirroring how Turn.Author
// is populated in production (never a bare CanonicalFromTrusted passthrough,
// which is a DIFFERENT, opaque-id shape DecodeForDisplay cannot recover an
// email from).
func emailCanonicalSubject(t *testing.T, email string) identity.Subject {
	t.Helper()
	canon, err := identity.VerifiedEmail(identity.Email(email), "Test User").Canonical()
	require.NoError(t, err)
	return canon.Subject()
}

// TestBuildPreferenceSavePending_AddressesCurrentTurnAuthor is the keystone:
// the built interaction_request must be AudienceRequester-scoped to the
// loop's lastInboundAuthor, in the raw Kind/Email/ExternalID shape
// interaction_decision.go's DecideRequester check re-derives a canonical
// from — proving the standing check that runs at decision time can actually
// succeed for the SAME person the card was shown to.
func TestBuildPreferenceSavePending_AddressesCurrentTurnAuthor(t *testing.T) {
	author := emailCanonicalSubject(t, "alice@example.com")
	var published channelevents.Envelope
	l := &Loop{
		Status:            LocalStatusPatcher(),
		Approval:          approval.New(),
		lastInboundAuthor: author,
		InteractionRequestPublish: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			assert.Equal(t, "ns", ns)
			assert.Equal(t, "s", name)
			published = env
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), preferenceSaveAsk(json.RawMessage(`"de"`)))
	require.NoError(t, err)
	require.NotEmpty(t, reqID)

	pa, ok := h.pending().m[reqID]
	require.True(t, ok, "the pending approval must be registered under reqID")
	require.NoError(t, pa.onPublish(context.Background()))

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	assert.Equal(t, categories.UserPreferenceConfirm, pl.Category)
	assert.Equal(t, reqID, pl.RequestRef)
	assert.Equal(t, `Save "language: de" as your default for this agent?`, pl.Lead)

	// Audience: requester-scoped, addressed to the CURRENT TURN author.
	assert.Equal(t, channelevents.AudienceRequester, pl.Audience.Scope)
	require.NotNil(t, pl.Audience.Requester)
	assert.Equal(t, identity.Kind("idp"), pl.Audience.Requester.Kind)
	assert.Equal(t, identity.Email("alice@example.com"), pl.Audience.Requester.Email)
	assert.Equal(t, identity.RawExternalID("alice@example.com"), pl.Audience.Requester.ExternalID)

	// THE keystone assertion: interaction_decision.go:126's DecideRequester
	// check re-derives a canonical from EXACTLY these raw fields and compares
	// it against the clicking decider's own canonical. Prove the two would
	// actually match for the SAME person (alice), addressed via a DIFFERENT
	// channel Kind than the one she is stored under here (Slack, say) — email
	// alone must be enough, per Principal.Canonical()'s precedence.
	reqCanon, err := identity.FromExternal(
		identity.Kind(pl.Audience.Requester.Kind), identity.TeamScope(pl.Audience.Requester.TeamScope),
		identity.RawExternalID(pl.Audience.Requester.ExternalID), identity.Email(pl.Audience.Requester.Email),
	).Canonical()
	require.NoError(t, err)
	deciderCanon, err := identity.FromExternal(
		"slack", "", "U_ALICE", "alice@example.com",
	).Canonical()
	require.NoError(t, err)
	assert.Equal(t, deciderCanon.String(), reqCanon.String(),
		"the built Requester must canonicalize to the SAME id a real click's decider (any channel Kind, same email) would — this is what makes DecideRequester's standing check actually pass")

	// Details carries the wire contract exactly: key/value/display.
	var det struct {
		Key     string          `json:"key"`
		Value   json.RawMessage `json:"value,omitempty"`
		Display string          `json:"display,omitempty"`
	}
	require.NoError(t, json.Unmarshal(pl.Details, &det))
	assert.Equal(t, "language", det.Key)
	assert.JSONEq(t, `"de"`, string(det.Value))
	assert.Equal(t, `Save "language: de" as your default for this agent?`, det.Display)

	require.Len(t, pl.Actions, 2)
	assert.Equal(t, "approve", pl.Actions[0].ID)
	assert.Equal(t, "deny", pl.Actions[1].ID)
	assert.True(t, pl.Interruptible)
	require.NotNil(t, pl.ExpiresAt)

	// Wire-contract sanity: the built payload must pass Validate() too.
	assert.NoError(t, pl.Validate())
}

// TestBuildPreferenceSavePending_ClearOmitsValueFromDetails proves a clear
// (no "value" in ask.Payload) produces Details with NO "value" key at all —
// never a JSON null — matching Task 11's channelsd decoder contract exactly.
func TestBuildPreferenceSavePending_ClearOmitsValueFromDetails(t *testing.T) {
	author := emailCanonicalSubject(t, "alice@example.com")
	var published channelevents.Envelope
	l := &Loop{
		Status:            LocalStatusPatcher(),
		Approval:          approval.New(),
		lastInboundAuthor: author,
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			published = env
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), preferenceSaveAsk(nil))
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(pl.Details, &raw))
	_, hasValue := raw["value"]
	assert.False(t, hasValue, `Details must OMIT "value" entirely on a clear, never send it as null`)
	assert.Contains(t, raw, "key")
	assert.Contains(t, raw, "display")
}

// TestBuildPreferenceSavePending_NoCurrentTurnAuthor_FailsClosed proves a
// session with no addressable current-turn author (kubectl-driven, or a
// human turn that has not yet drained) refuses to publish an undeliverable —
// and un-decidable — confirm, rather than parking forever.
func TestBuildPreferenceSavePending_NoCurrentTurnAuthor_FailsClosed(t *testing.T) {
	l := &Loop{
		Status:   LocalStatusPatcher(),
		Approval: approval.New(),
		// lastInboundAuthor deliberately left empty.
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	_, err := h.PublishApproval(context.Background(), preferenceSaveAsk(json.RawMessage(`"de"`)))
	require.Error(t, err, "no addressable current-turn author must fail closed")
	assert.Contains(t, err.Error(), "current-turn author")
}

// TestBuildPreferenceSavePending_SyntheticAuthor_FailsClosed proves a
// current-turn author whose canonical carries no recoverable email (a guest /
// foreign-workspace identity minted via the synthetic channel-native
// encoding) is ALSO refused at publish — mirroring
// interaction_decision.go's own DecideRequester refusal of a synthetic
// decider (categories.go: "DecideRequester also refuses synthetic subjects,
// fail-closed"). An unaddressable requester fails at the same conceptual
// point, just earlier, with a clearer error than publishing into the void.
func TestBuildPreferenceSavePending_SyntheticAuthor_FailsClosed(t *testing.T) {
	synthetic, err := identity.FromExternal("slack", "T1", "U123", "").AllowSynthetic().Canonical()
	require.NoError(t, err)
	l := &Loop{
		Status:                    LocalStatusPatcher(),
		Approval:                  approval.New(),
		lastInboundAuthor:         synthetic.Subject(),
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	_, perr := h.PublishApproval(context.Background(), preferenceSaveAsk(json.RawMessage(`"de"`)))
	require.Error(t, perr, "a synthetic (email-less) current-turn author must fail closed, not publish an unaddressable ask")
}

// TestAwaitDecision_PreferenceSave_Approve drives preference_save through
// PublishApproval → AwaitDecision (approve), asserting the decision resolves
// with no post-approval effect error (the commit is channelsd's).
func TestAwaitDecision_PreferenceSave_Approve(t *testing.T) {
	orch := approval.New()
	author := emailCanonicalSubject(t, "alice@example.com")
	l := &Loop{
		Status:                    LocalStatusPatcher(),
		Approval:                  orch,
		lastInboundAuthor:         author,
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), preferenceSaveAsk(json.RawMessage(`"de"`)))
	require.NoError(t, err)

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "alice-canonical"})
	}()

	approved, by, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	assert.True(t, approved)
	assert.False(t, timedOut)
	assert.Equal(t, "alice-canonical", by)
}

// TestAwaitDecision_PreferenceSave_Deny mirrors the approve test for denial.
func TestAwaitDecision_PreferenceSave_Deny(t *testing.T) {
	orch := approval.New()
	author := emailCanonicalSubject(t, "alice@example.com")
	l := &Loop{
		Status:                    LocalStatusPatcher(),
		Approval:                  orch,
		lastInboundAuthor:         author,
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), preferenceSaveAsk(json.RawMessage(`"de"`)))
	require.NoError(t, err)

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: false, ApproverID: "alice-canonical"})
	}()

	approved, by, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	assert.False(t, approved)
	assert.False(t, timedOut)
	assert.Equal(t, "alice-canonical", by)
}

// TestAwaitDecision_PreferenceSave_TimeoutPublishesInteractionAppliedExpired
// verifies a lapsed preference_save confirm clears channelsd's pending
// surface (interaction_applied, category user_preference_confirm, outcome
// expired) rather than stranding it — same behavior as every other
// host-driven approval kind's timeout path.
func TestAwaitDecision_PreferenceSave_TimeoutPublishesInteractionAppliedExpired(t *testing.T) {
	orch := approval.New()
	author := emailCanonicalSubject(t, "alice@example.com")
	var applied []channelevents.Envelope
	l := &Loop{
		Status:                    LocalStatusPatcher(),
		Approval:                  orch,
		lastInboundAuthor:         author,
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
		TimeoutAppliedPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			applied = append(applied, env)
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), preferenceSaveAsk(json.RawMessage(`"de"`)))
	require.NoError(t, err)

	approved, _, timedOut, err := h.AwaitDecision(context.Background(), reqID, 20*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, timedOut)
	assert.False(t, approved)

	require.Len(t, applied, 1, "timeout must publish exactly one applied envelope")
	assert.Equal(t, channelevents.KindInteractionApplied, applied[0].Kind)
	var pl channelevents.InteractionAppliedPayload
	require.NoError(t, json.Unmarshal(applied[0].Payload, &pl))
	assert.Equal(t, categories.UserPreferenceConfirm, pl.Category)
	assert.Equal(t, reqID, pl.RequestRef)
	assert.Equal(t, channelevents.OutcomeExpired, pl.Outcome)
}
