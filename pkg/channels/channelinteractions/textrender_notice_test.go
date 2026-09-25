package channelinteractions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func noticePayload(category string) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "agents", Name: "demo-session"},
		Category:        category,
		RequestRef:      "req-1",
		Lead:            "Agent stopped",
		Body:            "It ran out of memory.",
		NextStep:        "Start a new thread to try again.",
		Audience:        channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	}
}

func registerNoticeFixture(t *testing.T, name string, sev Tone) {
	t.Helper()
	Reset()
	t.Cleanup(Reset)
	Register(Category{Name: name, Notice: true, Tone: sev, Resurface: ResurfaceNone})
}

func TestRenderText_NoticeCarriesToneMarkerAndNextStep(t *testing.T) {
	cases := []struct {
		name       string
		tone       Tone
		wantMarker string
	}{
		{name: "critical: marked [!]", tone: ToneCritical, wantMarker: "[!]"},
		{name: "privacy: marked [privacy]", tone: TonePrivacy, wantMarker: "[privacy]"},
		{name: "degraded: marked [degraded]", tone: ToneDegraded, wantMarker: "[degraded]"},
		{name: "waiting: marked [waiting]", tone: ToneWaiting, wantMarker: "[waiting]"},
		{name: "resolved: marked [ok]", tone: ToneResolved, wantMarker: "[ok]"},
		{name: "routine: deliberately unmarked", tone: ToneRoutine, wantMarker: ""},
		{name: "housekeeping: deliberately unmarked", tone: ToneHousekeeping, wantMarker: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registerNoticeFixture(t, "demo_notice", tc.tone)
			out := RenderText(noticePayload("demo_notice"), nil)

			assert.Contains(t, out, "**Agent stopped**")
			assert.Contains(t, out, "**Start a new thread to try again.**",
				"NextStep must be emphasised on every surface")
			if tc.wantMarker == "" {
				assert.NotContains(t, out, "[!]")
				assert.NotContains(t, out, "[degraded]")
				assert.NotContains(t, out, "[privacy]")
				assert.NotContains(t, out, "[waiting]")
				assert.NotContains(t, out, "[ok]")
				return
			}
			require.Contains(t, out, tc.wantMarker)
			assert.Less(t, indexOf(out, tc.wantMarker), indexOf(out, "**Agent stopped**"),
				"the marker leads the notice")
		})
	}
}

// A notice has no actions, so the floor must not emit the "respond from a
// connected app" trailer that an unlinkable PROMPT gets — there is nothing to
// respond to.
func TestRenderText_NoticeHasNoRespondTrailer(t *testing.T) {
	registerNoticeFixture(t, "demo_notice", ToneRoutine)
	out := RenderText(noticePayload("demo_notice"), nil)
	assert.NotContains(t, out, "respond from a connected app")
}

// A prompt category resolves no tone marker: the floor leaves a prompt
// unmarked, because its actions already say what it is.
func TestRenderText_PromptRenderingUnchanged(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Register(Category{Name: "tool_approval", Deciders: DecideOwner, Tone: ToneRoutine, Resurface: ResurfaceNone})

	out := RenderText(renderableRequest(), nil)
	assert.True(t, len(out) > 0)
	assert.NotContains(t, out, "[!]")
	assert.NotContains(t, out, "[degraded]")
	assert.Contains(t, out, "**Approval needed**")
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
