// pkg/channels/channelkinds/slack/interaction_screenshot_test.go
//
// Task 5 (browser approval screenshots): after an interaction_request card
// posts, a payload carrying Screenshot must have the resolved PNG uploaded
// as a file threaded under that SAME card — see interaction.go's
// uploadScreenshot. These tests pin the three contract points: the happy
// path threads the upload under the card's own ts, a nil Screenshot is a
// complete no-op (no Fetch, no upload — the common case for every
// non-browser interaction), and a Fetch failure must not fail the card send
// (best-effort; the card already carries the full approval).
package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// screenshotRequestPayload returns a wire-valid AudienceRequester
// InteractionRequestPayload carrying the given Screenshot ref — the shape a
// browser-originated interaction (acting-approval, login_human_step,
// captcha) publishes. nil is legal (every non-browser category's zero
// value).
func screenshotRequestPayload(rawExternalID string, screenshot *channelevents.AttachmentRef) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		Category:   "login_human_step",
		RequestRef: "req-shot-1",
		Lead:       "Finish signing in",
		Screenshot: screenshot,
		Actions: []channelevents.InteractionAction{
			{ID: "done", Label: "Done", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStylePrimary},
		},
		Audience: channelevents.InteractionAudience{
			Scope: channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{
				Kind:       "slack",
				ExternalID: identity.RawExternalID(rawExternalID),
			},
		},
	}
}

// TestInteractionSender_Request_Screenshot_UploadsThreadedUnderCard verifies
// the happy path end to end: the card posts first (via PostEphemeralContext,
// since the session carries channel context), and the Screenshot is then
// resolved via AssetFetcher.Fetch(ns, sess, RenderName) and uploaded via
// UploadFileContext with ThreadTimestamp equal to the card's own ts — the
// card establishes the thread root; the upload never can (see sender.go's
// sendWithAttachments note on files.uploadV2 returning no ts).
func TestInteractionSender_Request_Screenshot_UploadsThreadedUnderCard(t *testing.T) {
	const (
		slackID  = "U_SHOT"
		chanID   = "C01CHAN"
		threadTS = "1700000000.000001"
	)
	pngBytes := []byte("\x89PNG-fake-bytes")
	af := &stubAssetFetcher{bytes: pngBytes, mime: "image/png", filename: "page.png"}
	c := &fakeSlackClient{}
	s := &interactionSender{client: c, deps: channelkinds.Deps{AssetFetcher: af}}

	screenshot := &channelevents.AttachmentRef{
		RenderName: "shot-1",
		MIME:       "image/png",
		Filename:   "page.png",
		AltText:    "redacted page screenshot",
	}
	pl := screenshotRequestPayload(slackID, screenshot)
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	// The card itself landed via PostEphemeral (channel context present).
	require.Len(t, c.postEphemeralCalls, 1, "expected 1 card post")
	cardTS := "1614191050.013301" // fakeSlackClient's fixed PostEphemeralContext ts

	require.Len(t, af.calls, 1, "expected exactly one asset fetch")
	assert.Equal(t, stubFetchCall{ns: "default", sess: "sess-1", render: "shot-1"}, af.calls[0],
		"AssetFetcher.Fetch must be called with the ref's RenderName")

	require.Len(t, c.uploadFileCalls, 1, "expected exactly one file upload")
	up := c.uploadFileCalls[0]
	assert.Equal(t, chanID, up.channelID, "upload must target the card's channel")
	assert.Equal(t, cardTS, up.threadTS, "upload must be threaded under the card's own ts")
	assert.Equal(t, pngBytes, up.contents, "uploaded bytes must be the fetched PNG bytes")
	assert.Equal(t, "page.png", up.filename)
	assert.Equal(t, "redacted page screenshot", up.altTxt)
}

// TestInteractionSender_Request_NilScreenshot_NoUploadAttempt verifies the
// common case (every non-browser interaction): a nil Screenshot means no
// Fetch call and no UploadFileContext call, while the card itself still
// sends normally.
func TestInteractionSender_Request_NilScreenshot_NoUploadAttempt(t *testing.T) {
	const (
		slackID  = "U_NOSHOT"
		chanID   = "C01CHAN"
		threadTS = "1700000000.000002"
	)
	af := &stubAssetFetcher{bytes: []byte("should never be fetched"), mime: "image/png", filename: "page.png"}
	c := &fakeSlackClient{}
	s := &interactionSender{client: c, deps: channelkinds.Deps{AssetFetcher: af}}

	pl := screenshotRequestPayload(slackID, nil)
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "Send")

	require.Len(t, c.postEphemeralCalls, 1, "the card must still send")
	assert.Empty(t, af.calls, "no Screenshot ⇒ AssetFetcher.Fetch must never be called")
	assert.Empty(t, c.uploadFileCalls, "no Screenshot ⇒ UploadFileContext must never be called")
}

// TestInteractionSender_Request_ScreenshotFetchError_CardStillSucceeds
// verifies the best-effort contract: an AssetFetcher.Fetch error must not
// fail sendRequest (the card already carries the full approval text) and
// must not attempt an upload with no bytes.
func TestInteractionSender_Request_ScreenshotFetchError_CardStillSucceeds(t *testing.T) {
	const (
		slackID  = "U_FETCHFAIL"
		chanID   = "C01CHAN"
		threadTS = "1700000000.000003"
	)
	af := &stubAssetFetcher{err: errors.New("operator says no")}
	c := &fakeSlackClient{}
	s := &interactionSender{client: c, deps: channelkinds.Deps{AssetFetcher: af}}

	screenshot := &channelevents.AttachmentRef{RenderName: "shot-err", MIME: "image/png", Filename: "page.png"}
	pl := screenshotRequestPayload(slackID, screenshot)
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "a screenshot fetch failure must not fail the card send")

	require.Len(t, c.postEphemeralCalls, 1, "the card must still have landed")
	require.Len(t, af.calls, 1, "the fetch must still have been attempted")
	assert.Empty(t, c.uploadFileCalls, "no bytes to upload after a fetch failure")
}

// TestInteractionSender_Request_ScreenshotNoAssetFetcher_SkipsUpload verifies
// the AssetFetcher-nil guard: a surface that never configured an
// AssetFetcher (some surfaces don't) must skip the upload entirely —
// no panic, no error, the card still sends.
func TestInteractionSender_Request_ScreenshotNoAssetFetcher_SkipsUpload(t *testing.T) {
	const (
		slackID  = "U_NOFETCHER"
		chanID   = "C01CHAN"
		threadTS = "1700000000.000004"
	)
	c := &fakeSlackClient{}
	s := &interactionSender{client: c} // deps.AssetFetcher left nil

	screenshot := &channelevents.AttachmentRef{RenderName: "shot-nofetcher", MIME: "image/png", Filename: "page.png"}
	pl := screenshotRequestPayload(slackID, screenshot)
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, pl)
	sess := sessionWithChannel(chanID, threadTS)

	_, err := s.Send(context.Background(), sess, env)
	require.NoError(t, err, "a nil AssetFetcher must not fail the card send")

	require.Len(t, c.postEphemeralCalls, 1, "the card must still have landed")
	assert.Empty(t, c.uploadFileCalls, "no AssetFetcher ⇒ no upload attempted")
}
