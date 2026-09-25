package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// toolCallAsk is the shared representative tool_call ApprovalAsk the migrated
// publisher tests drive: an external-effect write on repo:r1 with an args hash,
// a justification, and a summarizer "What".
func toolCallAsk() pipeline.ApprovalAsk {
	payload := ToolCallApprovalPayload{
		SessNS:        "ns",
		SessName:      "s",
		ToolName:      "write_repo",
		Permission:    "write",
		ResourceType:  "repo",
		ResourceID:    "r1",
		StateImpact:   "external",
		ArgsHash:      "h1",
		ArgsJSON:      `{"repo":"r1"}`,
		Justification: "need to land the fix",
		Perm:          authz.Permission{StateImpact: authz.External},
		UseID:         "tu-1",
	}
	return pipeline.ApprovalAsk{
		Kind:    "tool_call",
		Summary: "push a commit to repo r1",
		Payload: payload.ToMap(),
	}
}

// TestBuildToolCallPending_BuildsInteractionRequest is the keystone
// characterization for Slice C2: buildToolCallPending now emits a generic
// KindInteractionRequest (category tool_approval) on the IN subject via
// loop.InteractionRequestPublish — NOT the legacy KindToolApprovalRequest. It
// pins the security-critical shape: the resource-owner Resources set (the
// DecideResourceOwners standing input), the grant-write Details the Task-9
// handler reads back, PublicNote=true, Interruptible=true, and a durable
// memapproval record carrying the SAME Resources + Details (the D3
// cross-restart authority).
func TestBuildToolCallPending_BuildsInteractionRequest(t *testing.T) {
	orch := approval.New()
	mem := memory.NewLocal(inmem.NewBackend())
	var published channelevents.Envelope
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		ChannelKind:       "slack",
		Mem:               mem,
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			published = env
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), toolCallAsk())
	require.NoError(t, err)
	require.NotEmpty(t, reqID)

	pa, ok := h.pending().m[reqID]
	require.True(t, ok, "pending approval must be registered under reqID")
	require.NoError(t, pa.onPublish(context.Background()))

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind,
		"tool_call now publishes the generic interaction_request, not the legacy typed kind")
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	assert.Equal(t, categories.ToolApproval, pl.Category)
	assert.Equal(t, reqID, pl.RequestRef)
	assert.Equal(t, channelevents.SessionRef{Namespace: "ns", Name: "s"}, pl.AgentSessionRef)
	assert.True(t, pl.Interruptible, "tool_call interactions are interruptible")

	// Approvers resolved from the resource #owner set (Subject passthrough).
	assert.Equal(t, channelevents.AudienceApprovers, pl.Audience.Scope)
	require.Len(t, pl.Audience.Approvers, 1)
	assert.Equal(t, "user:owner@corp.example", pl.Audience.Approvers[0].Subject.String(),
		"the canonical id rides Subject, never the raw-only ExternalID (silent-delivery bug guard)")
	assert.Empty(t, pl.Audience.Approvers[0].ExternalID.String(),
		"ExternalID must stay empty — a canonical here is the known silent-delivery bug")
	assert.Equal(t, "slack", pl.Audience.Approvers[0].Kind.String())

	// PublicNote: the only category that posts one.
	assert.True(t, pl.Audience.PublicNote, "tool_call posts a public approval-pending note")
	assert.Contains(t, pl.Audience.PublicNoteBody, "wants to", "public note reproduces the legacy thread lead")

	// Resources = the resource-owner standing input (DecideResourceOwners).
	require.Len(t, pl.Resources, 1)
	assert.Equal(t, channelevents.InteractionResourceRef{Type: "repo", ID: "r1", Permission: "owner"}, pl.Resources[0])

	// Details = the exact grant-write inputs the Task-9 handler reads back.
	require.NotEmpty(t, pl.Details)
	var det channelevents.ToolApprovalDetails
	require.NoError(t, json.Unmarshal(pl.Details, &det))
	assert.Equal(t, "h1", det.ArgsHash)
	assert.Equal(t, "external", det.StateImpact)
	assert.Equal(t, "write", det.Permission)
	assert.Equal(t, "repo", det.ResourceType)
	assert.Equal(t, "r1", det.ResourceID)

	// Approve/deny decision actions.
	require.Len(t, pl.Actions, 2)
	assert.Equal(t, "approve", pl.Actions[0].ID)
	assert.Equal(t, "deny", pl.Actions[1].ID)

	// The lead carries no glyph: the surface draws the tone chip from the
	// category, so a publisher-supplied one would compete with it.
	assert.Equal(t, "Approval needed", pl.Lead)
	fields := map[string]string{}
	for _, f := range pl.Fields {
		fields[f.Label] = f.Value
	}
	assert.Contains(t, fields, "Why")
	assert.Contains(t, fields, "What")
	// Human copy, not wire identifiers. This card used to print the raw tool id
	// and permission name in backticks, on a surface a person decides from,
	// while the plan-gate card beside it resolved both to declared phrases.
	// Neither value here declares a title or a display, so both degrade to the
	// detokenized fallback rather than to schema.
	assert.Equal(t, "write repo", fields["Tool"])
	assert.Equal(t, "write on repo", fields["Permission"])
	assert.NotContains(t, fields["Tool"]+fields["Permission"], "`",
		"no backtick-wrapped wire values on an approval card")

	// Durable memapproval record (D3): carries the SAME Resources + Details.
	rec, err := memapproval.RequestByID(memory.WithSystemApproval(context.Background(), "test"), mem,
		memory.Scope{Kind: "session", ID: "ns/s"}, reqID)
	require.NoError(t, err)
	require.NotNil(t, rec, "durable request record must exist keyed by requestID")
	require.Len(t, rec.Resources, 1)
	assert.Equal(t, channelevents.InteractionResourceRef{Type: "repo", ID: "r1", Permission: "owner"}, rec.Resources[0])
	require.NotEmpty(t, rec.Details, "durable record must carry the grant-write Details")
	var durableDet channelevents.ToolApprovalDetails
	require.NoError(t, json.Unmarshal(rec.Details, &durableDet))
	assert.Equal(t, det, durableDet, "durable Details must match the interaction Details byte-for-byte")
}

// TestBuildToolCallPending_NoResourceUsesSessionGate verifies the session-gate
// branch: a tool_call whose permission names no resource carries nil Resources
// (so the decision pipe folds to the session approve-set) and resolves its
// approvers from the session approve-set.
func TestBuildToolCallPending_NoResourceUsesSessionGate(t *testing.T) {
	orch := approval.New()
	var published channelevents.Envelope
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		ChannelKind:       "slack",
		SpiceDBLookupSubjects: func(_ context.Context, resource, _ string) ([]string, error) {
			assert.Equal(t, "agentsession:ns/s", resource, "resource-less gate must look up the session approve-set")
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			published = env
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	payload := ToolCallApprovalPayload{SessNS: "ns", SessName: "s", ToolName: "noop", UseID: "tu-2"}
	reqID, err := h.PublishApproval(context.Background(), pipeline.ApprovalAsk{Kind: "tool_call", Payload: payload.ToMap()})
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))
	assert.Empty(t, pl.Resources, "no resource ⇒ nil Resources ⇒ session-approve-set gate")
	require.Len(t, pl.Audience.Approvers, 1)
}

// TestBuildToolCallPending_FailsClosedWithoutApprovers verifies the fail-closed
// guard: no resolvable approvers refuses to publish an undeliverable prompt.
func TestBuildToolCallPending_FailsClosedWithoutApprovers(t *testing.T) {
	l := &Loop{
		ResourceStandings:         testResourceStandings(),
		Status:                    LocalStatusPatcher(),
		Approval:                  approval.New(),
		ChannelKind:               "slack",
		SpiceDBLookupSubjects:     func(_ context.Context, _, _ string) ([]string, error) { return nil, nil },
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	_, err := h.PublishApproval(context.Background(), toolCallAsk())
	require.Error(t, err, "no resolvable approvers must fail closed")
	assert.Contains(t, err.Error(), "approver")
}

// TestBuildToolCallPending_TimeoutPublishesInteractionAppliedExpired verifies the
// migrated timeout path: an expired tool_call deadline publishes
// interaction_applied(tool_approval, expired), NOT the legacy
// KindToolApprovalApplied.
func TestBuildToolCallPending_TimeoutPublishesInteractionAppliedExpired(t *testing.T) {
	orch := approval.New()
	var applied []channelevents.Envelope
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		ChannelKind:       "slack",
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
		TimeoutAppliedPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			applied = append(applied, env)
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), toolCallAsk())
	require.NoError(t, err)

	approved, _, timedOut, err := h.AwaitDecision(context.Background(), reqID, 20*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, timedOut)
	assert.False(t, approved)

	require.Len(t, applied, 1)
	assert.Equal(t, channelevents.KindInteractionApplied, applied[0].Kind)
	var pl channelevents.InteractionAppliedPayload
	require.NoError(t, json.Unmarshal(applied[0].Payload, &pl))
	assert.Equal(t, categories.ToolApproval, pl.Category)
	assert.Equal(t, reqID, pl.RequestRef)
	assert.Equal(t, channelevents.OutcomeExpired, pl.Outcome)
}

// leakageAsk is the shared representative leakage_share ApprovalAsk: a proposed
// share of tainted data:d1 to one named recipient, with the data #owner set as
// the approver.
func leakageAsk(leakedTo []string) pipeline.ApprovalAsk {
	return pipeline.ApprovalAsk{
		Kind: "leakage_share",
		Payload: map[string]any{
			"sess_ns":           "ns",
			"sess_name":         "s",
			"leaked_to":         leakedTo,
			"taint":             []leakageTaintRecord{{ResourceType: "data", ResourceID: "d1", Permission: "view", ToolName: "read_data"}},
			"ttl":               (5 * time.Minute).String(),
			"proposed_text":     "Share the customer record with the requested recipient",
			"approver":          "data:d1#owner",
			"approver_subjects": []string{"data:d1#owner"},
			"on_approved":       (func(string, string))(nil),
			"on_denied":         (func(string, string))(nil),
		},
	}
}

// TestBuildLeakagePending_BuildsInteractionRequest pins the info_leakage golden:
// the migrated interaction_request has NO public post (the no-public-post
// invariant), NAMES the recipient in a "Would share with" Field, carries the
// taint data refs as Resources (the data-owner standing input), and writes a
// durable memapproval record with those Resources.
func TestBuildLeakagePending_BuildsInteractionRequest(t *testing.T) {
	orch := approval.New()
	mem := memory.NewLocal(inmem.NewBackend())
	var published channelevents.Envelope
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		ChannelKind:       "slack",
		Mem:               mem,
		SpiceDBLookupSubjects: func(_ context.Context, resource, permission string) ([]string, error) {
			assert.Equal(t, "data:d1", resource, "info_leakage resolves the data #owner set (data-owner-only standing)")
			assert.Equal(t, "owner", permission)
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			published = env
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), leakageAsk([]string{"user:evan@corp.example"}))
	require.NoError(t, err)
	require.NotEmpty(t, reqID)

	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))

	assert.Equal(t, categories.InfoLeakage, pl.Category)
	assert.Equal(t, reqID, pl.RequestRef)
	assert.True(t, pl.Interruptible)

	// (a) NO public post — a private DM to the data owner.
	assert.False(t, pl.Audience.PublicNote, "info_leakage must NEVER post publicly (no-public-post invariant)")
	assert.Empty(t, pl.Audience.PublicNoteBody)

	// Data-owner-only standing: approvers resolved from the data #owner set.
	assert.Equal(t, channelevents.AudienceApprovers, pl.Audience.Scope)
	require.Len(t, pl.Audience.Approvers, 1)
	assert.Equal(t, "user:owner@corp.example", pl.Audience.Approvers[0].Subject.String())

	// (b) The render NAMES the recipient.
	var shareField *channelevents.InteractionField
	for i := range pl.Fields {
		if pl.Fields[i].Label == "Would share with" {
			shareField = &pl.Fields[i]
		}
	}
	require.NotNil(t, shareField, "a 'Would share with' Field must name the recipient")
	assert.Contains(t, shareField.Value, "corp.example", "the named recipient appears in the render")

	// (b2) Mentions carries the FULL structured identity of the recipient
	// alongside the flattened Value (standing rule: never flatten early) — the
	// canonical rides Subject, never ExternalID (silent-delivery bug guard),
	// same shape resolveInteractionApprovers stamps Audience.Approvers with.
	require.Len(t, shareField.Mentions, 1)
	assert.Equal(t, identity.Subject("user:evan@corp.example"), shareField.Mentions[0].Subject)
	assert.Empty(t, shareField.Mentions[0].ExternalID.String(),
		"ExternalID must stay empty — a canonical here is the known silent-delivery bug")
	assert.Equal(t, identity.Kind("slack"), shareField.Mentions[0].Kind)

	// Resources = the taint data refs.
	require.Len(t, pl.Resources, 1)
	assert.Equal(t, channelevents.InteractionResourceRef{Type: "data", ID: "d1"}, pl.Resources[0])

	// Durable memapproval record carries the taint Resources (D3).
	rec, err := memapproval.RequestByID(memory.WithSystemApproval(context.Background(), "test"), mem,
		memory.Scope{Kind: "session", ID: "ns/s"}, reqID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Resources, 1)
	assert.Equal(t, channelevents.InteractionResourceRef{Type: "data", ID: "d1"}, rec.Resources[0])
}

// TestInfoLeakageWouldShareWithFields_StampsMentions is the focused unit test
// for the publisher-side follow-up: infoLeakageWouldShareWithFields must
// stamp Mentions with the FULL structured identity of every recipient
// (Subject = the canonical passthrough, ExternalID left empty — the same
// invariant resolveInteractionApprovers enforces for Audience.Approvers),
// while Value keeps carrying the flattened display-string fallback for
// surfaces that don't resolve mentions.
func TestInfoLeakageWouldShareWithFields_StampsMentions(t *testing.T) {
	leakedTo := []string{"user:evan@corp.example", "user:dana@corp.example"}
	fields := infoLeakageWouldShareWithFields("slack", leakedTo)

	require.Len(t, fields, 1)
	f := fields[0]
	assert.Equal(t, "Would share with", f.Label)
	assert.Equal(t, "evan@corp.example, dana@corp.example", f.Value, "Value stays the display fallback")

	require.Len(t, f.Mentions, 2)
	assert.Equal(t, identity.Subject("user:evan@corp.example"), f.Mentions[0].Subject)
	assert.Empty(t, f.Mentions[0].ExternalID.String())
	assert.Equal(t, identity.Kind("slack"), f.Mentions[0].Kind)
	assert.Equal(t, identity.Subject("user:dana@corp.example"), f.Mentions[1].Subject)
	assert.Empty(t, f.Mentions[1].ExternalID.String())
}

// TestBuildLeakagePending_HardErrorsOnEmptyRecipient verifies invariant (c): an
// empty leaked_to refuses to build/deliver an interaction with an unnamed
// recipient — it returns an error and publishes NOTHING.
func TestBuildLeakagePending_HardErrorsOnEmptyRecipient(t *testing.T) {
	published := false
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          approval.New(),
		ChannelKind:       "slack",
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error {
			published = true
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	_, err := h.PublishApproval(context.Background(), leakageAsk(nil))
	require.Error(t, err, "empty leaked_to must hard-error, not prompt for an unnamed share")
	assert.Contains(t, err.Error(), "recipient")
	assert.False(t, published, "nothing may be published for an unnamed-recipient share")
}

// TestBuildLeakagePending_TimeoutPublishesInteractionAppliedExpired verifies the
// migrated timeout path: an expired leakage_share deadline publishes
// interaction_applied(info_leakage, expired).
func TestBuildLeakagePending_TimeoutPublishesInteractionAppliedExpired(t *testing.T) {
	orch := approval.New()
	var applied []channelevents.Envelope
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		ChannelKind:       "slack",
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error { return nil },
		TimeoutAppliedPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			applied = append(applied, env)
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), leakageAsk([]string{"user:evan@corp.example"}))
	require.NoError(t, err)

	approved, _, timedOut, err := h.AwaitDecision(context.Background(), reqID, 20*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, timedOut)
	assert.False(t, approved)

	require.Len(t, applied, 1)
	assert.Equal(t, channelevents.KindInteractionApplied, applied[0].Kind)
	var pl channelevents.InteractionAppliedPayload
	require.NoError(t, json.Unmarshal(applied[0].Payload, &pl))
	assert.Equal(t, categories.InfoLeakage, pl.Category)
	assert.Equal(t, channelevents.OutcomeExpired, pl.Outcome)
}
