package goals

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// runAuthority allows everything except AuthorizeDispatch's err and reads of
// the resource IDs in revoked.
type runAuthority struct {
	err     error
	revoked map[string]bool
}

func (*runAuthority) Authorize(context.Context, domain.Actor, bool) error            { return nil }
func (*runAuthority) Sources(context.Context, domain.Actor) ([]domain.Source, error) { return nil, nil }
func (a *runAuthority) ReadGoal(_ context.Context, _ domain.Actor, g domain.Goal) error {
	for _, src := range g.Sources {
		if a.revoked[src.ResourceID] {
			return domain.ErrDenied
		}
	}
	return nil
}
func (*runAuthority) Validate(context.Context, domain.Goal, domain.ExecutionTerms) error { return nil }
func (*runAuthority) VerifyDecision(context.Context, domain.Goal, domain.ExecutionDecision) error {
	return nil
}
func (a *runAuthority) AuthorizeDispatch(context.Context, domain.Goal) error { return a.err }

// runFixture is one claimed and attached goal occurrence with its Idle
// session and runner pod in a fake cluster.
type runFixture struct {
	store *sqlstore.Store
	svc   *domain.Service
	auth  *runAuthority
	actor domain.Actor
	goal  domain.Goal
	occ   domain.Occurrence
	sess  *v1.AgentSession
	pod   *corev1.Pod
	k8s   client.Client
	clock *time.Time
}

// newRunFixture builds the run at *clock. mutate adjusts the session and the
// runner pod before they are created; extra objects are created alongside.
func newRunFixture(t *testing.T, clock *time.Time, mutate func(*v1.AgentSession, *corev1.Pod), extra ...client.Object) *runFixture {
	t.Helper()
	ctx := context.Background()
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "runs.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := goalsqlite.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	now := *clock
	auth := &runAuthority{}
	svc := &domain.Service{Store: store, Auth: auth, ExecutionAuth: auth, Now: func() time.Time { return *clock }}
	a := domain.Actor{Domain: domain.Domain{Namespace: "team", Owner: "owner", Class: "assistant", ClassUID: "class-uid"}, Session: "team/source", Proof: "test-human"}
	g, err := svc.Create(ctx, a, domain.CreateRequest{RequestID: "create", Title: "Stretch", Outcome: "Send a reminder"})
	require.NoError(t, err)
	g, err = svc.Update(ctx, a, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "activate", Action: "activate"})
	require.NoError(t, err)
	g, err = svc.RequestExecution(ctx, a, domain.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "request", Terms: domain.ExecutionTerms{ClassDigest: "class-digest", DueAt: now, ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{DurationSeconds: 300, Turns: 10, Tokens: 10000, ApprovalSeconds: 90}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"private reminder"}, Destination: domain.PrivateDestination{Channel: "inbox", ChannelUID: "inbox-uid", Recipient: a.Domain.Owner}}})
	require.NoError(t, err)
	g, err = svc.DecideExecution(ctx, a.Domain, g.ID, domain.ExecutionDecision{RequestID: "approval", Digest: g.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: "test-only-human-witness"})
	require.NoError(t, err)
	o, err := store.Schedule(ctx, g)
	require.NoError(t, err)
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: o.SessionName, UID: "session-uid", CreationTimestamp: metav1.NewTime(now)}, Spec: v1.AgentSessionSpec{Class: "assistant", GoalExecution: ref(o)}, Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseIdle, EstimatedCost: &v1.EstimatedSessionCost{AmountMicroUSD: 3000, Currency: "USD", PricingKnown: true, AsOf: metav1.NewTime(now), ByModel: []v1.ModelCostBucket{{Model: "served-model", AmountMicroUSD: 2000, PricingKnown: true}}, ByTool: []v1.ToolCostBucket{{Tool: "inner-tool", AmountMicroUSD: 1000, PricingKnown: true}}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: agentsession.RunnerPodName(sess), OwnerReferences: []metav1.OwnerReference{{UID: sess.UID}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if mutate != nil {
		mutate(sess, pod)
	}
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	objects := append([]client.Object{sess}, extra...)
	if pod.Name != "" {
		objects = append(objects, pod)
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	o, err = store.Claim(ctx, domain.ClaimRequest{ID: o.ID, Worker: "setup", Now: now, Lease: time.Second, OwnerLimit: 1, ClassLimit: 4})
	require.NoError(t, err)
	o, err = store.Attach(ctx, o, string(sess.UID), now)
	require.NoError(t, err)
	return &runFixture{store: store, svc: svc, auth: auth, actor: a, goal: g, occ: o, sess: sess, pod: pod, k8s: k8s, clock: clock}
}

// report records the agent's report_goal_result for o.
func (f *runFixture) report(t *testing.T, o domain.Occurrence) domain.Occurrence {
	t.Helper()
	o, err := f.store.ProposeResult(context.Background(), o, domain.RunProposal{RequestID: "result", Status: "reported_success", Summary: "Reminder sent", Evidence: []string{"delivery receipt"}}, *f.clock)
	require.NoError(t, err)
	return o
}

func (f *runFixture) dispatcher(worker string) *Dispatcher {
	return &Dispatcher{Service: f.svc, Store: f.store, Client: f.k8s, Reader: f.k8s, Worker: worker, Now: func() time.Time { return *f.clock }}
}

func TestDispatcherDurableStopRecovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		phase  corev1.PodPhase
		reason domain.RunReason
		state  domain.OccurrenceState
	}{
		{"accounting restarts", corev1.PodSucceeded, domain.RunSessionEnded, domain.OccurrenceFinished},
		{"idle session with finished runner", corev1.PodSucceeded, domain.RunSessionEnded, domain.OccurrenceFinished},
		{"failed runner", corev1.PodFailed, domain.RunInfrastructureFailed, domain.OccurrenceFailed},
		{"time limit", corev1.PodRunning, domain.RunDurationExpired, domain.OccurrenceFailed},
		{"cancel", corev1.PodRunning, domain.RunCancelled, domain.OccurrenceCancelled},
		{"pause", corev1.PodRunning, domain.RunPaused, domain.OccurrenceCancelled},
		{"revise", corev1.PodRunning, domain.RunSuperseded, domain.OccurrenceCancelled},
		{"consent expires", corev1.PodRunning, domain.RunConsentExpired, domain.OccurrenceCancelled},
		{"authority denial", corev1.PodRunning, domain.RunAuthorityDenied, domain.OccurrenceCancelled},
		{"missing session releases capacity once its pods are gone", corev1.PodRunning, domain.RunSessionMissing, domain.OccurrenceFailed},
		{"external delete of a running session is a cancellation", corev1.PodRunning, domain.RunCancelled, domain.OccurrenceCancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
			externalDelete := test.name == "external delete of a running session is a cancellation"
			f := newRunFixture(t, &now, func(sess *v1.AgentSession, pod *corev1.Pod) {
				pod.Status.Phase = test.phase
				if externalDelete {
					// Holds the CR in Terminating, as the session finalizer does.
					sess.Finalizers = []string{"test.oap/hold"}
				}
			})
			store, svc, auth, a, g, o, sess, pod, k8s := f.store, f.svc, f.auth, f.actor, f.goal, f.occ, f.sess, f.pod, f.k8s
			var err error
			if test.reason == domain.RunSessionEnded {
				o = f.report(t, o)
			}
			if test.name == "accounting restarts" {
				older := sess.Status.EstimatedCost.DeepCopy()
				older.AmountMicroUSD = 10000
				older.ByModel[0].AmountMicroUSD = 9000
				o, err = store.RecordCost(ctx, o, domain.RunCost{SessionUID: o.SessionUID, Estimate: older, Reason: "runner_active_or_interrupted"}, now)
				require.NoError(t, err)
			}
			now = now.Add(2 * time.Second)
			d := &Dispatcher{Service: svc, Store: store, Client: k8s, Reader: k8s, Worker: "first-controller", Now: func() time.Time { return now }}
			auth.err = errors.New("authorization service unavailable")
			require.ErrorContains(t, d.Tick(ctx), "authorization service unavailable")
			untouched, err := store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			require.Nil(t, untouched.Outcome)
			var existing v1.AgentSession
			require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(sess), &existing))
			auth.err = nil
			now = now.Add(6 * time.Second)
			switch test.reason {
			case domain.RunDurationExpired:
				now = now.Add(301 * time.Second)
			case domain.RunConsentExpired:
				now = now.Add(time.Hour)
			case domain.RunCancelled:
				if externalDelete {
					require.NoError(t, k8s.Delete(ctx, sess))
					break
				}
				fallthrough
			case domain.RunPaused, domain.RunSuperseded:
				action := "cancel"
				if test.reason == domain.RunPaused {
					action = "pause"
				}
				if test.reason == domain.RunSuperseded {
					action = "revise"
				}
				_, err = svc.Update(ctx, a, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "change", Action: action, Outcome: func() *string {
					if action == "revise" {
						text := "Revised reminder"
						return &text
					}
					return nil
				}()})
				require.NoError(t, err)
			case domain.RunAuthorityDenied:
				auth.err = domain.ErrDenied
			case domain.RunSessionMissing:
				require.NoError(t, k8s.Delete(ctx, sess))
			}
			require.NoError(t, d.Tick(ctx))
			observed, err := store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			require.NotNil(t, observed.Outcome)
			require.Equal(t, test.reason, observed.Outcome.Reason)
			require.Equal(t, "unknown", observed.Outcome.Effects)
			require.NotNil(t, observed.Cost)
			if test.reason == domain.RunSessionMissing {
				require.Nil(t, observed.Cost.Estimate)
				require.Equal(t, "session_missing", observed.Cost.Reason)
			} else if test.name == "accounting restarts" {
				require.Equal(t, int64(10000), observed.Cost.Estimate.AmountMicroUSD)
				require.False(t, observed.Cost.Final)
				require.Equal(t, "accounting_regressed", observed.Cost.Reason)
			} else {
				require.Equal(t, sess.Status.EstimatedCost.AmountMicroUSD, observed.Cost.Estimate.AmountMicroUSD)
				require.Equal(t, sess.Status.EstimatedCost.ByModel, observed.Cost.Estimate.ByModel)
				require.Equal(t, sess.Status.EstimatedCost.ByTool, observed.Cost.Estimate.ByTool)
				require.True(t, sess.Status.EstimatedCost.AsOf.Equal(&observed.Cost.Estimate.AsOf))
				require.Equal(t, test.phase == corev1.PodSucceeded, observed.Cost.Final, "every runner exited, so the estimate is complete")
			}
			// Restart between the durable observation and pod GC acknowledgement.
			d.Worker = "restarted-controller"
			now = now.Add(6 * time.Second)
			require.NoError(t, d.Tick(ctx))
			held, err := store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			if test.reason == domain.RunSessionEnded {
				require.Equal(t, domain.OccurrenceRetained, held.State, "terminal runner no longer reserves capacity")
				// A different goal for the same owner may launch while this
				// completed conversation is still available for inspection.
				other, err := svc.Create(ctx, a, domain.CreateRequest{RequestID: "create-next", Title: "Next", Outcome: "Report an update"})
				require.NoError(t, err)
				other, err = svc.Update(ctx, a, domain.Change{ID: other.ID, Revision: other.Revision, RequestID: "activate-next", Action: "activate"})
				require.NoError(t, err)
				terms := g.Execution.Terms
				terms.DueAt = now
				other, err = svc.RequestExecution(ctx, a, domain.ExecutionRequest{ID: other.ID, Revision: other.Revision, RequestID: "request-next", Terms: terms})
				require.NoError(t, err)
				other, err = svc.DecideExecution(ctx, a.Domain, other.ID, domain.ExecutionDecision{RequestID: "approve-next", Digest: other.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: "test-human"})
				require.NoError(t, err)
				next, err := store.Schedule(ctx, other)
				require.NoError(t, err)
				next, err = store.Claim(ctx, domain.ClaimRequest{ID: next.ID, Worker: "next-run", Now: now, Lease: time.Second, OwnerLimit: 1, ClassLimit: 1})
				require.NoError(t, err, "retained session must not block the next goal")
				require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(sess), &existing), "completed conversation remains inspectable")
				_, err = store.Finish(ctx, next, domain.OccurrenceCancelled, now)
				require.NoError(t, err)
			} else {
				require.Equal(t, domain.OccurrenceRunning, held.State, "nonterminal or uncertain pod still owns capacity")
			}
			require.NoError(t, k8s.Delete(ctx, pod))
			if externalDelete {
				require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(sess), &existing))
				existing.Finalizers = nil
				require.NoError(t, k8s.Update(ctx, &existing))
			}
			now = now.Add(301 * time.Second)
			require.NoError(t, d.Tick(ctx))
			// A normally finished conversation is retained until its original bound,
			// then cleanup still needs a fresh observation of the CR's disappearance.
			now = now.Add(6 * time.Second)
			require.NoError(t, d.Tick(ctx))
			finished, err := store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			require.Equal(t, test.state, finished.State)
			require.Equal(t, observed.Outcome, finished.Outcome)
			require.Equal(t, observed.Cost, finished.Cost, "session GC and controller takeover preserve accounting")
			current, err := store.Get(ctx, g.Domain, g.ID)
			require.NoError(t, err)
			require.NotEqual(t, domain.Completed, current.State)
			if test.reason != domain.RunCancelled && test.reason != domain.RunPaused && test.reason != domain.RunSuperseded {
				require.Equal(t, g, current)
			}
		})
	}
}

// TestDispatcherClassifiesExitedRunner pins how a run whose runner container
// has stopped is labeled, and that every such session is kept for inspection
// rather than deleted. Only a reported run that left the session Idle (or
// Succeeded) ended; a run parked on a human gets no verdict, so the human can
// still act on it.
func TestDispatcherClassifiesExitedRunner(t *testing.T) {
	runnerExitedBesideSidecar := func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: agentsession.RunnerContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: "mcp-sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}
	}
	podSucceeded := func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodSucceeded }
	for _, tc := range []struct {
		name     string
		phase    string
		slept    bool
		runner   func(*corev1.Pod) // nil: the runner pod is gone
		reported bool
		want     domain.RunReason // "" means no verdict yet
		verdict  agentsession.GoalRunVerdict
	}{
		{name: "reported, runner exited beside a running sidecar: ended, verdict succeeded", phase: v1.AgentSessionPhaseIdle, runner: runnerExitedBesideSidecar, reported: true, want: domain.RunSessionEnded, verdict: agentsession.GoalRunSucceeded},
		{name: "runner exited 0 then pod marked Failed on eviction: ended, not crashed", phase: v1.AgentSessionPhaseIdle, runner: func(pod *corev1.Pod) {
			runnerExitedBesideSidecar(pod)
			pod.Status.Phase = corev1.PodFailed
		}, reported: true, want: domain.RunSessionEnded, verdict: agentsession.GoalRunSucceeded},
		{name: "Idle without a report (refused delivery, await timeout): failed, verdict failed", phase: v1.AgentSessionPhaseIdle, runner: podSucceeded, want: domain.RunSessionFailed, verdict: agentsession.GoalRunFailed},
		{name: "archived to Succeeded without a report: failed, verdict failed", phase: v1.AgentSessionPhaseSucceeded, runner: podSucceeded, want: domain.RunSessionFailed, verdict: agentsession.GoalRunFailed},
		{name: "slept by the reaper after reporting: ended, verdict succeeded", phase: v1.AgentSessionPhaseIdle, slept: true, reported: true, want: domain.RunSessionEnded, verdict: agentsession.GoalRunSucceeded},
		{name: "parked in AwaitingRetry after a provider error: no verdict, Retry still works", phase: v1.AgentSessionPhaseAwaitingRetry, runner: podSucceeded, reported: true},
		{name: "parked in AwaitingCredentials: no verdict, the link card still works", phase: v1.AgentSessionPhaseAwaitingCredentials, runner: runnerExitedBesideSidecar},
		{name: "Held with an exited runner: no verdict while a human decides", phase: v1.AgentSessionPhaseHeld, runner: podSucceeded, reported: true},
		{name: "runner still running beside a sidecar: no verdict", phase: v1.AgentSessionPhaseIdle, runner: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: agentsession.RunnerContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}, reported: true},
		{name: "runner pod gone without sleep or a terminal phase: no verdict", phase: v1.AgentSessionPhaseIdle, reported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
			f := newRunFixture(t, &now, func(sess *v1.AgentSession, pod *corev1.Pod) {
				sess.Status.Phase = tc.phase
				if tc.slept {
					sess.Status.SleptAt = &metav1.Time{Time: now}
				}
				if tc.runner == nil {
					pod.Name = ""
					return
				}
				tc.runner(pod)
			})
			if tc.reported {
				f.occ = f.report(t, f.occ)
			}
			now = now.Add(2 * time.Second)
			require.NoError(t, f.dispatcher("controller").Tick(ctx))

			got, err := f.store.Occurrence(ctx, f.occ.ID)
			require.NoError(t, err)
			var existing v1.AgentSession
			assert.NoError(t, f.k8s.Get(ctx, client.ObjectKeyFromObject(f.sess), &existing), "the session is never deleted on this verdict")
			_, verdict, err := f.dispatcher("controller").GoalSessionConcluded(ctx, &existing)
			require.NoError(t, err)
			assert.Equal(t, tc.verdict, verdict)
			if tc.want == "" {
				assert.Nil(t, got.Outcome)
				assert.Equal(t, domain.OccurrenceRunning, got.State)
				return
			}
			require.NotNil(t, got.Outcome)
			assert.Equal(t, tc.want, got.Outcome.Reason)
			assert.Equal(t, domain.OccurrenceRetained, got.State, "an exited runner releases capacity and keeps the session")
		})
	}
}

// TestDispatcherNeverDeletesHeldSession pins that a SessionHold keeps a goal
// session as evidence past its bound without pinning the owner's capacity: the
// outcome is recorded, the run is retained (the hold reaped its runner), the
// session stays, and cleanup resumes once the hold is released.
func TestDispatcherNeverDeletesHeldSession(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
	hold := &v1.SessionHold{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "review"}}
	f := newRunFixture(t, &now, func(sess *v1.AgentSession, pod *corev1.Pod) {
		sess.Status.Phase = v1.AgentSessionPhaseHeld
		pod.Name = "" // a hold reaps the session's pods
		hold.Spec.SessionRef = v1.NamespacedRef{Namespace: sess.Namespace, Name: sess.Name}
	}, hold)
	d := f.dispatcher("controller")
	now = now.Add(301 * time.Second)
	require.NoError(t, d.Tick(ctx))

	got, err := f.store.Occurrence(ctx, f.occ.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Outcome)
	assert.Equal(t, domain.RunDurationExpired, got.Outcome.Reason)
	assert.Equal(t, domain.OccurrenceRetained, got.State, "a held run releases the owner's capacity")
	var existing v1.AgentSession
	require.NoError(t, f.k8s.Get(ctx, client.ObjectKeyFromObject(f.sess), &existing), "a held session is never deleted")

	require.NoError(t, f.k8s.Get(ctx, client.ObjectKeyFromObject(hold), hold))
	hold.Status.Phase = v1.SessionHoldPhaseReleased
	require.NoError(t, f.k8s.Update(ctx, hold))
	now = now.Add(6 * time.Second)
	require.NoError(t, d.Tick(ctx))
	err = f.k8s.Get(ctx, client.ObjectKeyFromObject(f.sess), &existing)
	assert.True(t, apierrors.IsNotFound(err), "cleanup resumes after release, got %v", err)
	now = now.Add(6 * time.Second)
	require.NoError(t, d.Tick(ctx))
	got, err = f.store.Occurrence(ctx, f.occ.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.OccurrenceFailed, got.State, "the run finishes once its session is gone")
}

// TestDispatcherEndsOccurrenceWhoseNameIsTaken pins that a session this
// occurrence did not create is never adopted, stopped or deleted, and that
// the occurrence ends rather than holding the owner's capacity forever.
func TestDispatcherEndsOccurrenceWhoseNameIsTaken(t *testing.T) {
	squat := func(sess *v1.AgentSession, pod *corev1.Pod) {
		sess.UID = "squatter-uid"
		sess.Spec.GoalExecution = nil
		pod.Name = ""
	}
	t.Run("taken before launch: session_name_taken, cancelled, squatter untouched", func(t *testing.T) {
		ctx := context.Background()
		now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
		f := newRunFixture(t, &now, squat)
		// The fixture attaches the session it built; model one taken before launch.
		_, err := f.store.Finish(ctx, f.occ, domain.OccurrenceCancelled, now)
		require.NoError(t, err)
		g := f.goal
		g, err = f.svc.Update(ctx, f.actor, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "revise", Action: "revise", Outcome: ptr.To("Send a reminder again")})
		require.NoError(t, err)
		g, err = f.svc.RequestExecution(ctx, f.actor, domain.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "request-again", Terms: f.goal.Execution.Terms})
		require.NoError(t, err)
		g, err = f.svc.DecideExecution(ctx, f.actor.Domain, g.ID, domain.ExecutionDecision{RequestID: "approval-again", Digest: g.Execution.Digest, Owner: f.actor.Domain.Owner, Approved: true, Witness: "test-only-human-witness"})
		require.NoError(t, err)
		o, err := f.store.Schedule(ctx, g)
		require.NoError(t, err)
		squatter := f.sess.DeepCopy()
		squatter.ResourceVersion, squatter.Name = "", o.SessionName
		require.NoError(t, f.k8s.Create(ctx, squatter))

		require.NoError(t, f.dispatcher("controller").Tick(ctx))
		got, err := f.store.Occurrence(ctx, o.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Outcome)
		assert.Equal(t, domain.RunSessionNameTaken, got.Outcome.Reason)
		assert.Equal(t, domain.OccurrenceCancelled, got.State, "the owner's capacity is released")
		assert.NoError(t, f.k8s.Get(ctx, client.ObjectKeyFromObject(squatter), &v1.AgentSession{}), "the squatter is not deleted")
	})
	t.Run("recreated under the attached name: session_missing, squatter untouched", func(t *testing.T) {
		ctx := context.Background()
		now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
		f := newRunFixture(t, &now, nil)
		require.NoError(t, f.k8s.Delete(ctx, f.pod))
		require.NoError(t, f.k8s.Delete(ctx, f.sess))
		squatter := f.sess.DeepCopy()
		squat(squatter, &corev1.Pod{})
		squatter.ResourceVersion = ""
		require.NoError(t, f.k8s.Create(ctx, squatter))

		now = now.Add(2 * time.Second)
		require.NoError(t, f.dispatcher("controller").Tick(ctx))
		got, err := f.store.Occurrence(ctx, f.occ.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Outcome)
		assert.Equal(t, domain.RunSessionMissing, got.Outcome.Reason)
		assert.Equal(t, domain.OccurrenceFailed, got.State)
		assert.NoError(t, f.k8s.Get(ctx, client.ObjectKeyFromObject(squatter), &v1.AgentSession{}), "the squatter is not deleted")
	})
}
