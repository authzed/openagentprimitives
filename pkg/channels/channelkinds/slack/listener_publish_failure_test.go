// pkg/channels/channelkinds/slack/listener_publish_failure_test.go
//
// Tests for the user-visible "couldn't record your decision / reach the agent"
// surfaces raised when a click/mention's NATS publish fails. Without these the
// approver/requester is left STRANDED: a parked approval or scope-change request
// hangs forever with no feedback. Each test injects a NATSPublish that fails and
// asserts the listener surfaces a notice via the click's response_url (or, when
// there is none, an ephemeral) BEFORE returning the publish error.
package slack

import (
	"context"
	"errors"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// recordingResponseURLPost captures one postToResponseURL invocation so a test
// can assert the user-facing failure notice was surfaced.
type recordingResponseURLPost struct {
	url  string
	body any
}

// failingPublish is a NATSPublish that always errors, simulating an unreachable
// agent / NATS hiccup at click time.
func failingPublish(string, []byte) error {
	return errors.New("nats: connection closed")
}

// responseTextOf extracts the "text" field from a recorded response_url body
// (which the surfacer builds as map[string]any{"replace_original":..,"text":..}).
func responseTextOf(t *testing.T, body any) string {
	t.Helper()
	m, ok := body.(map[string]any)
	require.True(t, ok, "response_url body must be a map[string]any")
	s, ok := m["text"].(string)
	require.True(t, ok, "response_url body must carry a string text")
	return s
}

// Site 1: permission_request approve/deny click — publish-decision failure must
// surface to the approver via cb.ResponseURL so the buttons aren't left live
// with the requester's session-join hanging silently. permission_request's
// Approve/Deny buttons carry the shared discInteraction encoding, so this
// exercises handleInteractionDecisionClick — the same path identity_choice
// uses.
func TestOnInteractionPermissionRequest_PublishFails_SurfacesToApprover(t *testing.T) {
	value := encodeInteractionButtonValue("perm-req-1", "approve", "permission_request", "default/sess1")

	var posts []recordingResponseURLPost
	l := &slackListener{
		deps: channelkinds.Deps{NATSPublish: failingPublish},
		responseURLPoster: func(_ context.Context, url string, body any) error {
			posts = append(posts, recordingResponseURLPost{url, body})
			return nil
		},
	}
	cb := slackapi.InteractionCallback{
		Type:        slackapi.InteractionTypeBlockActions,
		User:        slackapi.User{ID: "U_APPROVER"},
		ResponseURL: "https://hooks.slack.com/actions/perm",
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "approve", Value: value},
			},
		},
	}

	err := l.onInteraction(context.Background(), cb)
	require.Error(t, err, "publish failure must still be returned (don't swallow)")
	require.Len(t, posts, 1, "approver must be told their click didn't register")
	assert.Equal(t, "https://hooks.slack.com/actions/perm", posts[0].url, "surfaced via cb.ResponseURL")
	assert.Contains(t, responseTextOf(t, posts[0].body), "Couldn't record your choice")
}

// Site 1 fallback: when the permission_request click has no response_url, the
// surface falls back to a channel-scoped ephemeral to the approver.
func TestOnInteractionPermissionRequest_PublishFails_NoResponseURL_FallsBackToEphemeral(t *testing.T) {
	value := encodeInteractionButtonValue("perm-req-1", "deny", "permission_request", "default/sess1")

	fc := &fakeSlackClient{}
	l := &slackListener{
		deps:            channelkinds.Deps{NATSPublish: failingPublish},
		ephemeralClient: fc,
	}
	cb := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		User:      slackapi.User{ID: "U_APPROVER"},
		Container: slackapi.Container{ChannelID: "C_DM", MessageTs: "1700000000.000003"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "deny", Value: value},
			},
		},
	}

	err := l.onInteraction(context.Background(), cb)
	require.Error(t, err, "publish failure must still be returned")
	require.Len(t, fc.postEphemeralCalls, 1, "approver must get an ephemeral fallback notice")
	assert.Equal(t, "C_DM", fc.postEphemeralCalls[0].channelID)
	assert.Equal(t, "U_APPROVER", fc.postEphemeralCalls[0].userID)
}

// Site 4: metaagent scope-change click — publish failure must surface to the
// approver via cb.ResponseURL so authzd's missing decision doesn't strand the
// scope-change request.
func TestMetaagentApproval_PublishFails_SurfacesToApprover(t *testing.T) {
	val := MetaagentApprovalButtonValue("req-001", "default/sess1", "approve")

	var posts []recordingResponseURLPost
	l := &slackListener{
		deps: channelkinds.Deps{NATSPublish: failingPublish},
		responseURLPoster: func(_ context.Context, url string, body any) error {
			posts = append(posts, recordingResponseURLPost{url, body})
			return nil
		},
		idents:          NewIdentityCache(8),
		installedTeamID: "T1",
	}
	// Prime identity so the metaagent click canonicalizes to user:<email> and
	// reaches the publish path (the metaagent handler canonicalizes in-listener,
	// so a missing email would short-circuit before the publish under test).
	l.idents.Put(userInfo{UserID: "U_APPROVER", Email: "approver@example.com", TeamID: "T1"})
	cb := slackapi.InteractionCallback{
		Type:        slackapi.InteractionTypeBlockActions,
		User:        slackapi.User{ID: "U_APPROVER"},
		ResponseURL: "https://hooks.slack.com/actions/meta",
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "metaagent_approve_req-001", Value: val},
			},
		},
	}

	err := l.onInteraction(context.Background(), cb)
	require.Error(t, err, "publish failure must still be returned")
	require.Len(t, posts, 1, "approver must be told their scope decision didn't register")
	assert.Equal(t, "https://hooks.slack.com/actions/meta", posts[0].url)
	assert.Contains(t, responseTextOf(t, posts[0].body), "Couldn't record your scope decision")
}

// Site 5: metaagent @-mention inbound — publish of the scope-change request
// fails. The mentioner must get an ephemeral notice rather than silence.
func TestMetaagentMention_PublishFails_SurfacesToMentioner(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	channel := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-ch", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mysession", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "slack-ch",
				spiceboxv1alpha1.LabelChannelKey:  "bbd1119116ce4ed888069aa0a639e19369abcf51078c60f0087e79b8f8cae89",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-ch", Kind: KindName,
				Key: "thread:CHAN:1234.000",
				External: map[string]string{
					"channel_id": "CHAN",
					"thread_ts":  "1234.000",
				},
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(channel, sess).Build()

	fc := &fakeSlackClient{}
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel:     channel,
			K8sClient:   cli,
			NATSPublish: failingPublish,
		},
		seenEvts:           newEventIDCache(64),
		threads:            newThreadIndex(),
		assistantThreads:   map[string]string{},
		botUserID:          "UBOT",
		metaagentBotUserID: "UMETA",
		idents:             NewIdentityCache(8),
		installedTeamID:    "T1",
		ephemeralClient:    fc,
	}
	// Prime identity so canonicalization succeeds and we reach the publish path.
	l.idents.Put(userInfo{UserID: "U_ALICE", Email: "alice@example.com", TeamID: "T1"})
	l.threads.put("CHAN:1234.000", "")

	ev := &slackevents.AppMentionEvent{
		User:            "U_ALICE",
		Channel:         "CHAN",
		ThreadTimeStamp: "1234.000",
		TimeStamp:       "1234.001",
		Text:            "<@UMETA> allow read on linear",
	}
	l.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: ev,
		},
	})

	require.Len(t, fc.postEphemeralCalls, 1, "mentioner must get a failure notice, not silence")
	assert.Equal(t, "CHAN", fc.postEphemeralCalls[0].channelID)
	assert.Equal(t, "U_ALICE", fc.postEphemeralCalls[0].userID)
}
