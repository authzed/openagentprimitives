package outbound

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The registry these tests run against is the one populated by
// human_directed_test.go's blank imports (agent, bento, fake, slack) — the same
// shipped answers the relay itself asks, not a stubbed predicate.

// approvalEnvelope builds the tool_approval interaction_request a runner
// publishes for an approver, stamped with approverKind: the shape
// host_approval.go produces, where Kind comes from
// Loop.AddressableChannelKind() and the canonical rides in Subject.
func approvalEnvelope(t *testing.T, approverKind identity.Kind) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope("default", "child-1", channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			Category:   "tool_approval",
			RequestRef: "req-1",
			Lead:       "Approve this call?",
			Actions: []channelevents.InteractionAction{
				{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			},
			Audience: channelevents.InteractionAudience{
				Scope: channelevents.AudienceApprovers,
				Approvers: []channelevents.ExternalIdentity{{
					Kind:    approverKind,
					Subject: identity.Subject("user:YWxpY2VAZXhhbXBsZS5jb20"),
				}},
			},
		})
	require.NoError(t, err, "build interaction_request envelope")
	return env
}

func decodeRequest(t *testing.T, env channelevents.Envelope) channelevents.InteractionRequestPayload {
	t.Helper()
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "decode interaction_request payload")
	return pl
}

// TestRestampAudienceKinds_AgentStampedApproverBecomesTheDeliveringKind is the
// defect this function exists for. A conversational child's outbound binding is
// an `agent` Channel, so its runner stamps "agent" on an approver who is a
// PERSON; the relay then routes the card to an ancestor's slack. Slack's
// interaction sender skips every recipient whose kind is not "slack", so
// without the re-stamp `delivered` stays false, reportUndeliverable fires, and
// the child parks on a decision nobody was asked for.
func TestRestampAudienceKinds_AgentStampedApproverBecomesTheDeliveringKind(t *testing.T) {
	env := approvalEnvelope(t, "agent")

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	pl := decodeRequest(t, out)
	require.Len(t, pl.Audience.Approvers, 1)
	assert.Equal(t, identity.Kind("slack"), pl.Audience.Approvers[0].Kind,
		"the approver must be addressed by the channel the card is actually delivered through")
	assert.Equal(t, identity.Subject("user:YWxpY2VAZXhhbXBsZS5jb20"), pl.Audience.Approvers[0].Subject,
		"the Subject passthrough is untouched: it, not the tag, is what the surface resolves this recipient by")
	assert.Equal(t, "tool_approval", pl.Category, "the rest of the payload survives the round-trip")
	assert.Equal(t, "Approve this call?", pl.Lead)
	require.Len(t, pl.Actions, 1)
	assert.Equal(t, "approve", pl.Actions[0].ID)
}

// TestRestampAudienceKinds_HumanReadableStampIsLeftAlone pins the bound. A
// recipient already addressed on a surface a person reads is a genuine
// cross-surface identity, not this defect — rewriting it would be a new bug
// introduced under cover of fixing an old one.
func TestRestampAudienceKinds_HumanReadableStampIsLeftAlone(t *testing.T) {
	env := approvalEnvelope(t, "slack")

	out := restampAudienceKinds(env, "fake", registry.DeliversToHuman, logr.Discard())

	assert.JSONEq(t, string(env.Payload), string(out.Payload),
		"a human-readable stamp is left exactly as published, even when it differs from the delivering kind")
}

// TestRestampAudienceKinds_UnregisteredStampIsLeftAlone covers the "idp" viewer
// identity a proxy-exec approval carries: not a channel kind at all, so the
// predicate errors on it. The publisher meant something specific and this
// function is not the place to guess at it.
func TestRestampAudienceKinds_UnregisteredStampIsLeftAlone(t *testing.T) {
	env := approvalEnvelope(t, "idp")

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	assert.JSONEq(t, string(env.Payload), string(out.Payload),
		"a kind the registry cannot answer for must be delivered as published")
}

// TestRestampAudienceKinds_EmptyStampBecomesTheDeliveringKind covers the
// headless publisher: sessionhold's ownerIdentity leaves Kind empty when a held
// session has neither its own binding nor a resolved one, and slack's
// recipient gate rejects an empty kind exactly as it rejects "agent".
func TestRestampAudienceKinds_EmptyStampBecomesTheDeliveringKind(t *testing.T) {
	env := approvalEnvelope(t, "")

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	pl := decodeRequest(t, out)
	require.Len(t, pl.Audience.Approvers, 1)
	assert.Equal(t, identity.Kind("slack"), pl.Audience.Approvers[0].Kind)
}

// TestRestampAudienceKinds_RequesterScopeIsRestampedToo: the requester audience
// (identity_choice, credential_link, the leakage notice) is addressed by the
// same gate as the approver audience, so it needs the same correction. Without
// it, exactly the prompts a conversational child most needs a person for would
// be the ones still skipped.
func TestRestampAudienceKinds_RequesterScopeIsRestampedToo(t *testing.T) {
	env, err := channelevents.BuildEnvelope("default", "child-1", channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			Category: "identity_choice", RequestRef: "idc-1", Lead: "Choose an identity",
			Actions: []channelevents.InteractionAction{
				{ID: "agent", Label: "Agent", Kind: channelevents.ActionKindDecision},
			},
			Audience: channelevents.InteractionAudience{
				Scope: channelevents.AudienceRequester,
				Requester: &channelevents.ExternalIdentity{
					Kind: "agent", ExternalID: "U_ALICE", Email: "alice@example.com",
				},
			},
		})
	require.NoError(t, err)

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	pl := decodeRequest(t, out)
	require.NotNil(t, pl.Audience.Requester)
	assert.Equal(t, identity.Kind("slack"), pl.Audience.Requester.Kind)
	assert.Equal(t, identity.RawExternalID("U_ALICE"), pl.Audience.Requester.ExternalID,
		"the external id is the publisher's; only the surface tag is corrected")
}

// TestRestampAudienceKinds_NonRequestKindIsUntouched: interaction_applied and
// interaction_decision_rejected are human-directed too, but they name a decider
// and a clicker rather than a delivery audience. Decoding them as a request
// payload would silently drop every field they do carry.
func TestRestampAudienceKinds_NonRequestKindIsUntouched(t *testing.T) {
	env, err := channelevents.BuildEnvelope("default", "child-1", channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			Category: "tool_approval", RequestRef: "req-1", Outcome: channelevents.OutcomeApproved,
			DecidedBy: &channelevents.ExternalIdentity{Kind: "agent", Subject: identity.Subject("user:x")},
		})
	require.NoError(t, err)

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	assert.JSONEq(t, string(env.Payload), string(out.Payload), "a non-request kind is delivered byte-for-byte as published")
}

// TestRestampAudienceKinds_UndecodablePayloadIsDeliveredAsPublished: a card
// delivered with a stale tag is a behaviour that already exists; a card
// swallowed by the correction is not. Fail toward delivering.
func TestRestampAudienceKinds_UndecodablePayloadIsDeliveredAsPublished(t *testing.T) {
	env := approvalEnvelope(t, "agent")
	env.Payload = json.RawMessage(`{"audience":"not-an-object"}`)

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	assert.JSONEq(t, string(env.Payload), string(out.Payload))
}

// TestRestampAudienceKinds_EmptyResolvedKindIsANoOp: the relay never calls this
// with an empty kind (an envelope with no resolved binding was already
// dropped), but stamping "" over a real one would be strictly worse than
// leaving the publisher's guess in place.
func TestRestampAudienceKinds_EmptyResolvedKindIsANoOp(t *testing.T) {
	env := approvalEnvelope(t, "agent")

	out := restampAudienceKinds(env, "", registry.DeliversToHuman, logr.Discard())

	assert.JSONEq(t, string(env.Payload), string(out.Payload))
}

// TestRestampAudienceKinds_FieldMentionsAreRestampedToo is slack's THIRD gate
// on the tag, and the one the recipient/approver rewrite does not reach on its
// own. An info_leakage card's "Would share with" mentions are stamped by the
// runner from its INPUT binding kind (h.l.ChannelKind), which is `agent` for a
// conversational child; resolveFieldMentions skips every mention whose Kind is
// not "slack" and renders the publisher's display text instead, so the data
// owner would read inert names where the card is meant to name live people.
func TestRestampAudienceKinds_FieldMentionsAreRestampedToo(t *testing.T) {
	env, err := channelevents.BuildEnvelope("default", "child-1", channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			Category: "info_leakage", RequestRef: "leak-1", Lead: "Share this?",
			Fields: []channelevents.InteractionField{{
				Label: "Would share with",
				Value: "alice@example.com, bob@example.com",
				Mentions: []channelevents.ExternalIdentity{
					{Kind: "agent", Subject: identity.Subject("user:YWxpY2VAZXhhbXBsZS5jb20")},
					{Kind: "agent", Subject: identity.Subject("user:Ym9iQGV4YW1wbGUuY29t")},
				},
			}},
			Audience: channelevents.InteractionAudience{
				Scope: channelevents.AudienceApprovers,
				Approvers: []channelevents.ExternalIdentity{{
					Kind: "agent", Subject: identity.Subject("user:YWxpY2VAZXhhbXBsZS5jb20"),
				}},
			},
		})
	require.NoError(t, err, "build info_leakage envelope")

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	pl := decodeRequest(t, out)
	require.Len(t, pl.Fields, 1)
	require.Len(t, pl.Fields[0].Mentions, 2)
	assert.Equal(t, identity.Kind("slack"), pl.Fields[0].Mentions[0].Kind,
		"every named party must be addressed by the channel the card is delivered through")
	assert.Equal(t, identity.Kind("slack"), pl.Fields[0].Mentions[1].Kind,
		"the rewrite covers the whole mention list, not just its first entry")
	assert.Equal(t, "alice@example.com, bob@example.com", pl.Fields[0].Value,
		"the display fallback is the publisher's copy and is never rewritten")
	require.Len(t, pl.Audience.Approvers, 1)
	assert.Equal(t, identity.Kind("slack"), pl.Audience.Approvers[0].Kind,
		"the approver audience is corrected in the same pass")
}

// TestRestampAudienceKinds_EmailLessExternalIDIdentityMovesItsDerivedSubject
// states the property the doc comment's safety argument rests on, as an
// assertion rather than prose. An identity with neither Email nor Subject
// derives its canonical from base64url(kind:teamScope:externalID), so the tag
// is an INPUT to the subject and restamping moves it — slack then reads that
// externalID back out as a user id, unverified. That is only correct while the
// raw id is already native to the resolved kind, which holds today because
// every publisher that can emit this shape sources the id from the session's
// started-by annotations (see restampAudienceKinds' doc). If a future change
// makes Kind stop feeding the canonical, this test is what fails.
func TestRestampAudienceKinds_EmailLessExternalIDIdentityMovesItsDerivedSubject(t *testing.T) {
	before := channelevents.ExternalIdentity{Kind: "agent", ExternalID: "U_ALICE"}
	beforeCanon, err := before.Principal().AllowSynthetic().Canonical()
	require.NoError(t, err, "derive the pre-restamp canonical")

	env, err := channelevents.BuildEnvelope("default", "child-1", channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			Category: "identity_choice", RequestRef: "idc-2", Lead: "Choose an identity",
			Actions: []channelevents.InteractionAction{
				{ID: "agent", Label: "Agent", Kind: channelevents.ActionKindDecision},
			},
			Audience: channelevents.InteractionAudience{
				Scope: channelevents.AudienceRequester, Requester: &before,
			},
		})
	require.NoError(t, err, "build identity_choice envelope")

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	pl := decodeRequest(t, out)
	require.NotNil(t, pl.Audience.Requester)
	assert.Equal(t, identity.Kind("slack"), pl.Audience.Requester.Kind)
	assert.Equal(t, identity.RawExternalID("U_ALICE"), pl.Audience.Requester.ExternalID,
		"the raw id is the publisher's and is carried across verbatim")
	afterCanon, err := pl.Audience.Requester.Principal().AllowSynthetic().Canonical()
	require.NoError(t, err, "derive the post-restamp canonical")
	assert.NotEqual(t, beforeCanon, afterCanon,
		"with no Email and no Subject the kind feeds the canonical, so restamping MOVES the subject the surface resolves")
	assert.Equal(t, identity.CanonicalFromTrusted(base64.RawURLEncoding.EncodeToString([]byte("slack::U_ALICE")), "test fixture"), afterCanon,
		"the moved subject is the delivering kind's own synthetic form, which slack reads the raw id back out of")
}

// TestRestampAudienceKinds_EmailBearingIdentityKeepsItsSubject is the other
// half of the same property: an identity carrying an Email canonicalizes to
// base64url(email), which never reads Kind, so restamping is pure addressing
// metadata for it. This is the shape a publisher should prefer, and the test
// says why.
func TestRestampAudienceKinds_EmailBearingIdentityKeepsItsSubject(t *testing.T) {
	before := channelevents.ExternalIdentity{Kind: "agent", ExternalID: "U_ALICE", Email: "alice@example.com"}
	beforeCanon, err := before.Principal().Canonical()
	require.NoError(t, err, "derive the pre-restamp canonical")

	env, err := channelevents.BuildEnvelope("default", "child-1", channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			Category: "identity_choice", RequestRef: "idc-3", Lead: "Choose an identity",
			Actions: []channelevents.InteractionAction{
				{ID: "agent", Label: "Agent", Kind: channelevents.ActionKindDecision},
			},
			Audience: channelevents.InteractionAudience{
				Scope: channelevents.AudienceRequester, Requester: &before,
			},
		})
	require.NoError(t, err, "build identity_choice envelope")

	out := restampAudienceKinds(env, "slack", registry.DeliversToHuman, logr.Discard())

	pl := decodeRequest(t, out)
	require.NotNil(t, pl.Audience.Requester)
	assert.Equal(t, identity.Kind("slack"), pl.Audience.Requester.Kind)
	afterCanon, err := pl.Audience.Requester.Principal().Canonical()
	require.NoError(t, err, "derive the post-restamp canonical")
	assert.Equal(t, beforeCanon, afterCanon,
		"an email-bearing identity resolves to the same person before and after the tag is corrected")
}
