// pkg/channels/channelkinds/slack/metaagent_listener_test.go
//
// Tests for F1/F4 (metaagent mention routing) and F5 (button callbacks).
package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
)

// ---------------------------------------------------------------------------
// F1/F4: AppMentionEvent routing
// ---------------------------------------------------------------------------

// TestHandleEventsAPI_MetaagentMention_Publishes verifies that an app_mention
// event whose text contains <@METAAGENT_BOT_ID> is published to the
// metaagent_request subject instead of going through handleChannelMessage.
func TestHandleEventsAPI_MetaagentMention_Publishes(t *testing.T) {
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
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(channel, sess).
		Build()

	var published []struct {
		subject string
		payload []byte
	}
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel:   channel,
			K8sClient: cli,
			NATSPublish: func(subject string, payload []byte) error {
				published = append(published, struct {
					subject string
					payload []byte
				}{subject, payload})
				return nil
			},
		},
		seenEvts:           newEventIDCache(64),
		threads:            newThreadIndex(),
		assistantThreads:   map[string]string{},
		botUserID:          "UBOT",
		metaagentBotUserID: "UMETA",
		idents:             NewIdentityCache(8),
		installedTeamID:    "T1",
	}
	// Prime the identity cache so the mid-session requester canonicalizes
	// without a live users.info call (channelsd now publishes a canonical
	// "user:<...>" subject so authzd's manage_scope gate can match started_by).
	l.idents.Put(userInfo{UserID: "U_ALICE", Email: "alice@example.com", TeamID: "T1"})
	// Register the thread so the session can be found.
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

	// Must have published exactly one NATS message.
	require.Len(t, published, 1, "expected exactly one NATS publish")
	assert.True(t,
		strings.HasSuffix(published[0].subject, ".in.metaagent_request"),
		"subject %q must end in .in.metaagent_request", published[0].subject)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(published[0].payload, &payload), "unmarshal payload")
	// The requester is now the canonical SpiceDB subject "user:<canonical>",
	// not the raw Slack user_id — so authzd's manage_scope gate can match
	// started_by@user:<canonical>.
	wantCanonical := "user:" + CanonicalID("alice@example.com", "T1", "U_ALICE")
	assert.Equal(t, wantCanonical, payload["requester"], "requester must be canonicalized")
	assert.True(t, strings.HasPrefix(payload["requester"], "user:"), "requester is the canonical subject form")
	// The mention markup should be stripped.
	assert.Equal(t, "allow read on linear", payload["text"], "text should have mention stripped")
}

// noopInbound is a stub InboundPipeline that returns a zero InboundDecision.
type noopInbound struct{}

func (noopInbound) Deliver(_ context.Context, _ channelkinds.InboundEvent) (channelkinds.InboundDecision, error) {
	return channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}, nil
}

// TestHandleEventsAPI_RegularMention_NotRouted verifies that an app_mention
// event that does NOT contain the metaagent bot ID is handled normally (i.e.,
// does NOT publish to metaagent_request).
func TestHandleEventsAPI_RegularMention_NotRouted(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	channel := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-ch", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(channel).Build()

	var publishedSubjects []string
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel:   channel,
			K8sClient: cli,
			Inbound:   noopInbound{},
			NATSPublish: func(subject string, _ []byte) error {
				publishedSubjects = append(publishedSubjects, subject)
				return nil
			},
		},
		seenEvts:           newEventIDCache(64),
		threads:            newThreadIndex(),
		assistantThreads:   map[string]string{},
		botUserID:          "UBOT",
		metaagentBotUserID: "UMETA",
	}

	ev := &slackevents.AppMentionEvent{
		User:      "U_ALICE",
		Channel:   "CHAN",
		TimeStamp: "9999.000",
		Text:      "<@UBOT> hello there", // mentions UBOT, not UMETA
	}
	l.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: ev,
		},
	})

	for _, s := range publishedSubjects {
		assert.False(t, strings.Contains(s, "metaagent_request"),
			"regular mention must not publish to metaagent_request, got subject %q", s)
	}
}

// TestHandleEventsAPI_MetaagentDisabled_AllMentionToRunner verifies that when
// metaagentBotUserID is empty, even a mention containing <@UMETA> is NOT
// routed to metaagent_request (the feature is disabled).
func TestHandleEventsAPI_MetaagentDisabled_AllMentionToRunner(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	channel := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-ch", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(channel).Build()

	var publishedSubjects []string
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel:   channel,
			K8sClient: cli,
			Inbound:   noopInbound{},
			NATSPublish: func(subject string, _ []byte) error {
				publishedSubjects = append(publishedSubjects, subject)
				return nil
			},
		},
		seenEvts:           newEventIDCache(64),
		threads:            newThreadIndex(),
		assistantThreads:   map[string]string{},
		botUserID:          "UBOT",
		metaagentBotUserID: "", // disabled
	}

	ev := &slackevents.AppMentionEvent{
		User:      "U_ALICE",
		Channel:   "CHAN",
		TimeStamp: "8888.000",
		Text:      "<@UMETA> allow write on github",
	}
	l.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "app_mention",
			Data: ev,
		},
	})

	for _, s := range publishedSubjects {
		assert.False(t, strings.Contains(s, "metaagent_request"),
			"disabled feature: no metaagent_request publish, got %q", s)
	}
}

// ---------------------------------------------------------------------------
// stripMetaagentMention helper
// ---------------------------------------------------------------------------

func TestStripMetaagentMention(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		botUserID string
		want      string
	}{
		{
			name:      "strips single mention",
			text:      "<@UMETA> allow read on linear",
			botUserID: "UMETA",
			want:      "allow read on linear",
		},
		{
			name:      "strips all occurrences",
			text:      "<@UMETA> please <@UMETA> do something",
			botUserID: "UMETA",
			want:      "please  do something",
		},
		{
			name:      "trims surrounding whitespace",
			text:      "  <@UMETA>   hello  ",
			botUserID: "UMETA",
			want:      "hello",
		},
		{
			name:      "no mention: unchanged",
			text:      "hello world",
			botUserID: "UMETA",
			want:      "hello world",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripMetaagentMention(tc.text, tc.botUserID))
		})
	}
}

// ---------------------------------------------------------------------------
// F5: metaagent button callbacks via onInteraction
// ---------------------------------------------------------------------------

// TestOnInteraction_MetaagentApprove verifies that clicking "Approve" on a
// metaagent scope-approval block publishes to .in.metaagent_approval_applied
// with approved:true.
func TestOnInteraction_MetaagentApprove(t *testing.T) {
	val := MetaagentApprovalButtonValue("req-001", "default/sess1", "approve")

	var publishedSubject string
	var publishedBody []byte
	l := &slackListener{
		deps: channelkinds.Deps{
			NATSPublish: func(subject string, payload []byte) error {
				publishedSubject = subject
				publishedBody = payload
				return nil
			},
		},
		idents:          NewIdentityCache(8),
		installedTeamID: "T1",
	}
	// The clicker is gated on agentsession#manage_scope (= owner), so the
	// listener resolves their trusted email to canonicalize to user:<email>.
	l.idents.Put(userInfo{UserID: "U_APPROVER", Email: "approver@example.com", TeamID: "T1"})

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_APPROVER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{
					ActionID: "metaagent_approve_req-001",
					Value:    val,
				},
			},
		},
	}
	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")

	require.NotEmpty(t, publishedSubject, "NATSPublish must be called")
	assert.True(t,
		strings.HasSuffix(publishedSubject, ".in.metaagent_approval_applied"),
		"subject %q must end in .in.metaagent_approval_applied", publishedSubject)

	var result map[string]any
	require.NoError(t, json.Unmarshal(publishedBody, &result), "unmarshal payload")
	assert.Equal(t, "req-001", result["requestId"], "requestId")
	assert.Equal(t, true, result["approved"], "approved must be true")
	assert.Equal(t, "U_APPROVER", result["approverId"], "approverId")
	// Subject must encode the session ns/name correctly.
	assert.Contains(t, publishedSubject, "default")
	assert.Contains(t, publishedSubject, "sess1")
}

// TestOnInteraction_MetaagentApprove_PublishesCanonical verifies that the
// approve click publishes the clicker's EMAIL-CANONICAL subject (resolved via
// the identity cache) as approverCanonical, in addition to the raw Slack
// approverId. authzd gates the clicker on agentsession#manage_scope using this
// canonical, so it MUST match the email-keyed owner tuple — not the bare Slack
// user_id (which authzd cannot canonicalize).
func TestOnInteraction_MetaagentApprove_PublishesCanonical(t *testing.T) {
	val := MetaagentApprovalButtonValue("req-canon", "default/sess1", "approve")

	var publishedBody []byte
	l := &slackListener{
		deps: channelkinds.Deps{
			NATSPublish: func(_ string, payload []byte) error {
				publishedBody = payload
				return nil
			},
		},
		idents:          NewIdentityCache(8),
		installedTeamID: "T1",
	}
	// Prime the cache so resolveIdentity returns the trusted email without a
	// live users.info call (mirrors the inbound-mention test setup).
	l.idents.Put(userInfo{UserID: "U_APPROVER", Email: "alice@example.com", TeamID: "T1"})

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_APPROVER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "metaagent_approve_req-canon", Value: val},
			},
		},
	}
	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")

	var result map[string]any
	require.NoError(t, json.Unmarshal(publishedBody, &result), "unmarshal payload")
	// Raw Slack id is kept for display / back-compat.
	assert.Equal(t, "U_APPROVER", result["approverId"], "approverId (raw) preserved")
	// The canonical the email-keyed owner gate will check.
	wantCanonical := "user:" + CanonicalID("alice@example.com", "T1", "U_APPROVER")
	gotCanonical, _ := result["approverCanonical"].(string)
	assert.NotEmpty(t, gotCanonical, "approverCanonical must be published")
	assert.Equal(t, wantCanonical, gotCanonical, "approverCanonical must be the email-keyed canonical subject")
	assert.True(t, strings.HasPrefix(gotCanonical, "user:"), "approverCanonical is the full subject form")
}

// TestOnInteraction_MetaagentDeny verifies the deny path.
func TestOnInteraction_MetaagentDeny(t *testing.T) {
	val := MetaagentApprovalButtonValue("req-002", "ns2/sess2", "deny")

	var publishedBody []byte
	l := &slackListener{
		deps: channelkinds.Deps{
			NATSPublish: func(_ string, payload []byte) error {
				publishedBody = payload
				return nil
			},
		},
		idents:          NewIdentityCache(8),
		installedTeamID: "T1",
	}
	// The clicker is gated on agentsession#manage_scope (= owner), so the
	// listener resolves their trusted email to canonicalize to user:<email>.
	l.idents.Put(userInfo{UserID: "U_ALICE", Email: "alice@example.com", TeamID: "T1"})

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_ALICE"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "metaagent_deny_req-002", Value: val},
			},
		},
	}
	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")

	var result map[string]any
	require.NoError(t, json.Unmarshal(publishedBody, &result), "unmarshal payload")
	assert.Equal(t, false, result["approved"], "approved must be false for deny")
	assert.Equal(t, "req-002", result["requestId"], "requestId")
}

// TestOnInteraction_MetaagentColdStartActions verifies that clicking each of
// the cold-start "proceed" buttons publishes metaagent_approval_applied with
// the correct approved boolean AND the action string (so authzd's cold-start
// decider can pick among the five actions), and that cold-start deny publishes
// approved:false,action:deny.
func TestOnInteraction_MetaagentColdStartActions(t *testing.T) {
	cases := []struct {
		name         string
		decision     string
		wantApproved bool
	}{
		{name: "approve_cleaned → approved:true,action:approve_cleaned", decision: MetaagentDecisionApproveCleaned, wantApproved: true},
		{name: "approve_original → approved:true,action:approve_original", decision: MetaagentDecisionApproveOriginal, wantApproved: true},
		{name: "run_without_scope → approved:true,action:run_without_scope", decision: MetaagentDecisionRunWithoutScope, wantApproved: true},
		{name: "cold-start deny → approved:false,action:deny", decision: MetaagentDecisionDeny, wantApproved: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val := MetaagentApprovalButtonValue("req-cs", "default/coldsess", tc.decision)

			var publishedSubject string
			var publishedBody []byte
			l := &slackListener{
				deps: channelkinds.Deps{
					NATSPublish: func(subject string, payload []byte) error {
						publishedSubject = subject
						publishedBody = payload
						return nil
					},
				},
				idents:          NewIdentityCache(8),
				installedTeamID: "T1",
			}
			// The clicker is gated on agentsession#manage_scope (= owner),
			// so the listener resolves their trusted email to canonicalize
			// to user:<email>.
			l.idents.Put(userInfo{UserID: "U_APPROVER", Email: "approver@example.com", TeamID: "T1"})
			cb := slackapi.InteractionCallback{
				Type: slackapi.InteractionTypeBlockActions,
				User: slackapi.User{ID: "U_APPROVER"},
				ActionCallback: slackapi.ActionCallbacks{
					BlockActions: []*slackapi.BlockAction{
						{ActionID: "metaagent_" + tc.decision + "_req-cs", Value: val},
					},
				},
			}
			require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")

			require.NotEmpty(t, publishedSubject, "NATSPublish must be called")
			assert.True(t,
				strings.HasSuffix(publishedSubject, ".in.metaagent_approval_applied"),
				"subject %q must end in .in.metaagent_approval_applied", publishedSubject)

			var result map[string]any
			require.NoError(t, json.Unmarshal(publishedBody, &result), "unmarshal payload")
			assert.Equal(t, "req-cs", result["requestId"], "requestId")
			assert.Equal(t, tc.wantApproved, result["approved"], "approved")
			assert.Equal(t, tc.decision, result["action"], "action must carry the clicked decision")
		})
	}
}

// TestOnInteraction_MetaagentShowDetails_NoPublish verifies that the
// show_details button does NOT publish to metaagent_approval_applied
// (it posts an ephemeral instead). Here l.api is nil so the ephemeral
// post is a no-op; we just assert no NATS publish occurred.
func TestOnInteraction_MetaagentShowDetails_NoPublish(t *testing.T) {
	val := MetaagentApprovalButtonValue("req-003", "ns3/sess3", "show_details")

	published := false
	l := &slackListener{
		deps: channelkinds.Deps{
			NATSPublish: func(_ string, _ []byte) error {
				published = true
				return nil
			},
		},
		// api is nil — postEphemeral is a no-op.
	}

	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_ALICE"},
		Container: slackapi.Container{
			ChannelID: "CHAN", MessageTs: "1234.567",
		},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "metaagent_show_details_req-003", Value: val},
			},
		},
	}
	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")
	assert.False(t, published, "show_details must NOT publish to NATS")
}

// recordingEphemeralAPI is a *slackapi.Client backed by an httptest server
// that acks chat.postEphemeral and records the "text" form value sent.
// slackListener.api is the concrete *slackapi.Client (not the slackClient
// interface fakeSlackClient satisfies for the sender), so handler tests that
// need to inspect a posted ephemeral's text drive the real client against a
// fake HTTP endpoint rather than swapping in a mock — same approach as
// recordingSlackAPI (listener_terminal_continuation_test.go) and
// recordingViewsOpenAPI (listener_test.go).
func recordingEphemeralAPI(t *testing.T) (*slackapi.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.FormValue("text"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message_ts":"1234.999"}`))
	}))
	t.Cleanup(srv.Close)
	api := slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	return api, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
}

// TestOnInteraction_MetaagentShowDetails_CacheMiss_FallsBackToMemory verifies
// a Show Details click after channelsd restart (empty in-proc cache) renders
// the full details from the metaagent_audit request record authzd wrote at
// publish time.
func TestOnInteraction_MetaagentShowDetails_CacheMiss_FallsBackToMemory(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	require.NoError(t, metaagentaudit.RecordRequested(context.Background(), mem,
		memory.Scope{Kind: "session", ID: "ns/sess"}, metaagentaudit.Content{
			RequestID:   "req-003",
			Requester:   "U_BOB",
			RequestText: "widen scope to repo X",
			ColdStart:   true,
			CleanedTask: "summarize repo X",
			ComposerOutput: metaagentaudit.ComposerOutput{
				ApproverSummary: "adds read access to repo X",
			},
		}))

	api, ephemerals := recordingEphemeralAPI(t)
	l := &slackListener{
		deps:                  channelkinds.Deps{Memory: mem},
		api:                   api,
		metaagentApprovalRefs: newMetaagentApprovalRefCache(), // empty: cache miss
	}

	val := MetaagentApprovalButtonValue("req-003", "ns/sess", MetaagentDecisionShowDetails)
	cb := slackapi.InteractionCallback{
		Type:      slackapi.InteractionTypeBlockActions,
		User:      slackapi.User{ID: "U_ALICE"},
		Container: slackapi.Container{ChannelID: "CHAN", MessageTs: "1234.567"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{
			{ActionID: "metaagent_show_details_req-003", Value: val},
		}},
	}
	require.NoError(t, l.onInteraction(memory.WithSystemApproval(context.Background(), "test"), cb), "onInteraction")

	posted := ephemerals()
	require.Len(t, posted, 1, "ephemeral must post")
	text := posted[0]
	assert.Contains(t, text, "widen scope to repo X", "verbatim must render")
	assert.Contains(t, text, "adds read access to repo X", "approver summary must render")
	assert.Contains(t, text, "summarize repo X", "cold-start cleaned task must render")
	assert.NotContains(t, text, "not available", "degraded body must not render")
}

// TestOnInteraction_MetaagentUnknown_NoPublish verifies that an action_id
// with "metaagent_" prefix but an unrecognized decision is silently dropped.
func TestOnInteraction_MetaagentUnknown_NoPublish(t *testing.T) {
	// Build a value with an unknown decision.
	v := metaagentApprovalValue{
		V: "metaagent_approval", R: "req-999",
		S: "ns/sess", D: "unknown_decision",
	}
	valJSON, err := json.Marshal(v)
	require.NoError(t, err)

	published := false
	l := &slackListener{
		deps: channelkinds.Deps{
			NATSPublish: func(_ string, _ []byte) error {
				published = true
				return nil
			},
		},
	}
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_ALICE"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "metaagent_unknown_req-999", Value: string(valJSON)},
			},
		},
	}
	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")
	assert.False(t, published, "unknown decision must NOT publish to NATS")
}
