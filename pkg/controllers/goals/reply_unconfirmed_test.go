package goals

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
)

// noReceiverKindName is a group-hosting kind with no durable delivery
// receiver: the fake's transport and group answers, without its
// DeliveryReceiverProvider. It stands in for any kind whose receiver is not
// built yet.
const noReceiverKindName = "fake-noreceiver"

type noReceiverKind struct {
	channelkinds.Kind
	channelkinds.GroupGoalHost
}

func (noReceiverKind) Name() string { return noReceiverKindName }

func init() {
	registry.Register(noReceiverKind{Kind: fakekind.Kind{}, GroupGoalHost: fakekind.Kind{}})
}

func TestNoReceiverKindHasNoReceiver(t *testing.T) {
	k, ok := registry.Get(noReceiverKindName)
	require.True(t, ok)
	_, has := k.(channelkinds.DeliveryReceiverProvider)
	assert.False(t, has, "the test kind must not offer a delivery receiver")
	_, host := registry.GroupGoalHostFor(noReceiverKindName)
	assert.True(t, host, "the test kind must host group goals")
}

// TestGroupReplyWithoutReceiverIsUnconfirmed: a group run on a kind with no
// delivery receiver is not refused. Its reply is recorded once, directly as
// unconfirmed (handed to the ordinary outbound relay, never a receipt), and
// the dispatcher neither posts it itself nor retries it.
func TestGroupReplyWithoutReceiverIsUnconfirmed(t *testing.T) {
	h := newGroupReplyHarnessOnKind(t, noReceiverKindName)
	ctx := context.Background()
	handed, err := h.d.PrepareGoalReply(ctx, h.sess, h.payload, nil)
	require.NoError(t, err, "a group goal on a kind without a receiver can reply")
	require.NotNil(t, handed.Reply)
	assert.Equal(t, domain.ReplyUnconfirmed, handed.Reply.State)
	assert.Nil(t, handed.Reply.Receipt, "an unconfirmed reply never carries a receipt")
	pin, err := h.g.Execution.Terms.Group.RouteDigest()
	require.NoError(t, err)
	assert.Equal(t, groupRunOwner, handed.Reply.Intent.Destination.Recipient)
	assert.Equal(t, pin, handed.Reply.Intent.Destination.BindingDigest)

	*h.f.clock = h.f.clock.Add(time.Second)
	again, err := h.d.PrepareGoalReply(ctx, h.sess, h.payload, nil)
	require.NoError(t, err, "the same call repeated (a lost response, a resumed runner) gets its hand-off back")
	assert.Equal(t, handed.Reply, again.Reply, "the repeat records nothing new")

	other := channelevents.OutboundUserMessagePayload{Text: "A second message."}
	other.Delivery, err = channelevents.NewDeliveryOperation(string(h.sess.UID), "another-call", other)
	require.NoError(t, err)
	_, err = h.d.PrepareGoalReply(ctx, h.sess, other, nil)
	assert.ErrorIs(t, err, domain.ErrReplyHandedOff, "a different reply for the run is a second post")
	assert.ErrorIs(t, err, domain.ErrConflict)

	require.NoError(t, h.tick(t), "an unconfirmed reply is terminal: nothing to reconcile")
	require.NoError(t, h.tick(t))
	o := h.occurrence(t)
	assert.Equal(t, domain.ReplyUnconfirmed, o.Reply.State, "no retry moves it")
	assert.Nil(t, o.Reply.Receipt)
	assert.Zero(t, h.faults.posts, "the dispatcher posts nothing itself; the relay posts the runner's one envelope")
	assert.Empty(t, h.stored(t))

	reported, err := h.d.RecordGoalResult(ctx, h.sess, domain.RunProposal{RequestID: "result", Status: "reported_success", Summary: "Posted the summary", Evidence: []string{"thread"}})
	require.NoError(t, err, "the run still reports its outcome")
	require.NotNil(t, reported.Proposal)
	assert.Equal(t, domain.ReplyUnconfirmed, reported.Reply.State)

	page, err := h.f.svc.Runs(ctx, h.f.alice, h.g.ID, domain.ListRequest{})
	require.NoError(t, err)
	require.Len(t, page.Runs, 1)
	require.NotNil(t, page.Runs[0].Reply)
	assert.Equal(t, domain.ReplyUnconfirmed, page.Runs[0].Reply.State, "list_goal_runs shows the reply as unconfirmed, not accepted")
	assert.Nil(t, page.Runs[0].Reply.Receipt)
}

// TestGroupReplyPublishFailed: when the runner could not publish a handed-off
// reply, the record moves to publish_failed (terminal, shown by
// list_goal_runs), the operator logs it with the goal and occurrence, and only
// the handed-off operation can be marked.
func TestGroupReplyPublishFailed(t *testing.T) {
	h := newGroupReplyHarnessOnKind(t, noReceiverKindName)
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{}))
	_, err := h.d.PrepareGoalReply(ctx, h.sess, h.payload, nil)
	require.NoError(t, err)

	other := h.payload
	other.Delivery = &channelevents.DeliveryOperation{ID: "another-operation", SessionUID: h.payload.Delivery.SessionUID}
	_, err = h.d.RecordGoalReplyPublishFailed(ctx, h.sess, other)
	assert.ErrorIs(t, err, domain.ErrConflict, "only the handed-off operation can be marked failed")

	failed, err := h.d.RecordGoalReplyPublishFailed(ctx, h.sess, h.payload)
	require.NoError(t, err)
	assert.Equal(t, domain.ReplyPublishFailed, failed.Reply.State)
	assert.Nil(t, failed.Reply.Receipt)
	again, err := h.d.RecordGoalReplyPublishFailed(ctx, h.sess, h.payload)
	require.NoError(t, err, "marking it again is a no-op")
	assert.Equal(t, failed.Reply, again.Reply)
	logged := strings.Join(lines, "\n")
	for _, want := range []string{h.payload.Delivery.ID, h.o.ID, h.o.GoalID, h.sess.Name} {
		assert.Contains(t, logged, want)
	}

	retried, err := h.d.PrepareGoalReply(ctx, h.sess, h.payload, nil)
	require.NoError(t, err)
	assert.Equal(t, domain.ReplyPublishFailed, retried.Reply.State, "a resumed call sees the failure and does not publish again")

	require.NoError(t, h.tick(t))
	page, err := h.f.svc.Runs(ctx, h.f.alice, h.g.ID, domain.ListRequest{})
	require.NoError(t, err)
	require.Len(t, page.Runs, 1)
	assert.Equal(t, domain.ReplyPublishFailed, page.Runs[0].Reply.State, "list_goal_runs shows publish_failed, never accepted")
	assert.Zero(t, h.faults.posts)
}

// privateRunOnKind is a started private goal run whose binding is on kind.
func privateRunOnKind(t *testing.T, kind string) (*Dispatcher, *v1.AgentSession, channelevents.OutboundUserMessagePayload) {
	t.Helper()
	ctx := context.Background()
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "reply.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.Migrate(ctx))
	store := goalsqlite.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	auth := &runAuthority{}
	svc := &domain.Service{Store: store, Auth: auth, ExecutionAuth: auth, Now: func() time.Time { return now }}
	a := domain.Actor{Domain: domain.Domain{Namespace: "team", Owner: "owner", Class: "assistant", ClassUID: "class-uid"}, Session: "team/source", SessionUID: "source-uid", Proof: "unit-test-human"}
	g, err := svc.Create(ctx, a, domain.CreateRequest{RequestID: "create", Title: "Stretch", Outcome: "Private reminder"})
	require.NoError(t, err)
	g, err = svc.Update(ctx, a, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "activate", Action: "activate"})
	require.NoError(t, err)
	g, err = svc.RequestExecution(ctx, a, domain.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "request", Terms: domain.ExecutionTerms{ClassDigest: "class", DueAt: now, ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{DurationSeconds: 300, Turns: 10, Tokens: 10000, ApprovalSeconds: 90}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"receipt"}, Destination: domain.PrivateDestination{Channel: "inbox", ChannelUID: "channel-uid", BindingDigest: "binding", Recipient: a.Domain.Owner}}})
	require.NoError(t, err)
	g, err = svc.DecideExecution(ctx, a.Domain, g.ID, domain.ExecutionDecision{RequestID: "unit-decision", Digest: g.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: "unit-only-witness"})
	require.NoError(t, err)
	o, err := store.Schedule(ctx, g)
	require.NoError(t, err)
	binding := &v1.ChannelBinding{Kind: kind, Name: "inbox", Key: kind + ":inbox"}
	source := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "source", UID: "source-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant", InputChannel: binding}}
	root := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: o.SessionName, UID: "root-uid", CreationTimestamp: metav1.NewTime(now), Annotations: map[string]string{v1.AnnotationStartedByCanonicalID: "user:owner"}}, Spec: v1.AgentSessionSpec{Class: "assistant", GoalExecution: ref(o), InputChannel: binding, Budget: &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 10000, MaxDuration: metav1.Duration{Duration: 300 * time.Second}, SessionExpiration: metav1.Duration{Duration: 300 * time.Second}}}, Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning}}
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, root).Build()
	o, err = store.Claim(ctx, domain.ClaimRequest{ID: o.ID, Worker: "setup", Now: now, Lease: time.Second, OwnerLimit: 1, ClassLimit: 1})
	require.NoError(t, err)
	_, err = store.Attach(ctx, o, string(root.UID), now)
	require.NoError(t, err)
	d := &Dispatcher{Service: svc, Store: store, Client: k8s, Reader: k8s, Worker: "first", Now: func() time.Time { return now }, DeliveryMemory: memory.NewLocal(meminmem.NewBackend())}
	payload := channelevents.OutboundUserMessagePayload{Text: "Stand up and stretch!"}
	payload.Delivery, err = channelevents.NewDeliveryOperation(string(root.UID), "approved-reply", payload)
	require.NoError(t, err)
	return d, root, payload
}

// TestPrivateReplyStillNeedsAReceiver: the unconfirmed path is for group
// goals only. A private reply on a kind without a receiver is refused, and a
// private reply on a kind with one is prepared for the confirmed path.
func TestPrivateReplyStillNeedsAReceiver(t *testing.T) {
	cases := []struct {
		name string
		kind string
		want func(t *testing.T, o domain.Occurrence, err error)
	}{
		{name: "no receiver: refused", kind: noReceiverKindName, want: func(t *testing.T, _ domain.Occurrence, err error) {
			assert.ErrorIs(t, err, domain.ErrDenied)
		}},
		{name: "browser receiver: prepared", kind: "browser", want: func(t *testing.T, o domain.Occurrence, err error) {
			require.NoError(t, err)
			assert.Equal(t, domain.ReplyPrepared, o.Reply.State)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, root, payload := privateRunOnKind(t, tc.kind)
			o, err := d.PrepareGoalReply(context.Background(), root, payload, nil)
			tc.want(t, o, err)
		})
	}
}
