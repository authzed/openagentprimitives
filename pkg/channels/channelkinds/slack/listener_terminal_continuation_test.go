package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// fixedInbound is a stub InboundPipeline that always returns the same decision.
type fixedInbound struct{ dec channelkinds.InboundDecision }

func (f fixedInbound) Deliver(_ context.Context, _ channelkinds.InboundEvent) (channelkinds.InboundDecision, error) {
	return f.dec, nil
}

// recordingSlackAPI is an httptest server that records which Slack Web API
// endpoints were hit, so a listener test can assert that a terminal-continuation
// inbound posts a visible message (chat.postMessage) but NEVER sets a
// "starting…" status (assistant.threads.setStatus) on a session with no runner.
func recordingSlackAPI(t *testing.T) (*slackapi.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// A minimal ok response covers chat.postMessage + setStatus.
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1.0","channel":"C1"}`))
	}))
	t.Cleanup(srv.Close)
	api := slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	return api, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), hits...)
		return out
	}
}

func hit(paths []string, suffix string) bool {
	for _, p := range paths {
		if len(p) >= len(suffix) && p[len(p)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

// TestHandleChannelMessage_TerminalContinuationPostsNoticeNotStatus verifies the
// listener render for the two terminal-continuation outcomes: it posts the
// visible Notice in-thread and does NOT call assistant.threads.setStatus (which
// would strand the thread on a "starting…" status with no runner behind it).
func TestHandleChannelMessage_TerminalContinuationPostsNoticeNotStatus(t *testing.T) {
	cases := []struct {
		name    string
		outcome channelkinds.Outcome
		notice  *notice.Notice
	}{
		{"Refused: loud notice, no setStatus", channelkinds.OutcomeRefused,
			notice.New(categories.SessionEnded, notice.Args{
				Lead:     "This session has ended and can't continue",
				NextStep: "Start a new thread to begin again.",
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
			})},
		{"ForkPending: fork ack, no setStatus", channelkinds.OutcomeForkPending,
			notice.New(categories.SessionContinuedInherited, notice.Args{
				Lead:     "Continuing in a new session",
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
			})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
			channel := &spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Name: "slack-ch", Namespace: "default"},
				Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName},
			}
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(channel).Build()

			api, hits := recordingSlackAPI(t)
			l := &slackListener{
				deps: channelkinds.Deps{
					Channel:   channel,
					K8sClient: cli,
					Inbound:   fixedInbound{dec: channelkinds.InboundDecision{Outcome: tc.outcome, Notice: tc.notice}},
				},
				api:              api,
				seenEvts:         newEventIDCache(64),
				threads:          newThreadIndex(),
				assistantThreads: map[string]string{},
				botUserID:        "UBOT",
				idents:           NewIdentityCache(8),
				installedTeamID:  "T1",
			}
			// Prime the identity cache so resolveIdentity does not call users.info.
			l.idents.Put(userInfo{UserID: "U1", Email: "u1@example.com", TeamID: "T1"})

			l.handleChannelMessage(context.Background(), "U1", "C1", "9.9", "10.0", "continue please", nil, true, originHuman)

			paths := hits()
			assert.True(t, hit(paths, "/chat.postMessage"), "the visible notice must be posted in-thread; hits=%v", paths)
			assert.False(t, hit(paths, "/assistant.threads.setStatus"),
				"a terminal continuation must NOT set a starting status (no runner); hits=%v", paths)
		})
	}
}
