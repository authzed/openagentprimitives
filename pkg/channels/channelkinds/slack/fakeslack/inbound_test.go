package fakeslack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInjectDM_RecordsTopLevelAndBuildsWellFormedEvent proves InjectDM's two
// jobs: (1) the user's message lands as a TOP-LEVEL entry in the simulated
// conversation tree (so a subsequent agent reply threads under it coherently)
// and (2) the returned socketmode.Event decodes to exactly the shape
// pkg/channels/channelkinds/slack/listener.go's `handle` + `handleEventsAPI` require —
// a mismatch here would mean the listener silently drops the event.
func TestInjectDM_RecordsTopLevelAndBuildsWellFormedEvent(t *testing.T) {
	c := New()

	ts, ev := c.InjectDM("U1", "D01", "hello there")
	require.NotEmpty(t, ts)

	// (1) recorded as a top-level message.
	top := c.TopLevel("D01")
	require.Len(t, top, 1)
	assert.Equal(t, ts, top[0].TS)
	assert.Equal(t, "hello there", top[0].Text)
	assert.Equal(t, "U1", top[0].UserID)
	assert.Empty(t, top[0].ThreadTS, "InjectDM's message must be top-level, not threaded")

	// (2) event shape: handle() asserts evt.Type == EventTypeEventsAPI and
	// evt.Data is a VALUE slackevents.EventsAPIEvent (not a pointer).
	require.Equal(t, socketmode.EventTypeEventsAPI, ev.Type)
	require.NotNil(t, ev.Request, "handle()'s Ack-before-processing branch requires a non-nil *Request")
	api, ok := ev.Data.(slackevents.EventsAPIEvent)
	require.True(t, ok, "evt.Data must be a slackevents.EventsAPIEvent value, not a pointer or other type")

	// handleEventsAPI gates on api.Type == CallbackEvent before dispatching.
	require.Equal(t, slackevents.CallbackEvent, api.Type)
	assert.Equal(t, "message", api.InnerEvent.Type)

	// handleEventsAPI's switch type-asserts *slackevents.MessageEvent for the
	// "message" inner event.
	msg, ok := api.InnerEvent.Data.(*slackevents.MessageEvent)
	require.True(t, ok, "api.InnerEvent.Data must be *slackevents.MessageEvent")
	assert.Equal(t, "U1", msg.User)
	assert.Equal(t, "D01", msg.Channel)
	assert.Equal(t, "hello there", msg.Text)
	assert.Equal(t, ts, msg.TimeStamp)
	assert.Equal(t, "im", msg.ChannelType, "must route to the message.im/handleDM branch")
	assert.Empty(t, msg.SubType, "handleEventsAPI rejects non-empty SubType (edits, joins, etc.)")
}

// TestInjectMention_RecordsTopLevelAndBuildsWellFormedEvent mirrors the DM
// proof for the app_mention path.
func TestInjectMention_RecordsTopLevelAndBuildsWellFormedEvent(t *testing.T) {
	c := New()

	ts, ev := c.InjectMention("U2", "C01", "<@UBOT> help")
	require.NotEmpty(t, ts)

	top := c.TopLevel("C01")
	require.Len(t, top, 1)
	assert.Equal(t, ts, top[0].TS)

	require.Equal(t, socketmode.EventTypeEventsAPI, ev.Type)
	require.NotNil(t, ev.Request)
	api, ok := ev.Data.(slackevents.EventsAPIEvent)
	require.True(t, ok)
	require.Equal(t, slackevents.CallbackEvent, api.Type)
	assert.Equal(t, "app_mention", api.InnerEvent.Type)

	mention, ok := api.InnerEvent.Data.(*slackevents.AppMentionEvent)
	require.True(t, ok, "api.InnerEvent.Data must be *slackevents.AppMentionEvent")
	assert.Equal(t, "U2", mention.User)
	assert.Equal(t, "C01", mention.Channel)
	assert.Equal(t, "<@UBOT> help", mention.Text)
	assert.Equal(t, ts, mention.TimeStamp)
	assert.Empty(t, mention.BotID, "handleEventsAPI drops app_mention with a non-empty BotID")
}

func TestAuthTestContext_ReturnsBotIdentityAndGrantedScopes(t *testing.T) {
	c := New()
	resp, err := c.AuthTestContext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, BotUserID, resp.UserID)
	require.NotNil(t, resp.Header, "Header must be set so listener.Start's missingScopes() sees granted scopes")
	assert.NotEmpty(t, resp.Header.Get("X-OAuth-Scopes"))
}

func TestOpenView_And_PublishViewContext_ReturnValidResponses(t *testing.T) {
	c := New()
	v, err := c.OpenView("trigger", slackapi.ModalViewRequest{})
	require.NoError(t, err)
	assert.NotNil(t, v)

	pv, err := c.PublishViewContext(context.Background(), slackapi.PublishViewContextRequest{})
	require.NoError(t, err)
	assert.NotNil(t, pv)
}
