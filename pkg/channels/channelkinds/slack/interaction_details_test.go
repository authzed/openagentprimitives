package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestIncludedRequestDetailsRemainExactAndInert(t *testing.T) {
	instructions := strings.Repeat("<@U_ALICE> & exact instructions ", 180)
	raw, err := json.Marshal([]map[string]string{{"requestRef": "reminder", "instructions": instructions}})
	require.NoError(t, err)
	blocks, err := renderRawInteractionDetailsModal(raw, "plan", "ns/session")
	require.NoError(t, err)
	require.Greater(t, len(blocks), 1)
	var shown strings.Builder
	for _, block := range blocks {
		section := block.(*slackapi.SectionBlock)
		assert.Equal(t, "plain_text", section.Text.Type, "Slack cannot activate mentions or links in exact instructions")
		assert.LessOrEqual(t, len([]rune(section.Text.Text)), 2800)
		shown.WriteString(section.Text.Text)
	}
	var restored []map[string]string
	require.NoError(t, json.Unmarshal([]byte(shown.String()), &restored))
	assert.Equal(t, instructions, restored[0]["instructions"], "details must not truncate the approved request")
}

// toolApprovalDetailsJSON marshals a ToolApprovalDetails for use as an
// InteractionRequestPayload.Details / memapproval.Request.Details blob.
func toolApprovalDetailsJSON(t *testing.T, d channelevents.ToolApprovalDetails) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	return raw
}

// --- buildInteractionRequestBlocks: button presence -----------------------

// TestBuildInteractionRequestBlocks_Details_AppendsShowDetailsButton verifies
// buildInteractionRequestBlocks appends a generic Show-Details button
// (discriminator discInteractionDetails) whenever the payload carries
// Details, alongside its decision actions.
func TestBuildInteractionRequestBlocks_Details_AppendsShowDetailsButton(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{
		Category:   "tool_approval",
		RequestRef: "req-details-1",
		Lead:       "Approval needed",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
		},
		Details: toolApprovalDetailsJSON(t, channelevents.ToolApprovalDetails{
			Permission: "repo_write", ResourceType: "repo", ResourceID: "org/x",
		}),
	}
	blocks := buildInteractionRequestBlocks(pl, "default/sess-1")

	actions := interactionActionBlock(t, blocks)
	ok := actions != nil
	require.True(t, ok)
	require.Len(t, actions.Elements.ElementSet, 3, "approve + deny + Show Details")

	btn, ok := actions.Elements.ElementSet[2].(*slackapi.ButtonBlockElement)
	require.True(t, ok, "third element is the Show Details button")
	assert.Equal(t, interactionDetailsActionID, btn.ActionID)
	assert.Equal(t, "Show Details", btn.Text.Text)
	assert.Empty(t, btn.URL, "Show Details is a click, not a link")

	decoded, ok := decodeApprovalButtonValue(btn.Value)
	require.True(t, ok, "Show Details button value must decode")
	assert.Equal(t, discInteractionDetails, decoded.V)
	assert.Equal(t, "req-details-1", decoded.R, "requestRef")
	assert.Equal(t, "default/sess-1", decoded.S, "sessRef")
}

// TestBuildInteractionRequestBlocks_NoDetails_OmitsShowDetailsButton verifies
// categories that carry no Details (content_inspection, info_leakage,
// notice-only cards) render their own actions with no Show-Details button
// appended.
func TestBuildInteractionRequestBlocks_NoDetails_OmitsShowDetailsButton(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{
		Category:   "info_leakage",
		RequestRef: "req-no-details",
		Lead:       "Potential data exposure",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
		},
		// Details deliberately unset.
	}
	blocks := buildInteractionRequestBlocks(pl, "default/sess-1")

	actions := interactionActionBlock(t, blocks)
	ok := actions != nil
	require.True(t, ok)
	require.Len(t, actions.Elements.ElementSet, 2, "only approve + deny; no Show Details button")
	for _, el := range actions.Elements.ElementSet {
		btn, ok := el.(*slackapi.ButtonBlockElement)
		require.True(t, ok)
		assert.NotEqual(t, interactionDetailsActionID, btn.ActionID)
	}
}

// TestBuildInteractionRequestBlocks_NoDetailsNoActions_OmitsActionsBlockEntirely
// verifies a read-only, Details-less notice card (no decision actions
// either) renders no actions block at all — the Show-Details button must not
// resurrect an otherwise-omitted actions block.
func TestBuildInteractionRequestBlocks_NoDetailsNoActions_OmitsActionsBlockEntirely(t *testing.T) {
	pl := channelevents.InteractionRequestPayload{Lead: "FYI", RequestRef: "req-notice"}
	blocks := buildInteractionRequestBlocks(pl, "default/sess-1")
	require.Len(t, blocks, 1, "section only, no actions block")
}

// --- sendRequest: Details cached at delivery -------------------------------

// TestSendRequest_Details_CachedAtDelivery verifies sendRequest caches the
// request's Details in the shared delivery store when the prompt actually
// delivers, so the listener's Show-Details click handler can read it back
// without a memory round-trip.
func TestSendRequest_Details_CachedAtDelivery(t *testing.T) {
	restore := snapshotCategories(t)
	defer restore()
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "tool_approval", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceEphemeralDMFallback,
	})

	const slackID = "UAPPROVER"
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	details := toolApprovalDetailsJSON(t, channelevents.ToolApprovalDetails{
		Permission: "repo_write", ResourceType: "repo", ResourceID: "org/x",
		ArgsJSON: `{"branch":"main"}`,
	})
	p := channelevents.InteractionRequestPayload{
		Category: "tool_approval", RequestRef: "req-cache-1", Lead: "Approval needed",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "slack", ExternalID: identity.RawExternalID(slackID)}},
		},
		Details: details,
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)

	_, err := s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err, "Send")

	raw, ok := s.delivery.getDetails("req-cache-1")
	require.True(t, ok, "Details must be cached after a successful delivery")
	assert.JSONEq(t, string(details), string(raw))
}

// TestSendRequest_NoDetails_NothingCached verifies a payload with no Details
// leaves the details cache empty for its requestRef — recordDetails must be
// skip-on-empty, not write a spurious empty entry.
func TestSendRequest_NoDetails_NothingCached(t *testing.T) {
	restore := snapshotCategories(t)
	defer restore()
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "identity_choice", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceEphemeralDMFallback,
	})

	const slackID = "UREQUESTER"
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}

	p := channelevents.InteractionRequestPayload{
		Category: "identity_choice", RequestRef: "req-no-cache", Lead: "Which identity?",
		Actions: []channelevents.InteractionAction{
			{ID: "agent", Label: "Run as the agent", Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "slack", ExternalID: identity.RawExternalID(slackID)}},
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)

	_, err := s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err, "Send")

	_, ok := s.delivery.getDetails("req-no-cache")
	assert.False(t, ok, "no Details on the payload means nothing to cache")
}

// --- interactionDeliveryStore: Details round-trip --------------------------

func TestInteractionDeliveryStore_DetailsRoundTrip(t *testing.T) {
	st := newInteractionDeliveryStore()
	_, ok := st.getDetails("r1")
	assert.False(t, ok, "unpopulated store has nothing cached")

	st.recordDetails("r1", json.RawMessage(`{"permission":"repo_write"}`))
	raw, ok := st.getDetails("r1")
	require.True(t, ok)
	assert.JSONEq(t, `{"permission":"repo_write"}`, string(raw))

	// recordDetails is independent of record(): calling one does not clobber
	// the other's field on the same entry.
	st.record("r1", deliveryRef{ChannelID: "C1", TS: "1.1"}, deliveryRef{})
	prompt, _, ok := st.get("r1")
	require.True(t, ok)
	assert.Equal(t, "C1", prompt.ChannelID)
	raw, ok = st.getDetails("r1")
	require.True(t, ok, "recordDetails' write survives a later record() call")
	assert.JSONEq(t, `{"permission":"repo_write"}`, string(raw))

	st.drop("r1")
	_, ok = st.getDetails("r1")
	assert.False(t, ok, "drop clears Details along with prompt/publicNote")
}

// --- handleInteractionDetailsAction: listener-side click handler ----------

// TestHandleInteractionDetailsAction_CacheHit verifies a click resolves
// Details from the process-wide interactionDelivery cache (populated by
// sendRequest) and renders the full modal — no memory round-trip needed.
func TestHandleInteractionDetailsAction_CacheHit(t *testing.T) {
	api, openedViews := recordingViewsOpenAPI(t)
	delivery := newInteractionDeliveryStore()
	delivery.recordDetails("req-hit", toolApprovalDetailsJSON(t, channelevents.ToolApprovalDetails{
		Permission: "repo_write", ResourceType: "repo", ResourceID: "org/x",
		ArgsJSON: `{"title":"hello"}`, ToolDescription: "creates a github issue",
	}))
	l := &slackListener{
		deps:                channelkinds.Deps{}, // no Memory wired: proves the cache hit never falls through
		api:                 api,
		interactionDelivery: delivery,
	}

	val := encodeApprovalButtonValue(discInteractionDetails, "req-hit", "", "ns/sess")
	cb := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		TriggerID: "trig-hit",
		User:      slackapi.User{ID: "U_ALICE"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{
			{ActionID: interactionDetailsActionID, Value: val},
		}},
	}
	handled, err := l.handleInteractionDetailsAction(context.Background(), cb)
	require.NoError(t, err)
	require.True(t, handled)

	views := openedViews()
	require.Len(t, views, 1, "modal must open")
	rendered := blocksJSON(t, views[0].Blocks.BlockSet)
	assert.Contains(t, rendered, "creates a github issue")
	assert.Contains(t, rendered, `{\"title\":\"hello\"}`, "args must render")
	assert.Contains(t, rendered, "org/x")
	assert.NotContains(t, rendered, "not available", "degraded body must not render")
}

// TestHandleInteractionDetailsAction_CacheMiss_FallsBackToMemory verifies a
// click whose request is absent from the in-process cache (channelsd
// restart, or another replica handled the request) renders the full modal
// from the durable memapproval record's generic Details field instead.
func TestHandleInteractionDetailsAction_CacheMiss_FallsBackToMemory(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	details := channelevents.ToolApprovalDetails{
		Permission: "repo_write", ResourceType: "repo", ResourceID: "org/x",
		ArgsJSON: `{"title":"hello"}`, ToolDescription: "creates a github issue",
	}
	require.NoError(t, memapproval.RecordRequest(context.Background(), mem,
		memory.Scope{Kind: "session", ID: "ns/sess"}, "tu-77",
		memapproval.Request{RequestID: "req-77", ToolName: "github_create_issue",
			Details: toolApprovalDetailsJSON(t, details)}))

	api, openedViews := recordingViewsOpenAPI(t)
	l := &slackListener{
		deps:                channelkinds.Deps{Memory: mem},
		api:                 api,
		interactionDelivery: newInteractionDeliveryStore(), // empty: forces the cache miss
	}

	val := encodeApprovalButtonValue(discInteractionDetails, "req-77", "", "ns/sess")
	cb := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		TriggerID: "trig-1",
		User:      slackapi.User{ID: "U_ALICE"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{
			{ActionID: interactionDetailsActionID, Value: val},
		}},
	}
	handled, err := l.handleInteractionDetailsAction(memory.WithSystemApproval(context.Background(), "test"), cb)
	require.NoError(t, err)
	require.True(t, handled)

	views := openedViews()
	require.Len(t, views, 1, "modal must open")
	rendered := blocksJSON(t, views[0].Blocks.BlockSet)
	assert.Contains(t, rendered, "creates a github issue", "full modal, not the degraded body")
	assert.Contains(t, rendered, `{\"title\":\"hello\"}`, "args must render")
	assert.NotContains(t, rendered, "not available", "degraded body must not render")
}

// TestHandleInteractionDetailsAction_CacheMissAndMemoryMiss_DegradedModal
// verifies the degraded body still renders (rather than dropping the click
// silently) when both the cache and the durable record miss.
func TestHandleInteractionDetailsAction_CacheMissAndMemoryMiss_DegradedModal(t *testing.T) {
	api, openedViews := recordingViewsOpenAPI(t)
	l := &slackListener{
		deps:                channelkinds.Deps{Memory: memory.NewLocal(inmem.NewBackend())},
		api:                 api,
		interactionDelivery: newInteractionDeliveryStore(),
	}
	val := encodeApprovalButtonValue(discInteractionDetails, "req-gone", "", "ns/sess")
	cb := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		TriggerID: "trig-2",
		User:      slackapi.User{ID: "U_ALICE"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{
			{ActionID: interactionDetailsActionID, Value: val},
		}},
	}
	handled, err := l.handleInteractionDetailsAction(context.Background(), cb)
	require.NoError(t, err)
	require.True(t, handled)
	views := openedViews()
	require.Len(t, views, 1)
	assert.Contains(t, blocksJSON(t, views[0].Blocks.BlockSet), "not available")
}

// TestHandleInteractionDetailsAction_NoBlockActions_FallsThrough verifies an
// InteractionCallback with no block actions at all is not recognized —
// caller falls through to the next handler.
func TestHandleInteractionDetailsAction_NoBlockActions_FallsThrough(t *testing.T) {
	l := &slackListener{}
	cb := slackapi.InteractionCallback{Type: slackapi.InteractionTypeBlockActions}
	handled, err := l.handleInteractionDetailsAction(context.Background(), cb)
	require.NoError(t, err)
	assert.False(t, handled, "no block actions means nothing to recognize")
}

// TestHandleInteractionDetailsAction_WrongDiscriminator_FallsThrough
// verifies a block_action whose value decodes but carries a different
// discriminator (e.g. the generic decision codec) is not claimed by this
// handler — it must fall through to handleInteractionDecisionClick.
func TestHandleInteractionDetailsAction_WrongDiscriminator_FallsThrough(t *testing.T) {
	l := &slackListener{interactionDelivery: newInteractionDeliveryStore()}
	val := encodeInteractionButtonValue("req-1", "approve", "identity_choice", "ns/sess")
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{
			{ActionID: interactionDetailsActionID, Value: val},
		}},
	}
	handled, err := l.handleInteractionDetailsAction(context.Background(), cb)
	require.NoError(t, err)
	assert.False(t, handled, "a discInteraction-valued click must not be claimed by the Details handler")
}
