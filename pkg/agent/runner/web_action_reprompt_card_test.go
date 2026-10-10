package runner

// web_action_reprompt_card_test.go is the TDD suite for Phase E's E10 task:
// when a browser mutate's web_action deny is a CHANGED-PAGE RE-PROMPT (the
// ToolCallAuthz hook stamped ask.WebActionChangedPageReprompt from the
// checker's authz.Result.ChangedPageReprompt), buildToolCallPending must:
//
//   - prepend a "the page changed since you approved this" notice to the
//     card's fields — and must NOT for an ordinary first-time web_action ask
//     (WebActionKey set, but ChangedPageReprompt false);
//   - attach a FRESH screenshot (via h.captureScreenshot, called at
//     publish/ask-raise time — i.e. against the CURRENT page) for any
//     web_action ask, re-prompt or not, mirroring the same browser-only gate
//     buildPlanGatePending / buildLoginHumanStepPending already use
//     (browser_screenshot_wiring_test.go);
//   - never attempt a capture for an ordinary (non-browser-mutate) tool_call
//     ask, even if a capturer happens to be wired.
//
// Task #47 widened the LAST bullet: a coarse web_host#act acting ask
// (ResourceType == browserschema.WebHostType, WebActionKey == "" — the JIT
// per-call ask check_tool_call.go's LAYER 1 raises when no plan-gate grant
// already covers the call) now ALSO captures, not just the web_action mutate
// recheck. The gate stays keyed on the ASK SHAPE (resourceType/WebActionKey),
// not on captureScreenshot's mere presence, so the "ordinary tool_call never
// captures even if wired" invariant above is unchanged.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// webActionToolCallAsk builds a tool_call ApprovalAsk for a browser mutate —
// the same shape toolCallAsk() returns, with the web_action dedicated fields
// set as the ToolCallAuthz hook would (ask.WebActionKey/WebActionFingerprint
// always both set for a web_action ask; WebActionChangedPageReprompt only
// when the denial was a re-prompt).
func webActionToolCallAsk(changedPage bool) pipeline.ApprovalAsk {
	ask := toolCallAsk()
	ask.WebActionKey = "aa.com/click/place-order-abcd1234"
	ask.WebActionFingerprint = "fp-live"
	ask.WebActionChangedPageReprompt = changedPage
	return ask
}

// webActionApprovalHost builds a runnerHost wired exactly like
// TestBuildToolCallPending_BuildsInteractionRequest's, capturing the
// published envelope into published.
func webActionApprovalHost(published *channelevents.Envelope) *runnerHost {
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          approval.New(),
		ChannelKind:       "slack",
		Mem:               memory.NewLocal(inmem.NewBackend()),
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			*published = env
			return nil
		},
	}
	return newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
}

// A changed-page RE-PROMPT's card must carry the changed-page notice as its
// FIRST field.
func TestBuildToolCallPending_ChangedPageReprompt_AddsNotice(t *testing.T) {
	var published channelevents.Envelope
	h := webActionApprovalHost(&published)

	reqID, err := h.PublishApproval(context.Background(), webActionToolCallAsk(true))
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	pl := decodeInteractionRequest(t, published)
	require.NotEmpty(t, pl.Fields)
	assert.Equal(t, "Note", pl.Fields[0].Label, "the changed-page notice must lead the card")
	assert.Contains(t, pl.Fields[0].Value, "page changed since you last approved")
}

// An ordinary FIRST-TIME web_action ask (WebActionKey set, but NOT a
// re-prompt) must NOT carry the notice — the two must read differently.
func TestBuildToolCallPending_FirstTimeWebActionAsk_NoNotice(t *testing.T) {
	var published channelevents.Envelope
	h := webActionApprovalHost(&published)

	reqID, err := h.PublishApproval(context.Background(), webActionToolCallAsk(false))
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	pl := decodeInteractionRequest(t, published)
	for _, f := range pl.Fields {
		assert.NotEqual(t, "Note", f.Label, "a first-time ask must not carry the changed-page notice")
	}
}

// An ordinary (non-browser) tool_call ask must never carry the notice either,
// as a baseline: TestBuildToolCallPending_BuildsInteractionRequest already
// covers its shape; this just confirms the new field is additive.
func TestBuildToolCallPending_OrdinaryToolCall_NoNotice(t *testing.T) {
	var published channelevents.Envelope
	h := webActionApprovalHost(&published)

	reqID, err := h.PublishApproval(context.Background(), toolCallAsk())
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	pl := decodeInteractionRequest(t, published)
	for _, f := range pl.Fields {
		assert.NotEqual(t, "Note", f.Label)
	}
}

// A web_action ask (re-prompt or not) must attach a FRESH screenshot — i.e.
// one captured at publish time, not a screenshot handed in some other way —
// when the host carries a working capturer.
func TestBuildToolCallPending_WebActionAsk_AttachesFreshScreenshot(t *testing.T) {
	var published channelevents.Envelope
	h := webActionApprovalHost(&published)
	ref := &channelevents.AttachmentRef{RenderName: "shot-reprompt", ArtifactID: "art-1", MIME: "image/png", Filename: "page.png", AltText: "current page"}
	var calls int
	h.captureScreenshot = func(_ context.Context) *channelevents.AttachmentRef {
		calls++
		return ref
	}

	reqID, err := h.PublishApproval(context.Background(), webActionToolCallAsk(true))
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	assert.Equal(t, 1, calls, "the capture must run exactly once, at publish time — i.e. against the CURRENT page")
	pl := decodeInteractionRequest(t, published)
	require.NotNil(t, pl.Screenshot, "a working capturer must attach a screenshot to the re-prompt card")
	assert.Equal(t, "shot-reprompt", pl.Screenshot.RenderName)
}

// A PLAIN (non-browser) tool_call ask must never invoke the capturer, even if
// one happens to be wired on the host — the browser-only gate is keyed on
// ask.WebActionKey, not on captureScreenshot's mere presence.
func TestBuildToolCallPending_OrdinaryToolCall_NeverCapturesScreenshot(t *testing.T) {
	var published channelevents.Envelope
	h := webActionApprovalHost(&published)
	var calls int
	h.captureScreenshot = func(_ context.Context) *channelevents.AttachmentRef {
		calls++
		return &channelevents.AttachmentRef{RenderName: "shot-x"}
	}

	reqID, err := h.PublishApproval(context.Background(), toolCallAsk())
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	assert.Equal(t, 0, calls, "a non-browser-mutate tool_call ask must never attempt a screenshot capture")
	pl := decodeInteractionRequest(t, published)
	assert.Nil(t, pl.Screenshot)
}

// webHostAskResourceStandings mirrors testResourceStandings but also
// declares web_host (session-only, matching the browser toolbox's real
// standing — see pkg/authz/plangate/card_slot_standing_test.go) so a coarse
// web_host#act acting ask can resolve approvers in these tests.
func webHostAskResourceStandings() map[string]ResourceStanding {
	standings := testResourceStandings()
	standings[browserschema.WebHostType] = ResourceStanding{Standing: spiceboxv1alpha1.StandingSessionOnly}
	return standings
}

// webHostToolCallAsk builds a tool_call ApprovalAsk for the COARSE
// web_host#act acting approval — the JIT per-call ask check_tool_call.go's
// LAYER 1 raises when no plan-gate grant already covers the call.
// WebActionKey/Fingerprint stay empty: only the LAYER 2 mutate recheck stamps
// those (see webActionToolCallAsk above).
func webHostToolCallAsk() pipeline.ApprovalAsk {
	payload := ToolCallApprovalPayload{
		SessNS:       "ns",
		SessName:     "s",
		ToolName:     "browser_click",
		Permission:   "act",
		ResourceType: browserschema.WebHostType,
		ResourceID:   "example.com",
		StateImpact:  "external",
		ArgsHash:     "h1",
		Perm:         authz.Permission{StateImpact: authz.External},
		UseID:        "tu-1",
	}
	return pipeline.ApprovalAsk{
		Kind:    "tool_call",
		Summary: "act on example.com",
		Payload: payload.ToMap(),
	}
}

// webHostApprovalHost mirrors webActionApprovalHost, but with web_host
// declared on ResourceStandings so a coarse web_host#act ask can route.
func webHostApprovalHost(published *channelevents.Envelope) *runnerHost {
	l := &Loop{
		ResourceStandings: webHostAskResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          approval.New(),
		ChannelKind:       "slack",
		Mem:               memory.NewLocal(inmem.NewBackend()),
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			*published = env
			return nil
		},
	}
	return newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
}

// TestBuildToolCallPending_WebHostActingAsk_AttachesScreenshot is Task #47's
// Part B: a coarse web_host#act acting ask (WebActionKey == "") must now
// attach a screenshot too, not just the web_action mutate recheck — the
// product decision behind widening the gate at host_approval.go:~759.
func TestBuildToolCallPending_WebHostActingAsk_AttachesScreenshot(t *testing.T) {
	var published channelevents.Envelope
	h := webHostApprovalHost(&published)
	ref := &channelevents.AttachmentRef{RenderName: "shot-webhost", ArtifactID: "art-wh", MIME: "image/png", Filename: "page.png", AltText: "current page"}
	var calls int
	h.captureScreenshot = func(_ context.Context) *channelevents.AttachmentRef {
		calls++
		return ref
	}

	reqID, err := h.PublishApproval(context.Background(), webHostToolCallAsk())
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	assert.Equal(t, 1, calls, "a coarse web_host acting ask must capture the live page, not only a web_action mutate")
	pl := decodeInteractionRequest(t, published)
	require.NotNil(t, pl.Screenshot, "a working capturer must attach a screenshot to the web_host acting approval")
	assert.Equal(t, "shot-webhost", pl.Screenshot.RenderName)
}

// TestBuildToolCallPending_WebHostActingAsk_NoScreenshot_WhenCaptureScreenshotUnset
// proves the browser-only gate still holds for the widened case: a web_host
// ask whose host carries no capturer (the production shape for a non-browser
// call, and simply "not wired yet" here) attempts no capture and ships
// text-only.
func TestBuildToolCallPending_WebHostActingAsk_NoScreenshot_WhenCaptureScreenshotUnset(t *testing.T) {
	var published channelevents.Envelope
	h := webHostApprovalHost(&published) // h.captureScreenshot left nil

	reqID, err := h.PublishApproval(context.Background(), webHostToolCallAsk())
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	pl := decodeInteractionRequest(t, published)
	assert.Nil(t, pl.Screenshot)
}

// A capture failure (nil return) must never block the re-prompt from
// publishing — best-effort, same contract as every other screenshot site.
func TestBuildToolCallPending_ChangedPageReprompt_CaptureNilNeverBlocksPublish(t *testing.T) {
	var published channelevents.Envelope
	h := webActionApprovalHost(&published)
	h.captureScreenshot = func(_ context.Context) *channelevents.AttachmentRef { return nil }

	reqID, err := h.PublishApproval(context.Background(), webActionToolCallAsk(true))
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()), "a nil screenshot must never block the re-prompt's publish")

	pl := decodeInteractionRequest(t, published)
	assert.Nil(t, pl.Screenshot)
	// The notice is unaffected by the screenshot outcome — the two are
	// independent best-effort additions to the same card.
	require.NotEmpty(t, pl.Fields)
	assert.Equal(t, "Note", pl.Fields[0].Label)
}

// The Layer-2 web_action marker must travel onto the published
// ToolApprovalDetails so the channelsd handler can SKIP the coarse web_host
// grant write. Without it, OPERATION_TOUCH would shorten an existing
// session-length web_host grant to the ask's flat 30s expiry, collapsing the
// standing approval and re-prompting every later acting call on that host.
// An ordinary tool_call ask must carry no marker. This is the runner half of
// the fix; its channelsd counterpart is
// TestToolApprovalHandler_WebActionAsk_DoesNotTouchWebHostGrant.
func TestBuildToolCallPending_WebActionAsk_CarriesWebActionKeyInDetails(t *testing.T) {
	cases := []struct {
		name    string
		ask     pipeline.ApprovalAsk
		wantKey string
	}{
		{
			name:    "web_action ask carries its key so channelsd skips the web_host clobber",
			ask:     webActionToolCallAsk(false),
			wantKey: "aa.com/click/place-order-abcd1234",
		},
		{
			name:    "ordinary tool_call ask carries no web_action key",
			ask:     toolCallAsk(),
			wantKey: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var published channelevents.Envelope
			h := webActionApprovalHost(&published)

			reqID, err := h.PublishApproval(context.Background(), tc.ask)
			require.NoError(t, err)
			pa, ok := h.pending().m[reqID]
			require.True(t, ok)
			require.NoError(t, pa.onPublish(context.Background()))

			pl := decodeInteractionRequest(t, published)
			var det channelevents.ToolApprovalDetails
			require.NoError(t, json.Unmarshal(pl.Details, &det))
			assert.Equal(t, tc.wantKey, det.WebActionKey)
		})
	}
}
