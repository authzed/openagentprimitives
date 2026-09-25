package slack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestSubChannelSenderCoversMetaagent is the RED test for the Slack Block Kit
// that lived inside internal/cmd/channelsd's metaagent handlers.
//
// channelkinds.Kind already carries the seam for a kind-specific outbound
// surface — SubChannelSender, whose contract is "nil = this kind doesn't
// implement that sub-channel; callers degrade gracefully". Routing the two
// metaagent surfaces through it is what lets the generic handler stop
// importing slack-go, and what makes a future kind able to answer by
// implementing a sender rather than by editing channelsd.
func TestSubChannelSenderCoversMetaagent(t *testing.T) {
	k := &Kind{}
	for _, name := range []string{
		channelkinds.SubChannelMetaagentScopeApproval,
		channelkinds.SubChannelMetaagentNotice,
	} {
		t.Run(name+": slack returns a sender", func(t *testing.T) {
			assert.NotNil(t, k.SubChannelSender(name, channelkinds.Deps{}),
				"slack must own its metaagent rendering; a nil sender here means the "+
					"Block Kit is still living in the transport-agnostic binary")
		})
	}
}

// approvalEnvelope wraps a payload the way the generic channelsd handler does:
// the raw authzd JSON, verbatim, in the envelope's Payload.
func approvalEnvelope(t *testing.T, pl scope.MetaagentApprovalPayload) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(pl)
	require.NoError(t, err)
	return channelevents.Envelope{Version: 1, Payload: b}
}

func noticeEnvelope(t *testing.T, pl channelkinds.MetaagentNoticePayload) channelevents.Envelope {
	t.Helper()
	b, err := json.Marshal(pl)
	require.NoError(t, err)
	return channelevents.Envelope{Version: 1, Payload: b}
}

func metaagentSession(external map[string]string, annotations map[string]string) channelkinds.SessionInfo {
	return channelkinds.SessionInfo{
		Namespace:   "default",
		Name:        "sess1",
		Channel:     &spiceboxv1alpha1.ChannelBinding{Kind: KindName, External: external},
		Annotations: annotations,
	}
}

// TestMetaagentScopeApprovalSenderRoutesColdStartToTheStarter pins the routing
// rule: a cold-start approval goes EPHEMERAL to the session starter (who is
// the approver), and only the starter's raw Slack
// id — carried on the session annotation, not in the payload, whose Requester
// is canonical — can address it. A missing starter id must NOT drop the prompt:
// the runner is blocked on this decision, so a visible prompt to a wider
// audience beats an invisible one.
func TestMetaagentScopeApprovalSenderRoutesColdStartToTheStarter(t *testing.T) {
	cases := []struct {
		name            string
		coldStart       bool
		annotations     map[string]string
		wantEphemeral   bool
		wantEphemeralTo string
	}{
		{
			name:            "cold start with a known starter: ephemeral to the starter",
			coldStart:       true,
			annotations:     map[string]string{spiceboxv1alpha1.AnnotationStartedByExternalID: "U_STARTER"},
			wantEphemeral:   true,
			wantEphemeralTo: "U_STARTER",
		},
		{
			name:          "cold start with no starter id: posted in-thread rather than dropped",
			coldStart:     true,
			annotations:   nil,
			wantEphemeral: false,
		},
		{
			name:          "mid-session request: posted in-thread for the whole thread to see",
			coldStart:     false,
			annotations:   map[string]string{spiceboxv1alpha1.AnnotationStartedByExternalID: "U_STARTER"},
			wantEphemeral: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeSlackClient{}
			s := &metaagentScopeApprovalSender{client: fc, refs: newMetaagentApprovalRefCache()}

			res, err := s.Send(context.Background(),
				metaagentSession(map[string]string{"channel_id": "C1", "thread_ts": "111.1"}, tc.annotations),
				approvalEnvelope(t, scope.MetaagentApprovalPayload{
					RequestID: "req-1", Requester: "U_ASKER", Verbatim: "let me read X",
					ApproverSummary: "adds read on X", ColdStart: tc.coldStart, CleanedTask: "read X",
				}))
			require.NoError(t, err)
			assert.Equal(t, "req-1", res.RequestRef, "the request id must ride back for the pending-requester stamp")

			if tc.wantEphemeral {
				require.Len(t, fc.postEphemeralCalls, 1)
				assert.Equal(t, "C1", fc.postEphemeralCalls[0].channelID)
				assert.Equal(t, tc.wantEphemeralTo, fc.postEphemeralCalls[0].userID)
				assert.Empty(t, fc.postMessageCalls)
			} else {
				require.Len(t, fc.postMessageCalls, 1)
				assert.Equal(t, "C1", fc.postMessageCalls[0].channelID)
				assert.Empty(t, fc.postEphemeralCalls)
			}
		})
	}
}

// TestMetaagentScopeApprovalSenderRecordsTheRef keeps Show Details working:
// the ref must be recorded next to the post, so a click renders the real
// request instead of the degraded fallback. Recording it in the sender rather
// than in the caller is what stops the two drifting.
func TestMetaagentScopeApprovalSenderRecordsTheRef(t *testing.T) {
	refs := newMetaagentApprovalRefCache()
	s := &metaagentScopeApprovalSender{client: &fakeSlackClient{}, refs: refs}

	_, err := s.Send(context.Background(),
		metaagentSession(map[string]string{"channel_id": "C9", "thread_ts": "222.2"}, nil),
		approvalEnvelope(t, scope.MetaagentApprovalPayload{
			RequestID: "req-9", Verbatim: "v", ApproverSummary: "s", CleanedTask: "t", ColdStart: true,
		}))
	require.NoError(t, err)

	got, ok := refs.get("req-9")
	require.True(t, ok, "the posted approval must be recorded for Show Details")
	assert.Equal(t, "C9", got.ChannelID, "the ref must locate where the prompt was posted")
	assert.Equal(t, "222.2", got.ThreadTS)
	assert.True(t, got.ColdStart)
	assert.Equal(t, "t", got.CleanedTask)
}

// TestMetaagentSendersFailLoudWithoutRouting pins the no-silent-errors half.
// A binding with no channel_id, or a payload with no body, must return an
// error the caller logs — not a silent success that leaves a human waiting.
func TestMetaagentSendersFailLoudWithoutRouting(t *testing.T) {
	ctx := context.Background()
	approval := &metaagentScopeApprovalSender{client: &fakeSlackClient{}, refs: newMetaagentApprovalRefCache()}
	notice := &metaagentNoticeSender{client: &fakeSlackClient{}}

	_, err := approval.Send(ctx, metaagentSession(map[string]string{}, nil),
		approvalEnvelope(t, scope.MetaagentApprovalPayload{RequestID: "r"}))
	assert.ErrorContains(t, err, "channel_id", "a binding with no channel must error, not drop")

	_, err = approval.Send(ctx, channelkinds.SessionInfo{Namespace: "default", Name: "sess1"},
		approvalEnvelope(t, scope.MetaagentApprovalPayload{RequestID: "r"}))
	assert.ErrorContains(t, err, "no channel binding")

	_, err = notice.Send(ctx, metaagentSession(map[string]string{"channel_id": "C1"}, nil),
		noticeEnvelope(t, channelkinds.MetaagentNoticePayload{Requester: "U1", Body: "  "}))
	assert.ErrorContains(t, err, "no body")

	_, err = notice.Send(ctx, metaagentSession(map[string]string{"channel_id": "C1"}, nil),
		noticeEnvelope(t, channelkinds.MetaagentNoticePayload{Body: "hi"}))
	assert.ErrorContains(t, err, "cannot address recipient",
		"a notice with no addressable recipient must surface, not post to nobody")
}

// TestMetaagentNoticeSenderPostsEphemeralInThread covers the happy path: the
// notice is private to its recipient and lands in the session's thread.
func TestMetaagentNoticeSenderPostsEphemeralInThread(t *testing.T) {
	fc := &fakeSlackClient{}
	s := &metaagentNoticeSender{client: fc}

	_, err := s.Send(context.Background(),
		metaagentSession(map[string]string{"channel_id": "C2", "thread_ts": "333.3"}, nil),
		noticeEnvelope(t, channelkinds.MetaagentNoticePayload{Requester: "U_BOB", Body: "scope applied"}))
	require.NoError(t, err)

	require.Len(t, fc.postEphemeralCalls, 1)
	assert.Equal(t, "C2", fc.postEphemeralCalls[0].channelID)
	assert.Equal(t, "U_BOB", fc.postEphemeralCalls[0].userID)
	assert.Empty(t, fc.postMessageCalls, "a notice is private; it must never be posted to the channel")
}
