// pkg/channels/channelkinds/slack/fakeslack_listener_dispatch_test.go
//
// End-to-end proof that fakeslack.InjectDM / InjectMention produce events the
// REAL listener dispatch path (l.handle -> l.handleEventsAPI -> handleDM /
// handleChannelMessage) actually consumes — not just that they type-assert
// correctly in isolation (see fakeslack/inbound_test.go for that narrower
// proof). A mismatched event shape would be silently dropped somewhere in
// this chain; these tests would fail if that happened.
package slack

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
)

// recordingInbound is a stub InboundPipeline that records the InboundEvent it
// was handed and returns a fixed decision, so a test can assert exactly what
// the listener's dispatch path delivered.
type recordingInbound struct {
	dec channelkinds.InboundDecision
	got *channelkinds.InboundEvent
}

func (r recordingInbound) Deliver(_ context.Context, ev channelkinds.InboundEvent) (channelkinds.InboundDecision, error) {
	*r.got = ev
	return r.dec, nil
}

func newTestChannel(t *testing.T) (*spiceboxv1alpha1.Channel, *fake.ClientBuilder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spiceboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	channel := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-ch", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: KindName},
	}
	return channel, fake.NewClientBuilder().WithScheme(scheme).WithObjects(channel)
}

// TestFakeslack_InjectDM_DispatchesThroughRealListenerToHandleDM drives the
// actual production dispatch chain: a fakeslack.SocketSource-delivered event
// goes through l.handle (type switch on evt.Type/evt.Data) and
// l.handleEventsAPI (type switch on api.InnerEvent.Data), landing in
// handleDM, which calls deps.Inbound.Deliver. If InjectDM's event shape
// didn't match what those switches expect, recordingInbound would never see
// a Deliver call and this test would fail.
func TestFakeslack_InjectDM_DispatchesThroughRealListenerToHandleDM(t *testing.T) {
	channel, builder := newTestChannel(t)
	cli := builder.Build()

	fc := fakeslack.New()
	var got channelkinds.InboundEvent
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel:   channel,
			K8sClient: cli,
			Inbound:   recordingInbound{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}, got: &got},
		},
		api:              fc,
		src:              fakeslack.NewSocketSource(),
		seenEvts:         newEventIDCache(64),
		threads:          newThreadIndex(),
		assistantThreads: map[string]string{},
		idents:           NewIdentityCache(8),
		botUserID:        fakeslack.BotUserID,
	}

	ts, ev := fc.InjectDM("U_ALICE", "D01", "hello agent")

	l.handle(context.Background(), ev)

	if got.MessageText != "hello agent" {
		t.Fatalf("handleDM never received the injected DM: got %+v", got)
	}
	if got.ChannelKey != "dm:U_ALICE" {
		t.Errorf("ChannelKey = %q, want dm:U_ALICE", got.ChannelKey)
	}
	if got.External["channel_id"] != "D01" {
		t.Errorf("External[channel_id] = %q, want D01", got.External["channel_id"])
	}
	if got.External["message_ts"] != ts {
		t.Errorf("External[message_ts] = %q, want %q", got.External["message_ts"], ts)
	}

	// The user's message was also recorded as a top-level fakeslack entry, so
	// a subsequent reply threads under it coherently.
	top := fc.TopLevel("D01")
	if len(top) != 1 || top[0].TS != ts {
		t.Fatalf("fakeslack top-level messages = %+v, want exactly [ts=%s]", top, ts)
	}
}

// TestFakeslack_InjectMention_DispatchesThroughRealListenerToHandleChannelMessage
// mirrors the DM proof for the app_mention path.
func TestFakeslack_InjectMention_DispatchesThroughRealListenerToHandleChannelMessage(t *testing.T) {
	channel, builder := newTestChannel(t)
	cli := builder.Build()

	fc := fakeslack.New()
	var got channelkinds.InboundEvent
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel:   channel,
			K8sClient: cli,
			Inbound:   recordingInbound{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}, got: &got},
		},
		api:              fc,
		src:              fakeslack.NewSocketSource(),
		seenEvts:         newEventIDCache(64),
		threads:          newThreadIndex(),
		assistantThreads: map[string]string{},
		idents:           NewIdentityCache(8),
		botUserID:        fakeslack.BotUserID,
	}

	mentionText := "<@" + fakeslack.BotUserID + "> help me"
	_, ev := fc.InjectMention("U_BOB", "C01", mentionText)

	l.handle(context.Background(), ev)

	if got.MessageText != mentionText {
		t.Fatalf("handleChannelMessage never received the injected mention: got %+v", got)
	}
	if got.External["channel_id"] != "C01" {
		t.Errorf("External[channel_id] = %q, want C01", got.External["channel_id"])
	}
}
