package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func replyFixture(t *testing.T) (channelevents.OutboundUserMessagePayload, delivery.Intent, delivery.Receipt) {
	t.Helper()
	p := channelevents.OutboundUserMessagePayload{Text: "Stretch"}
	var err error
	p.Delivery, err = channelevents.NewDeliveryOperation("uid", "call", p)
	require.NoError(t, err)
	i := delivery.Intent{Session: channelevents.SessionRef{Namespace: "team", Name: "root"}, Destination: delivery.Destination{Kind: "browser", ChannelUID: "channel", BindingDigest: "pin", Recipient: "owner"}, Payload: p, CreatedAt: time.Now().UTC()}
	digest, err := i.Destination.Digest()
	require.NoError(t, err)
	r := delivery.Receipt{OperationID: p.Delivery.ID, SessionUID: "uid", PayloadDigest: p.Delivery.PayloadDigest, DestinationDigest: digest, Transport: "browser-transcript", Reference: "reply-receipt", AcceptedAt: time.Now().UTC()}
	return p, i, r
}

func TestGoalReplyAcceptorWaitsForMatchingReceipt(t *testing.T) {
	p, i, r := replyFixture(t)
	calls := 0
	accept := GoalReplyAcceptor(func(_ context.Context, req goals.Request) (goals.Response, error) {
		calls++
		require.Equal(t, "prepare_reply", req.Operation)
		require.Equal(t, p, *req.Reply)
		reply := &goals.RunReply{Intent: i, State: "prepared"}
		if calls == 2 {
			reply.State, reply.Receipt = "accepted", &r
		}
		return goals.Response{Run: &goals.Occurrence{Reply: reply}}, nil
	})
	got, err := accept(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, &r, got)
	require.Equal(t, 2, calls)
	wrong := GoalReplyAcceptor(func(context.Context, goals.Request) (goals.Response, error) {
		changed := i
		changed.Payload.Text = "another body"
		return goals.Response{Run: &goals.Occurrence{Reply: &goals.RunReply{Intent: changed, State: "accepted", Receipt: &r}}}, nil
	})
	_, err = wrong(context.Background(), p)
	require.ErrorIs(t, err, delivery.ErrConflict)
}

func TestRespondDurableAcceptancePrecedesLivePublish(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed acceptance", true: "accepted but live notification lost"}[accepted], func(t *testing.T) {
			_, _, receipt := replyFixture(t)
			acceptanceChecked := false
			publishes := 0
			var note map[string]any
			cfg := RespondConfig{ChannelKind: "fake", Capabilities: []string{"text"}, AcceptReply: func(_ context.Context, p channelevents.OutboundUserMessagePayload) (*delivery.Receipt, error) {
				acceptanceChecked = true
				require.Equal(t, receipt.OperationID, p.Delivery.ID)
				if !accepted {
					return nil, errors.New("acknowledgement unknown")
				}
				return &receipt, nil
			}, NATSPublish: func(context.Context, string, []byte) error {
				require.True(t, acceptanceChecked)
				publishes++
				return errors.New("live bus unavailable")
			}, AppendSystemNote: func(_ context.Context, n map[string]any) error { note = n; return nil }}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			ctx = sandbox.WithIDs(ctx, sandbox.IDs{ToolUseID: "call"})
			result, err := newRespondTool(cfg).Execute(ctx, json.RawMessage(`{"text":"Stretch"}`), &tool.SessionContext{Namespace: "team", Name: "root", AgentSessionUID: "uid"})
			require.NoError(t, err)
			if accepted {
				require.False(t, result.IsError, result.Content)
				require.Positive(t, publishes)
				require.Contains(t, result.Content, "does not prove the user read it")
				require.Equal(t, &receipt, note["replyReceipt"])
				require.NotContains(t, note, "replyPublication", "failed publication is not recorded as successful")
			} else {
				require.True(t, result.IsError)
				require.Zero(t, publishes)
				require.Nil(t, note)
			}
		})
	}
}

// TestGoalReplyAcceptorReturnsAtHandOff: an unconfirmed reply is terminal at
// the operator. The acceptor returns at once with no receipt, and does not
// poll again: a repeat could only be refused as a second post.
func TestGoalReplyAcceptorReturnsAtHandOff(t *testing.T) {
	p, i, _ := replyFixture(t)
	calls := 0
	accept := GoalReplyAcceptor(func(context.Context, goals.Request) (goals.Response, error) {
		calls++
		return goals.Response{Run: &goals.Occurrence{Reply: &goals.RunReply{Intent: i, State: goals.ReplyUnconfirmed}}}, nil
	})
	got, err := accept(context.Background(), p)
	require.NoError(t, err)
	require.Nil(t, got, "an unconfirmed hand-off has no receipt")
	require.Equal(t, 1, calls)
	forged := GoalReplyAcceptor(func(context.Context, goals.Request) (goals.Response, error) {
		_, _, r := replyFixture(t)
		return goals.Response{Run: &goals.Occurrence{Reply: &goals.RunReply{Intent: i, State: goals.ReplyUnconfirmed, Receipt: &r}}}, nil
	})
	_, err = forged(context.Background(), p)
	require.ErrorIs(t, err, delivery.ErrConflict, "an unconfirmed reply carrying a receipt is malformed")
}

// TestRespondUnconfirmedGoalReply: a hand-off publishes the envelope exactly
// once for the relay to post, records the publication but no receipt, and
// tells the agent delivery is unconfirmed. The leak gate still runs first:
// a refused reply is never handed off or published.
func TestRespondUnconfirmedGoalReply(t *testing.T) {
	for _, leak := range []bool{false, true} {
		t.Run(map[bool]string{false: "handed off: one publish, no receipt, unconfirmed result", true: "leak gate refuses: no hand-off, no publish"}[leak], func(t *testing.T) {
			handoffs, publishes := 0, 0
			var published []string
			var note map[string]any
			cfg := RespondConfig{ChannelKind: "slack", Capabilities: []string{"text"},
				LeakageGate: func(context.Context, *tool.SessionContext, string, []channelevents.AttachmentRef) error {
					if leak {
						return errors.New("source not readable by the conversation")
					}
					return nil
				},
				AcceptReply: func(context.Context, channelevents.OutboundUserMessagePayload) (*delivery.Receipt, error) {
					handoffs++
					return nil, nil
				},
				NATSPublish: func(_ context.Context, _ string, data []byte) error {
					published = append(published, string(data))
					if strings.Contains(string(data), "Standup summary") {
						require.Equal(t, 1, handoffs, "the hand-off is recorded before the publish")
						publishes++
					}
					return nil
				},
				AppendSystemNote: func(_ context.Context, n map[string]any) error { note = n; return nil }}
			ctx := sandbox.WithIDs(context.Background(), sandbox.IDs{ToolUseID: "call"})
			result, err := newRespondTool(cfg).Execute(ctx, json.RawMessage(`{"text":"Standup summary"}`), &tool.SessionContext{Namespace: "team", Name: "run", AgentSessionUID: "uid"})
			if leak {
				require.True(t, err != nil || result.IsError, "a leaking reply is refused")
				require.Zero(t, handoffs)
				require.Zero(t, publishes, "the reply itself is never published; only the gate's generic notice is: %v", published)
				return
			}
			require.NoError(t, err)
			require.False(t, result.IsError, result.Content)
			require.Equal(t, 1, handoffs)
			require.Equal(t, 1, publishes)
			require.Len(t, published, 1, "one envelope, for the relay's one post")
			require.Contains(t, result.Content, "not confirmed")
			require.NotContains(t, result.Content, "accepted")
			require.NotEqual(t, "delivered", result.Content)
			require.Contains(t, note, "replyPublication")
			require.NotContains(t, note, "replyReceipt")
		})
	}
}

// handOffServer is a goal server for one hand-off: it records the reply once
// per operation, as the store does, and can lose its first response.
type handOffServer struct {
	t              *testing.T
	op             string
	state          string
	loseFirst      bool
	prepares       int
	publishFailure []string
}

func (s *handOffServer) call(_ context.Context, req goals.Request) (goals.Response, error) {
	require.NotNil(s.t, req.Reply)
	switch req.Operation {
	case "prepare_reply":
		s.prepares++
		if s.op != "" && s.op != req.Reply.Delivery.ID {
			return goals.Response{}, goals.ErrReplyHandedOff
		}
		if s.op == "" {
			s.op, s.state = req.Reply.Delivery.ID, goals.ReplyUnconfirmed
		}
		if s.loseFirst && s.prepares == 1 {
			return goals.Response{}, errors.New("response lost")
		}
		return goals.Response{Run: &goals.Occurrence{Reply: &goals.RunReply{Intent: delivery.Intent{Payload: *req.Reply}, State: s.state}}}, nil
	case "reply_publish_failed":
		s.publishFailure = append(s.publishFailure, req.Reply.Delivery.ID)
		s.state = goals.ReplyPublishFailed
		return goals.Response{Run: &goals.Occurrence{Reply: &goals.RunReply{Intent: delivery.Intent{Payload: *req.Reply}, State: s.state}}}, nil
	}
	s.t.Fatalf("unexpected operation %q", req.Operation)
	return goals.Response{}, nil
}

func (s *handOffServer) respond(publish func(context.Context, string, []byte) error) RespondConfig {
	return RespondConfig{ChannelKind: "slack", Capabilities: []string{"text"},
		AcceptReply:        GoalReplyAcceptor(s.call),
		ReplyPublishFailed: GoalReplyPublishFailed(s.call),
		NATSPublish:        publish,
	}
}

func runRespond(t *testing.T, cfg RespondConfig, toolUseID, text string) tool.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(sandbox.WithIDs(context.Background(), sandbox.IDs{ToolUseID: toolUseID}), 30*time.Second)
	defer cancel()
	result, err := newRespondTool(cfg).Execute(ctx, json.RawMessage(`{"text":"`+text+`"}`), &tool.SessionContext{Namespace: "team", Name: "run", AgentSessionUID: "uid"})
	require.NoError(t, err)
	return result
}

// TestGoalReplyHandOffRecovery: a lost hand-off response leaves the call
// retryable (the same call hands off again and publishes once); a publish
// failure is recorded as publish_failed; a different reply is refused with a
// specific message.
func TestGoalReplyHandOffRecovery(t *testing.T) {
	t.Run("lost prepare_reply response: the resumed call hands off again and publishes once", func(t *testing.T) {
		s := &handOffServer{t: t, loseFirst: true}
		publishes := 0
		cfg := s.respond(func(context.Context, string, []byte) error { publishes++; return nil })
		first := runRespond(t, cfg, "call", "Standup summary")
		require.True(t, first.IsError)
		require.Zero(t, publishes)
		second := runRespond(t, cfg, "call", "Standup summary")
		require.False(t, second.IsError, second.Content)
		assert.Equal(t, 1, publishes)
		assert.Equal(t, goals.ReplyUnconfirmed, s.state)
	})
	t.Run("publish fails after hand-off: recorded publish_failed, agent told nothing was posted", func(t *testing.T) {
		s := &handOffServer{t: t}
		cfg := s.respond(func(context.Context, string, []byte) error { return errors.New("bus down") })
		ctx, cancel := context.WithTimeout(sandbox.WithIDs(context.Background(), sandbox.IDs{ToolUseID: "call"}), 50*time.Millisecond)
		defer cancel()
		result, err := newRespondTool(cfg).Execute(ctx, json.RawMessage(`{"text":"Standup summary"}`), &tool.SessionContext{Namespace: "team", Name: "run", AgentSessionUID: "uid"})
		require.NoError(t, err)
		require.True(t, result.IsError)
		assert.Contains(t, result.Content, "publish_failed")
		assert.Contains(t, result.Content, "nothing was posted")
		assert.Equal(t, []string{s.op}, s.publishFailure)
		assert.Equal(t, goals.ReplyPublishFailed, s.state)
	})
	t.Run("a conflict that is not a hand-off: reported as unconfirmed, never as already handed off", func(t *testing.T) {
		publishes := 0
		cfg := RespondConfig{ChannelKind: "slack", Capabilities: []string{"text"},
			AcceptReply: GoalReplyAcceptor(func(context.Context, goals.Request) (goals.Response, error) {
				return goals.Response{}, fmt.Errorf("%w: the run is no longer current", goals.ErrConflict)
			}),
			NATSPublish: func(context.Context, string, []byte) error { publishes++; return nil },
		}
		result := runRespond(t, cfg, "call", "Standup summary")
		require.True(t, result.IsError)
		assert.Contains(t, result.Content, "Goal reply acceptance is unconfirmed")
		assert.NotContains(t, result.Content, "handed off")
		assert.Zero(t, publishes, "nothing is published for a refused reply")
	})
	t.Run("a different reply after a hand-off: refused as already handed off", func(t *testing.T) {
		s := &handOffServer{t: t}
		publishes := 0
		cfg := s.respond(func(context.Context, string, []byte) error { publishes++; return nil })
		require.False(t, runRespond(t, cfg, "call", "Standup summary").IsError)
		second := runRespond(t, cfg, "another-call", "Something else")
		require.True(t, second.IsError)
		assert.Contains(t, second.Content, "already handed off for posting")
		assert.Equal(t, 1, publishes)
	})
}
