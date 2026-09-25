package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func testBlocks() []slackapi.Block {
	return []slackapi.Block{slackapi.NewSectionBlock(slackapi.NewTextBlockObject(slackapi.MarkdownType, "hi", false, false), nil, nil)}
}

func TestDeliverPromptDMOnlyOpensDM(t *testing.T) {
	fc := &fakeSlackClient{} // records OpenConversation + PostMessage calls
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
	sess := channelkinds.SessionInfo{Namespace: "default", Name: "s1"} // no Channel → DM only anyway
	ref, err := s.deliverPrompt(context.Background(), sess, "UOWNER",
		channelinteractions.SurfaceDMOnly, testBlocks(), "hi")
	require.NoError(t, err)
	assert.True(t, fc.openedDM, "SurfaceDMOnly must open a DM")
	assert.False(t, fc.postedEphemeral, "SurfaceDMOnly must not post ephemeral")
	assert.NotEmpty(t, ref.TS, "delivery ref carries the posted ts")
}

// TestDeliverPromptDMOnlyOpensDMEvenWithChannel proves the SurfaceDMOnly
// override actually routes around an available channel — the previous test
// used a session with no Channel at all, which would also open a DM under
// the default surface and so never exercised the override.
func TestDeliverPromptDMOnlyOpensDMEvenWithChannel(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "s1",
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
	}
	ref, err := s.deliverPrompt(context.Background(), sess, "UOWNER",
		channelinteractions.SurfaceDMOnly, testBlocks(), "hi")
	require.NoError(t, err)
	assert.True(t, fc.openedDM, "SurfaceDMOnly must open a DM even though a channel is present")
	assert.False(t, fc.postedEphemeral, "SurfaceDMOnly must not post ephemeral")
	assert.NotEqual(t, "C1", ref.ChannelID, "delivery landed in the DM, not the session channel")
	assert.Len(t, fc.setTitleCalls, 1, "DM thread is titled so it surfaces in the mobile Messages tab")
}

// TestDeliverPromptDefaultSurfaceChannelPresentPostsEphemeral proves the
// default surface (SurfaceEphemeralDMFallback) posts ephemeral in the
// session channel and opens no DM when that post succeeds.
func TestDeliverPromptDefaultSurfaceChannelPresentPostsEphemeral(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "s1",
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
	}
	ref, err := s.deliverPrompt(context.Background(), sess, "UOWNER",
		channelinteractions.SurfaceEphemeralDMFallback, testBlocks(), "hi")
	require.NoError(t, err)
	assert.True(t, fc.postedEphemeral, "default surface posts ephemeral when a channel is present")
	assert.False(t, fc.openedDM, "no DM fallback needed when the ephemeral post succeeds")
	assert.Equal(t, "C1", ref.ChannelID, "delivery ref carries the session channel")
	assert.NotEmpty(t, ref.TS, "delivery ref carries the posted ts")
}

// TestDeliverPromptDefaultSurfaceEphemeralUserNotInChannelFallsBackToDM
// proves the user_not_in_channel rejection triggers the DM fallback.
func TestDeliverPromptDefaultSurfaceEphemeralUserNotInChannelFallsBackToDM(t *testing.T) {
	fc := &fakeSlackClient{postEphemeralErrs: []error{slackapi.SlackErrorResponse{Err: "user_not_in_channel"}}}
	s := &interactionSender{client: fc, delivery: newInteractionDeliveryStore()}
	sess := channelkinds.SessionInfo{
		Namespace: "default", Name: "s1",
		Channel: &spiceboxv1alpha1.ChannelBinding{External: map[string]string{"channel_id": "C1"}},
	}
	ref, err := s.deliverPrompt(context.Background(), sess, "UOWNER",
		channelinteractions.SurfaceEphemeralDMFallback, testBlocks(), "hi")
	require.NoError(t, err)
	assert.True(t, fc.postedEphemeral, "ephemeral post was attempted first")
	assert.True(t, fc.openedDM, "user_not_in_channel triggers the DM fallback")
	assert.NotEqual(t, "C1", ref.ChannelID, "delivery landed in the DM, not the session channel")
}

func TestDeliveryStoreRoundTrips(t *testing.T) {
	st := newInteractionDeliveryStore()
	st.record("r1", deliveryRef{ChannelID: "C1", TS: "1.1"}, deliveryRef{ChannelID: "C2", TS: "2.2"})
	prompt, note, ok := st.get("r1")
	require.True(t, ok)
	assert.Equal(t, "C1", prompt.ChannelID)
	assert.Equal(t, "C2", note.ChannelID)
	_, _, ok = st.get("missing")
	assert.False(t, ok, "unknown requestRef is a graceful miss")
}

func TestDeliveryStoreDropRemovesEntry(t *testing.T) {
	st := newInteractionDeliveryStore()
	st.record("r1", deliveryRef{ChannelID: "C1", TS: "1.1"}, deliveryRef{})
	_, _, ok := st.get("r1")
	require.True(t, ok, "precondition: entry recorded")

	st.drop("r1")

	_, _, ok = st.get("r1")
	assert.False(t, ok, "drop removes the entry so the store does not grow unbounded")
}
