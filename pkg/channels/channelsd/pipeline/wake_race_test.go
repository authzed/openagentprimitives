package pipeline

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestDeliverAnnotatesWhenRunnerIdlesAfterTheOpeningList is the regression
// guard for the stranded-message race.
//
// Deliver classifies the session from a List taken at its top, then does real
// work (the interact Check, the memory append, the msg-ref index write) before
// deciding whether the operator must respawn an exited runner. A runner that
// calls agent_work_complete inside that window leaves the snapshot reading
// Running while the live object already reads Idle. Gating the annotation on
// the SNAPSHOT writes nothing, falls back to a NATS wakeup the exited runner
// never receives, and strands the user's message in memory as an undrained
// inbox turn with no signal at all.
//
// The interceptor below reproduces that interleaving deterministically: the
// stored session flips Running→Idle immediately after Deliver's opening List
// returns, so the snapshot and the live object genuinely disagree.
func TestDeliverAnnotatesWhenRunnerIdlesAfterTheOpeningList(t *testing.T) {
	const key = "thread:C1:1"
	ch := newChannel("c1")
	sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseRunning)

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "add spicebox scheme")
	require.NoError(t, corev1.AddToScheme(scheme), "add core scheme")

	var once sync.Once
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ch, sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				// The caller now holds a Running snapshot. Land the runner's
				// WriteIdle before it acts on it.
				once.Do(func() {
					var cur spiceboxv1alpha1.AgentSession
					if err := c.Get(ctx, client.ObjectKeyFromObject(sess), &cur); err != nil {
						return
					}
					cur.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
					_ = c.Status().Update(ctx, &cur)
				})
				return nil
			},
		}).
		Build()

	p, _, mem, _ := newPipelineOn(t, cli)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  key,
		MessageText: "contacts on Acme",
	})
	require.NoError(t, err, "Deliver")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "message accepted onto the existing session")
	assert.Len(t, mem.appends, 1, "inbound appended to memory")
	assert.NotEmpty(t, wakeAnnotationOf(t, cli, sess.Name),
		"runner idled after the opening List: the annotation must be decided from the FRESH phase, or the turn strands forever")
}

// TestDeliverSkipsWakeForALiveRunner is the other side of the same decision:
// removing the snapshot gate must not start respawning sessions whose runner is
// still live. A respawn there re-runs a message the live runner already
// consumed via the NATS wakeup, producing a duplicate turn — a worse,
// user-visible bug than the one being fixed. The fresh re-read inside
// annotateWake is what keeps this case a no-op.
func TestDeliverSkipsWakeForALiveRunner(t *testing.T) {
	const key = "thread:C1:1"
	ch := newChannel("c1")
	sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseRunning)
	p, _, mem, _, cli := newPipeline(t, ch, sess)

	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  key,
		MessageText: "and Beta too",
	})
	require.NoError(t, err, "Deliver")

	assert.Len(t, mem.appends, 1, "inbound appended to memory")
	assert.Empty(t, wakeAnnotationOf(t, cli, sess.Name),
		"session is genuinely Running: no respawn annotation, the live runner reads the turn via the NATS wakeup")
}
