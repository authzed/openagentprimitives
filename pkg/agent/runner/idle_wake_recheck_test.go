package runner

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// TestIdleWithWakeRecheck is the runner half of the stranded-message fix.
//
// The interleaving under test is the one channelsd cannot catch: an inbound
// appended while the runner is committing phase=Idle. channelsd's own fresh
// re-read can still observe Running there and leave delivery to a NATS wakeup
// this pod never receives — so the runner must re-read the inbox AFTER WriteIdle
// and ask for its own respawn.
//
// The Status-patch interceptor makes that ordering real rather than assumed: the
// inbox turn is appended at the exact moment WriteIdle commits, i.e. strictly
// after the loop's last drain.
func TestIdleWithWakeRecheck(t *testing.T) {
	cases := []struct {
		name         string
		appendOnIdle bool
		wantWake     bool
	}{
		{
			name:         "message lands as the runner idles: respawn requested, turn is not stranded",
			appendOnIdle: true,
			wantWake:     true,
		},
		{
			name:         "nothing queued: session parks Idle with no respawn request",
			appendOnIdle: false,
			wantWake:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			key := memory.NamespacedName{Namespace: "default", Name: "s1"}
			store := LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key)
			require.NoError(t, store.Append(ctx, drainTextTurn(0, "user", "companies created recently")), "seed transcript")
			require.NoError(t, store.Append(ctx, drainTextTurn(1, "assistant", "found 3")), "seed transcript")

			scheme := runtime.NewScheme()
			require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
			}
			var once sync.Once
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(sess).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						if err := cl.SubResource(sub).Patch(ctx, obj, patch, opts...); err != nil {
							return err
						}
						if tc.appendOnIdle {
							// channelsd's inbound lands exactly here: after the
							// loop's last drain, as phase=Idle commits.
							once.Do(func() {
								_ = store.Append(memory.WithSystemApproval(context.Background(), "test"),
									drainTextTurn(2, "inbox", "contacts on Acme"))
							})
						}
						return nil
					},
				}).
				Build()

			l := &Loop{
				Memory:     store,
				Status:     NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
				SessionKey: key,
			}
			require.NoError(t, l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete),
				"idleWithWakeRecheck must not fail the session")

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get after idle")
			assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "session parks Idle either way")
			assert.Equal(t, tc.wantWake, spiceboxv1alpha1.WakePending(&got),
				"a held inbox turn at Idle must leave a pending wake — that pair is the no-strand invariant")
		})
	}
}

// TestIdleWithWakeRecheckSurvivesMemoryFailure pins the failure posture: the
// session is already correctly Idle when the re-check runs, so a memory error
// must not be promoted into a failed session. The held turn drains on the
// user's next message; the operator sees the miss in the runner log.
func TestIdleWithWakeRecheckSurvivesMemoryFailure(t *testing.T) {
	ctx := context.Background()
	key := memory.NamespacedName{Namespace: "default", Name: "s1"}
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	// No system approval on ctx → the memory facade refuses the read.
	l := &Loop{
		Memory:     LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key),
		Status:     NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		SessionKey: key,
	}
	_, herr := l.heldInbox(ctx)
	require.Error(t, herr, "fixture must genuinely break the inbox read, or this test proves nothing")

	require.NoError(t, l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete),
		"a failed re-check must not fail the session")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get after idle")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase, "WriteIdle still took effect")
}
