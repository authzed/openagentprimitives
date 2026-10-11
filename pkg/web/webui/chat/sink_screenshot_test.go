package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
)

// TestToFrame_InteractionRequest_CarriesScreenshotRef pins the claim Task 6
// of the browser-approval-screenshots feature depends on: toFrame's
// MsgInteractionRequest branch maps the WHOLE channelevents.
// InteractionRequestPayload (Payload: m) onto the wire frame, so
// InteractionRequestPayload.Screenshot — Task 1's redacted-screenshot
// AttachmentRef — rides along with no change to toFrame itself. This test
// exists to catch a future refactor that narrows the frame's payload to a
// hand-picked field set (the same mistake would silently drop Screenshot
// again) rather than to prove any code was written here.
func TestToFrame_InteractionRequest_CarriesScreenshotRef(t *testing.T) {
	ref := browser.SessionRef{Namespace: "default", Name: "s1"}
	shot := &channelevents.AttachmentRef{
		RenderName: "shot-1",
		ArtifactID: "art-1",
		MIME:       "image/png",
		Filename:   "page.png",
		AltText:    "the page",
	}
	msg := browser.MsgInteractionRequest{
		Session: ref,
		Payload: channelevents.InteractionRequestPayload{
			Category:   "tool_approval",
			RequestRef: "req-1",
			Lead:       "Approve this tool call?",
			Screenshot: shot,
		},
	}

	frame, ok := toFrame(msg)
	require.True(t, ok, "toFrame must map browser.MsgInteractionRequest")
	assert.Equal(t, "interaction_request", frame.Type)

	// Round-trip through JSON the same way the wire frame actually travels to
	// the browser, rather than reaching into frame.Payload's concrete type —
	// that is the only way to catch a regression in the json tag itself.
	raw, err := json.Marshal(frame)
	require.NoError(t, err)
	var decoded struct {
		Payload struct {
			Payload struct {
				Screenshot *channelevents.AttachmentRef `json:"screenshot"`
			} `json:"payload"`
		} `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotNil(t, decoded.Payload.Payload.Screenshot, "screenshot ref must survive the frame round-trip")
	assert.Equal(t, "shot-1", decoded.Payload.Payload.Screenshot.RenderName)
	assert.Equal(t, "art-1", decoded.Payload.Payload.Screenshot.ArtifactID)
	assert.Equal(t, "page.png", decoded.Payload.Payload.Screenshot.Filename)
	assert.Equal(t, "the page", decoded.Payload.Payload.Screenshot.AltText)
}

// TestToFrame_InteractionRequest_NilScreenshotOmitted covers the inverse: a
// non-browser (or best-effort-failed) interaction request carries no
// screenshot, and omitempty keeps it absent from the wire frame entirely
// rather than serializing an explicit null the TS side would have to special
// case.
func TestToFrame_InteractionRequest_NilScreenshotOmitted(t *testing.T) {
	ref := browser.SessionRef{Namespace: "default", Name: "s1"}
	msg := browser.MsgInteractionRequest{
		Session: ref,
		Payload: channelevents.InteractionRequestPayload{
			Category:   "credential_link",
			RequestRef: "req-2",
			Lead:       "Connect your account",
		},
	}

	frame, ok := toFrame(msg)
	require.True(t, ok)

	raw, err := json.Marshal(frame)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"screenshot"`, "a nil Screenshot must be omitted, not sent as null")
}
