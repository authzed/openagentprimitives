package channelinteractions

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestRenderText_Screenshot covers the four cases the browser-approval
// screenshot line must handle on the plain-text floor: a wired callback that
// resolves a URL, a wired callback that returns "" (e.g. minting failed or
// isn't configured), no Screenshot on the payload at all, and a nil callback.
// In every case but the first, no "Page screenshot:" line may appear — this
// is a best-effort addition to the card, never something that can block or
// alter the rest of it.
func TestRenderText_Screenshot(t *testing.T) {
	withScreenshot := renderableRequest()
	withScreenshot.Screenshot = &channelevents.AttachmentRef{
		RenderName: "shot-abc",
		ArtifactID: "artifact-xyz",
		MIME:       channelevents.ScreenshotMIME,
		Filename:   "page.png",
		AltText:    "page screenshot",
	}
	withoutScreenshot := renderableRequest()

	cases := []struct {
		name          string
		payload       channelevents.InteractionRequestPayload
		screenshotURL func(*channelevents.AttachmentRef) string
		wantLine      bool
		wantURL       string
	}{
		{
			name:    "screenshot present, callback resolves a URL: line appears with the URL",
			payload: withScreenshot,
			screenshotURL: func(ref *channelevents.AttachmentRef) string {
				return "https://example/view?d=x"
			},
			wantLine: true,
			wantURL:  "https://example/view?d=x",
		},
		{
			name:    "screenshot present, callback returns empty string: no line",
			payload: withScreenshot,
			screenshotURL: func(ref *channelevents.AttachmentRef) string {
				return ""
			},
			wantLine: false,
		},
		{
			name:    "no screenshot on the payload: callback is never consulted, no line",
			payload: withoutScreenshot,
			screenshotURL: func(ref *channelevents.AttachmentRef) string {
				return "should-not-appear"
			},
			wantLine: false,
		},
		{
			name:          "screenshot present, nil callback: no panic, no line",
			payload:       withScreenshot,
			screenshotURL: nil,
			wantLine:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RenderText(tc.payload, nil, tc.screenshotURL)
			if tc.wantLine {
				assert.Contains(t, out, "Page screenshot:")
				assert.Contains(t, out, tc.wantURL)
			} else {
				assert.NotContains(t, out, "Page screenshot:")
				assert.NotContains(t, out, "should-not-appear")
			}
		})
	}
}
