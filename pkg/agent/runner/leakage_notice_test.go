package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestPublishLeakageNotice_BuildsNilActionsInteractionRequest is the golden
// for Task 11 (D2): the info-leakage "logging" mode's would-block event
// publishes a nil-Actions categories.InfoLeakageNotice interaction_request —
// NOT the legacy KindInfoLeakageApprovalRequest — delivered to the requester
// only, naming the recipient the same way the approval card does.
func TestPublishLeakageNotice_BuildsNilActionsInteractionRequest(t *testing.T) {
	var published channelevents.Envelope
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		RequesterCanonicalID: func(_ context.Context, perCall identity.CanonicalUserID) (identity.Subject, error) {
			return perCall.SubjectRef(), nil
		},
		InteractionRequestPublish: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			assert.Equal(t, "ns", ns)
			assert.Equal(t, "s", name)
			published = env
			return nil
		},
	}

	taint := []infoleakagetaint.TaintRecord{
		{ResourceType: "record", ResourceID: "r1", ToolName: "get_record", Permission: "view"},
	}
	err := l.publishLeakageNotice(context.Background(), identity.CanonicalFromTrusted("requester@corp.example", "test fixture"), []string{"user:evan@corp.example"}, taint)
	require.NoError(t, err)

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind,
		"the notice must publish the generic interaction_request, not a legacy typed kind")
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	assert.Equal(t, categories.InfoLeakageNotice, pl.Category)
	assert.Equal(t, channelevents.SessionRef{Namespace: "ns", Name: "s"}, pl.AgentSessionRef)
	assert.NotEmpty(t, pl.RequestRef)
	assert.Equal(t, "Information may be shared", pl.Lead,
		"the lead carries no glyph; the surface draws the tone chip")

	// Nil Actions: a read-only FYI card, no decision leg.
	assert.Nil(t, pl.Actions, "info_leakage_notice must carry no Actions (read-only, no decision leg)")

	// Delivered to the requester only.
	assert.Equal(t, channelevents.AudienceRequester, pl.Audience.Scope)
	require.NotNil(t, pl.Audience.Requester)
	assert.Equal(t, identity.Subject("user:requester@corp.example"), pl.Audience.Requester.Subject,
		"the canonical id rides Subject, never the raw-only ExternalID (silent-delivery bug guard)")
	assert.Empty(t, pl.Audience.Requester.ExternalID.String())

	// The render NAMES the recipient — same invariant the approval card
	// (infoLeakageWouldShareWithFields) enforces.
	fields := map[string]string{}
	for _, f := range pl.Fields {
		fields[f.Label] = f.Value
	}
	assert.Contains(t, fields, "Will be shared with")
	assert.Contains(t, fields["Will be shared with"], "evan@corp.example")
	assert.Contains(t, fields, "Accessed resources")
	assert.Contains(t, fields["Accessed resources"], "record:r1")

	// No public post — a private notice, no PublicNote mechanism used.
	assert.False(t, pl.Audience.PublicNote)

	// Wire-contract sanity: nil Actions must pass Validate() (the documented
	// "read-only notice card" carve-out).
	assert.NoError(t, pl.Validate())
}

// TestInfoLeakageNoticeFields_StampsMentions is the focused unit test for the
// notice-side follow-up: infoLeakageNoticeFields' "Will be shared with" field
// must stamp Mentions with the FULL structured identity of every recipient
// (Subject = the canonical passthrough, ExternalID left empty), mirroring
// infoLeakageWouldShareWithFields — consistency across the approval card and
// its read-only notice sibling.
func TestInfoLeakageNoticeFields_StampsMentions(t *testing.T) {
	leakedTo := []string{"user:evan@corp.example"}
	fields := infoLeakageNoticeFields("slack", nil, leakedTo)

	var shareField *channelevents.InteractionField
	for i := range fields {
		if fields[i].Label == "Will be shared with" {
			shareField = &fields[i]
		}
	}
	require.NotNil(t, shareField, "a 'Will be shared with' Field must name the recipient")
	assert.Equal(t, "evan@corp.example", shareField.Value, "Value stays the display fallback")

	require.Len(t, shareField.Mentions, 1)
	assert.Equal(t, identity.Subject("user:evan@corp.example"), shareField.Mentions[0].Subject)
	assert.Empty(t, shareField.Mentions[0].ExternalID.String(),
		"ExternalID must stay empty — a canonical here is the known silent-delivery bug")
	assert.Equal(t, identity.Kind("slack"), shareField.Mentions[0].Kind)
}

// TestPublishLeakageNotice_HardErrorsOnEmptyRecipient mirrors
// buildLeakagePending's invariant: a notice naming no recipient is
// meaningless — refuse to build/deliver it rather than post a notice about
// sharing data with nobody.
func TestPublishLeakageNotice_HardErrorsOnEmptyRecipient(t *testing.T) {
	published := false
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		RequesterCanonicalID: func(_ context.Context, perCall identity.CanonicalUserID) (identity.Subject, error) {
			return perCall.SubjectRef(), nil
		},
		InteractionRequestPublish: func(context.Context, string, string, channelevents.Envelope) error {
			published = true
			return nil
		},
	}

	err := l.publishLeakageNotice(context.Background(), identity.CanonicalFromTrusted("requester@corp.example", "test fixture"), nil, nil)
	require.Error(t, err, "empty leaked_to must hard-error, not deliver an unnamed-recipient notice")
	assert.Contains(t, err.Error(), "recipient")
	assert.False(t, published, "nothing may be published for an unnamed-recipient notice")
}

// TestPublishLeakageNotice_NoRequesterIdentitySkipsSilently verifies the
// kubectl-driven-session case: an empty canonical (no channel identity to
// notify) is a silent no-op, not an error — mirrors
// InfoLeakReadDeps.Requester's documented empty-canonical contract.
func TestPublishLeakageNotice_NoRequesterIdentitySkipsSilently(t *testing.T) {
	published := false
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: "ns", Name: "s"},
		RequesterCanonicalID: func(_ context.Context, perCall identity.CanonicalUserID) (identity.Subject, error) {
			return perCall.SubjectRef(), nil
		},
		InteractionRequestPublish: func(context.Context, string, string, channelevents.Envelope) error {
			published = true
			return nil
		},
	}

	err := l.publishLeakageNotice(context.Background(), identity.CanonicalFromTrusted("", "test fixture"), []string{"user:evan@corp.example"}, nil)
	require.NoError(t, err)
	assert.False(t, published, "no requester identity ⇒ nothing to notify, not an error")
}
