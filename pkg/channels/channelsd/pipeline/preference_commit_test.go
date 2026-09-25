package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// recordingPreferenceCommitter is the fake PreferenceCommitter: it records
// every call so a test can assert on the exact request shape, and returns
// failErr (if set) to exercise the committer-error path.
type recordingPreferenceCommitter struct {
	calls   []recordedCommit
	failErr error
}

type recordedCommit struct {
	ns, name string
	req      preferences.CommitRequest
}

func (c *recordingPreferenceCommitter) CommitPreference(_ context.Context, ns, name string, req preferences.CommitRequest) error {
	c.calls = append(c.calls, recordedCommit{ns: ns, name: name, req: req})
	if c.failErr != nil {
		return c.failErr
	}
	return nil
}

// preferenceDecider is the fixture decider identity used across this file's
// tests. Its canonical form is derived the same way the decision pipe derives
// deciderCanon (interaction_decision.go:126) — via identity.FromExternal(...)
// .Canonical() — never hardcoded, so a change to the encoding cannot silently
// desync the fixture from what the handler actually computes.
func preferenceDecider() channelevents.ExternalIdentity {
	return channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
}

func preferenceDeciderCanonical(t *testing.T) string {
	t.Helper()
	decider := preferenceDecider()
	canon, err := identity.FromExternal(
		identity.Kind(decider.Kind), identity.TeamScope(decider.TeamScope),
		identity.RawExternalID(decider.ExternalID), identity.Email(decider.Email),
	).Canonical()
	require.NoError(t, err)
	return canon.String()
}

// preferenceConfirmDecision builds a user_preference_confirm Decision whose
// cached Request carries the given Details — channelsd's OWN copy of what
// the card displayed, exactly as the decision pipe recovers it from the
// parked-prompt record (interaction_decision.go's rec/req plumbing).
func preferenceConfirmDecision(actionID string, details []byte) channelinteractions.Decision {
	return channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   "user_preference_confirm",
			RequestRef: "req-1",
			ActionID:   actionID,
			Decider:    preferenceDecider(),
		},
		Request: &channelevents.InteractionRequestPayload{
			Category:   "user_preference_confirm",
			RequestRef: "req-1",
			Details:    details,
		},
	}
}

func marshalPreferenceDetails(t *testing.T, key string, value json.RawMessage, display string) []byte {
	t.Helper()
	m := map[string]any{"key": key, "display": display}
	if value != nil {
		m["value"] = value
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func preferenceCommitPipeline(committer *recordingPreferenceCommitter) *Pipeline {
	return &Pipeline{PreferenceCommitter: committer, Mem: nil}
}

func TestPreferenceCommitHandler_Approve_CommitsExactlyOnceWithCachedDetailsAndVerifiedDecider(t *testing.T) {
	committer := &recordingPreferenceCommitter{}
	det := marshalPreferenceDetails(t, "language", json.RawMessage(`"de"`), `Save "language: de" as your default?`)
	d := preferenceConfirmDecision("approve", det)

	out, err := preferenceCommitHandler(preferenceCommitPipeline(committer))(context.Background(), d)
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)

	require.Len(t, committer.calls, 1, "exactly one commit call")
	call := committer.calls[0]
	assert.Equal(t, "default", call.ns)
	assert.Equal(t, "demo-session", call.name)
	assert.Equal(t, "language", call.req.Key)
	require.NotNil(t, call.req.Value)
	assert.JSONEq(t, `"de"`, string(call.req.Value.Raw))
	assert.Equal(t, preferenceDeciderCanonical(t), call.req.Subject,
		"Subject must be the verified decider's own canonical id, bare (no user: prefix)")
}

func TestPreferenceCommitHandler_Deny_CommitsNothing(t *testing.T) {
	committer := &recordingPreferenceCommitter{}
	det := marshalPreferenceDetails(t, "language", json.RawMessage(`"de"`), `Save "language: de" as your default?`)
	d := preferenceConfirmDecision("deny", det)

	out, err := preferenceCommitHandler(preferenceCommitPipeline(committer))(context.Background(), d)
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeDenied, out.Result)
	assert.Empty(t, committer.calls, "a denial must never commit")
}

func TestPreferenceCommitHandler_MissingDetails_NoCommitAndErrorSurfaced(t *testing.T) {
	committer := &recordingPreferenceCommitter{}
	d := channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   "user_preference_confirm",
			RequestRef: "req-2",
			ActionID:   "approve",
			Decider:    preferenceDecider(),
		},
		// No Request (cache miss) and the pipeline's Mem is nil below, so the
		// durable fallback has nothing to recover from either.
		Request: nil,
	}

	out, err := preferenceCommitHandler(preferenceCommitPipeline(committer))(context.Background(), d)
	require.Error(t, err, "missing details must not be swallowed")
	assert.Contains(t, err.Error(), "req-2", "error must be traceable to the requestRef")
	assert.Empty(t, out.Result)
	assert.Empty(t, committer.calls, "no commit when details cannot be recovered")
}

func TestPreferenceCommitHandler_UndecodableDetails_NoCommitAndErrorSurfaced(t *testing.T) {
	committer := &recordingPreferenceCommitter{}
	d := preferenceConfirmDecision("approve", []byte(`{not-json`))

	out, err := preferenceCommitHandler(preferenceCommitPipeline(committer))(context.Background(), d)
	require.Error(t, err)
	assert.Empty(t, out.Result)
	assert.Empty(t, committer.calls)
}

func TestPreferenceCommitHandler_CommitterError_SurfacedNoRetry(t *testing.T) {
	committer := &recordingPreferenceCommitter{failErr: errors.New("preferences locked since publish")}
	det := marshalPreferenceDetails(t, "language", json.RawMessage(`"de"`), `Save "language: de" as your default?`)
	d := preferenceConfirmDecision("approve", det)

	out, err := preferenceCommitHandler(preferenceCommitPipeline(committer))(context.Background(), d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preferences locked since publish")
	assert.Empty(t, out.Result)
	require.Len(t, committer.calls, 1, "exactly one attempt — no retry loop")
}

func TestPreferenceCommitHandler_Clear_ValueAbsent_CommitsNilValue(t *testing.T) {
	committer := &recordingPreferenceCommitter{}
	det := marshalPreferenceDetails(t, "language", nil, "Clear your saved language preference?")
	d := preferenceConfirmDecision("approve", det)

	out, err := preferenceCommitHandler(preferenceCommitPipeline(committer))(context.Background(), d)
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)
	require.Len(t, committer.calls, 1)
	assert.Nil(t, committer.calls[0].req.Value, "an absent value in the cached details means clear")
}

func TestPreferenceCommitHandler_UnknownActionID_ReturnsError(t *testing.T) {
	committer := &recordingPreferenceCommitter{}
	det := marshalPreferenceDetails(t, "language", json.RawMessage(`"de"`), "Save?")
	d := preferenceConfirmDecision("bogus", det)

	_, err := preferenceCommitHandler(preferenceCommitPipeline(committer))(context.Background(), d)
	require.Error(t, err)
	assert.Empty(t, committer.calls)
}

func TestBindPreferenceCommitHandler_BindsUserPreferenceConfirmCategory(t *testing.T) {
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)

	BindPreferenceCommitHandler(preferenceCommitPipeline(&recordingPreferenceCommitter{}))

	_, bound := channelinteractions.HandlerFor("user_preference_confirm")
	assert.True(t, bound, "user_preference_confirm must have a bound decision handler")
}

var _ PreferenceCommitter = (*recordingPreferenceCommitter)(nil)
