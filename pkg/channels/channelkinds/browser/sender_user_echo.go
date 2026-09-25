// The "user_echo" sub-channel for the browser page: the outbound mirror of a
// view-originated message (typed in the artifact view or another chat tab)
// surfaced in THIS chat's timeline, so every view of the session sees the same
// conversation.
package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// userEchoSender implements channelkinds.Sender for the browser "user_echo"
// sub-channel. It only translates the envelope into a MsgUserEcho render
// event — no @mention resolution (the browser page renders a plain author
// label, not a channel-native mention).
type userEchoSender struct {
	sink EventSink
}

// Send conforms to channelkinds.Sender.
func (s *userEchoSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	var p channelevents.UserEchoPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC()})
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser user_echo sender: parse payload: %w", err)
	}
	s.sink.Emit(MsgUserEcho{
		Session:   refOf(sess),
		Text:      p.Text,
		Author:    echoAuthorLabel(p.Author),
		Via:       p.Via,
		RequestID: p.RequestID,
	})
	return channelkinds.SubChannelSendResult{}, nil
}

// echoAuthorLabel picks the best human-readable label for an echoed message's
// author: their verified email, else their channel-native external id, else a
// generic placeholder. Never empty. Mirrors slack's fallbackAuthorName — the
// browser page has no @mention directory, so this label is what it renders.
func echoAuthorLabel(author channelevents.ExternalIdentity) string {
	switch {
	case author.Email != "":
		return author.Email.String()
	case author.ExternalID != "":
		return author.ExternalID.String()
	default:
		return "someone"
	}
}
