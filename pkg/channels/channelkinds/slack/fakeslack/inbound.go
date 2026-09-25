package fakeslack

import (
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// InjectDM records a user DM as a top-level message in the simulated
// conversation tree (so a reply's thread anchor is coherent — see
// fakeslack.TopLevel/Replies) and returns its ts plus the socketmode.Event
// the real listener's dispatch loop will process. Feed the returned event to
// a SocketSource.Push.
//
// The event shape mirrors exactly what pkg/channels/channelkinds/slack/listener.go's
// `handle` + `handleEventsAPI` type-assert:
//   - handle: evt.Type == socketmode.EventTypeEventsAPI, evt.Data is a VALUE
//     (not pointer) slackevents.EventsAPIEvent, evt.Request is a non-nil
//     *socketmode.Request (so the Ack-before-processing branch runs).
//   - handleEventsAPI: api.Type == slackevents.CallbackEvent (or the whole
//     event is dropped); the switch on api.InnerEvent.Data asserts
//     *slackevents.MessageEvent for the "message" inner-event type.
//   - The MessageEvent case then requires ev.SubType == "" (not an edit/join
//     subtype), ev.User != "" and != botUserID, and routes on
//     ev.ChannelType == "im" to handleDM using ev.User/ev.Channel/ev.Text and
//     a thread_ts resolved from ev.ThreadTimeStamp/ev.TimeStamp.
func (c *Client) InjectDM(userID, channelID, text string) (ts string, ev socketmode.Event) {
	c.mu.Lock()
	ts = c.nextTS()
	c.byChannel[channelID] = append(c.byChannel[channelID],
		&Message{ChannelID: channelID, TS: ts, Text: text, UserID: userID})
	c.mu.Unlock()

	inner := &slackevents.MessageEvent{
		Type:        "message",
		Channel:     channelID,
		User:        userID,
		Text:        text,
		TimeStamp:   ts,
		ChannelType: slackevents.ChannelTypeIM,
	}
	api := slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: "message", Data: inner},
	}
	return ts, socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    api,
		Request: &socketmode.Request{},
	}
}

// InjectDMWithFiles is InjectDM plus one or more file attachments (from
// SeedFile). Slack tags ANY message that carries a file with the file_share
// SubType, with or without accompanying text, and — unlike AppMentionEvent
// (which carries Files directly) — MessageEvent's Files live nested under
// Message.Files; the real listener's slackMessageEventFiles reads exactly
// that nesting (see the comment on the *slackevents.MessageEvent case in
// listener.go, and TestListener_DMFileShare_PopulatesAttachmentsUngated in
// attachments_test.go for the shape this mirrors).
func (c *Client) InjectDMWithFiles(userID, channelID, text string, files []slackapi.File) (ts string, ev socketmode.Event) {
	c.mu.Lock()
	ts = c.nextTS()
	c.byChannel[channelID] = append(c.byChannel[channelID],
		&Message{ChannelID: channelID, TS: ts, Text: text, UserID: userID, SubType: slackapi.MsgSubTypeFileShare})
	c.mu.Unlock()

	inner := &slackevents.MessageEvent{
		Type:        "message",
		SubType:     slackapi.MsgSubTypeFileShare,
		Channel:     channelID,
		User:        userID,
		Text:        text,
		TimeStamp:   ts,
		ChannelType: slackevents.ChannelTypeIM,
		Message:     &slackapi.Msg{Files: files},
	}
	api := slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: "message", Data: inner},
	}
	return ts, socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    api,
		Request: &socketmode.Request{},
	}
}

// InjectMention records a channel @-mention as a top-level message (same
// rationale as InjectDM) and returns the socketmode.Event for an app_mention
// callback. handleEventsAPI's *slackevents.AppMentionEvent case requires
// ev.User != botUserID and ev.BotID == "" to route (via handleChannelMessage
// with newThreadAllowed=true), keyed on ev.Channel/ev.User/ev.Text and
// ev.TimeStamp/ev.ThreadTimeStamp (empty here — a fresh top-level mention).
func (c *Client) InjectMention(userID, channelID, text string) (ts string, ev socketmode.Event) {
	c.mu.Lock()
	ts = c.nextTS()
	c.byChannel[channelID] = append(c.byChannel[channelID],
		&Message{ChannelID: channelID, TS: ts, Text: text, UserID: userID})
	c.mu.Unlock()

	inner := &slackevents.AppMentionEvent{
		Type:      "app_mention",
		Channel:   channelID,
		User:      userID,
		Text:      text,
		TimeStamp: ts,
	}
	api := slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: "app_mention", Data: inner},
	}
	return ts, socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    api,
		Request: &socketmode.Request{},
	}
}

// InjectMentionInThread records a channel @-mention that is a REPLY within an
// existing thread (ev.ThreadTimeStamp == threadTS), so it resolves to the same
// channelKey ("thread:<channel>:<threadTS>") as the thread's root. This is how
// a SECOND user summons the bot into a thread another user started — the path a
// terminal-thread takeover needs. Mirrors InjectMention otherwise.
func (c *Client) InjectMentionInThread(userID, channelID, text, threadTS string) (ts string, ev socketmode.Event) {
	c.mu.Lock()
	ts = c.nextTS()
	c.byChannel[channelID] = append(c.byChannel[channelID],
		&Message{ChannelID: channelID, TS: ts, ThreadTS: threadTS, Text: text, UserID: userID})
	c.mu.Unlock()

	inner := &slackevents.AppMentionEvent{
		Type:            "app_mention",
		Channel:         channelID,
		User:            userID,
		Text:            text,
		TimeStamp:       ts,
		ThreadTimeStamp: threadTS,
	}
	api := slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: "app_mention", Data: inner},
	}
	return ts, socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    api,
		Request: &socketmode.Request{},
	}
}
