package runner

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// webhookUnavailable is the error the API server returns for a write that
// matched a failurePolicy: Fail ValidatingWebhook whose backing Service has no
// endpoints — the exact shape produced while the operator that SERVES the
// agentsessionidentity webhook is being replaced (replicas: 1, Recreate).
// It is a 500/InternalError, NOT a 403 denial.
func webhookUnavailable() error {
	return apierrors.NewInternalError(fmt.Errorf(
		`failed calling webhook "agentsessionidentity.agentprimitives.authzed.com": ` +
			`failed to call webhook: Post "https://spicebox-webhook.agentprimitives-system.svc:443/validate-agentsession-identity?timeout=10s": ` +
			`dial tcp 10.96.0.1:443: connect: connection refused`))
}

// webhookDenial is the OTHER answer the same webhook can give: a verdict. The
// API server renders an admission denial as 403 Forbidden.
func webhookDenial() error {
	return apierrors.NewForbidden(
		spiceboxv1alpha1.Resource("agentsessions"),
		"s1",
		fmt.Errorf("AgentSession.status.pendingRestart is not writable by a session runner"))
}

// outageLoop builds a fake client whose AgentSession writes are refused with
// webhookUnavailable for the first `failures` attempts of each kind, then
// admitted — the operator restarting and coming back. The counters let a test
// prove the write was actually retried rather than skipped.
func outageLoop(t *testing.T, sess *spiceboxv1alpha1.AgentSession, failures int32) (client.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	var statusPatches, metaPatches atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if statusPatches.Add(1) <= failures {
					return webhookUnavailable()
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if metaPatches.Add(1) <= failures {
					return webhookUnavailable()
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	return c, &statusPatches, &metaPatches
}

// TestIdleSurvivesOperatorRestartWebhookOutage is the operator-restart cascade.
//
// `oap install` — the documented upgrade path — replaces the single operator pod
// with strategy: Recreate, so the agentsessionidentity webhook it serves has
// zero endpoints for the whole teardown + boot window. The webhook is
// failurePolicy: Fail and matches exactly the `-runner-sa` principals, so the
// class of write it blocks is the runner's own lifecycle reporting. Idle-sleep
// is the resting state of every channel-attached agent, so a turn boundary
// landing in that window is the common case, not the corner.
//
// If WriteIdle surfaces that refusal, it leaves idleWithWakeRecheck, leaves
// Loop.Run, and exits the runner at internal/cmd/runner/main.go. RestartPolicy: OnFailure
// restarts it into the same closed door; on the second crash the returned
// operator's fastFail marks the session Failed/RunnerCrash and relays "failed
// calling webhook …" into the user's channel — a live session terminally killed
// by a routine operator restart.
//
// The write is valid and IS admitted the moment the webhook is back, so the
// runner must wait the outage out rather than die on it. The wake half matters
// as much as the phase: a message that landed as this runner idled strands until
// the user speaks again unless the wake annotation is stamped too.
func TestIdleSurvivesOperatorRestartWebhookOutage(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := memory.NamespacedName{Namespace: "default", Name: "s1"}
	store := LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key)
	require.NoError(t, store.Append(ctx, drainTextTurn(0, "user", "how many open orders")), "seed transcript")
	require.NoError(t, store.Append(ctx, drainTextTurn(1, "assistant", "seven")), "seed transcript")
	// Landed while this runner was idling: only a stamped wake picks it up.
	require.NoError(t, store.Append(ctx, drainTextTurn(2, "inbox", "and how many shipped")), "seed held inbound")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	c, statusPatches, metaPatches := outageLoop(t, sess, 1)

	l := &Loop{
		Memory:     store,
		Status:     NewStatusPatcher(c, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}),
		SessionKey: key,
	}
	require.NoError(t, l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAwaitingUserMsg),
		"an operator restart must not fail the write that parks a live session Idle")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sess), &got), "Get after idle")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, got.Status.Phase,
		"the session must reach Idle once the webhook is back")
	assert.True(t, spiceboxv1alpha1.WakePending(&got),
		"the held inbound must still leave a pending wake; a refused annotation patch strands it")
	assert.Greater(t, statusPatches.Load(), int32(1), "the status write must have been re-attempted, not abandoned")
	assert.Greater(t, metaPatches.Load(), int32(1), "the wake annotation write must have been re-attempted too")
}

// alwaysFails builds a fake client whose status writes always return err, and
// a counter of how many times the write was attempted.
func alwaysFails(t *testing.T, err error) (client.Client, *atomic.Int32) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	var attempts atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				attempts.Add(1)
				return err
			},
		}).
		Build()
	return c, &attempts
}

// TestStatusPatcherOutageHandling pins the two boundaries of the wait, which
// are what keep it from being a hang or a blunt instrument.
//
//   - The budget is FINITE. A runner blocked forever on a status write is its
//     own failure mode: the session shows no progress and nothing times it out.
//     Exiting is survivable precisely because the operator no longer
//     terminalizes a session for a crash it did not witness.
//   - A DENIAL is not an outage. The API server renders an admission "no" as
//     403 Forbidden; spinning the whole budget on it would blunt the
//     identity-pinning gate into a slow failure and delay the runner's only
//     diagnostic.
func TestStatusPatcherOutageHandling(t *testing.T) {
	// Fast schedule: same shape as production (initial, doubling, cap, budget),
	// three orders of magnitude smaller so the bound is observable in a unit test.
	fast := outageWait{initial: time.Millisecond, cap: 4 * time.Millisecond, budget: 40 * time.Millisecond}

	cases := []struct {
		name         string
		err          error
		wantErrPart  string
		wantAttempts func(t *testing.T, got int32)
	}{
		{
			name:        "outage outlasts the budget: the write is retried, then the error is surfaced",
			err:         webhookUnavailable(),
			wantErrPart: "failed calling webhook",
			wantAttempts: func(t *testing.T, got int32) {
				assert.Greater(t, got, int32(1), "an outage must be waited out, not abandoned on the first refusal")
			},
		},
		{
			name:        "admission denial: surfaced at once, never retried",
			err:         webhookDenial(),
			wantErrPart: "not writable by a session runner",
			wantAttempts: func(t *testing.T, got int32) {
				assert.Equal(t, int32(1), got, "a 403 verdict must not be retried; it will never be admitted")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, attempts := alwaysFails(t, tc.err)
			sp := NewStatusPatcher(c, client.ObjectKey{Namespace: "default", Name: "s1"})
			sp.outage = fast

			start := time.Now()
			err := sp.WriteIdle(context.Background(), spiceboxv1alpha1.ReasonAgentSessionAwaitingUserMsg)
			elapsed := time.Since(start)

			require.Error(t, err, "an unrecoverable write must surface, not be swallowed")
			assert.ErrorContains(t, err, tc.wantErrPart, "the surfaced error must name the real cause")
			assert.Less(t, elapsed, 30*fast.budget, "the wait must be bounded by the budget, not open-ended")
			tc.wantAttempts(t, attempts.Load())
		})
	}
}

// TestStatusPatcherRetriesConflictsWithoutWaiting pins the OTHER half of the
// two-policy split: a lost optimistic lock means somebody else's write already
// landed, so it is re-read and re-applied immediately. Folding conflicts into
// the outage backoff would put a sleep on the contended-but-healthy path that
// the operator's and channelsd's concurrent status writes make routine.
func TestStatusPatcherRetriesConflictsWithoutWaiting(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	var attempts atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if attempts.Add(1) <= 2 {
					return apierrors.NewConflict(spiceboxv1alpha1.Resource("agentsessions"), "s1", fmt.Errorf("the object has been modified"))
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	sp := NewStatusPatcher(c, client.ObjectKeyFromObject(sess))
	start := time.Now()
	require.NoError(t, sp.WriteIdle(context.Background(), spiceboxv1alpha1.ReasonAgentSessionAwaitingUserMsg),
		"a contended write must still land")
	assert.Equal(t, int32(3), attempts.Load(), "each conflict must re-read and re-apply")
	assert.Less(t, time.Since(start), defaultOutageWait.initial,
		"conflicts must retry immediately; the outage backoff must not leak onto the contended path")
}
