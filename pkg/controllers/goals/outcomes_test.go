package goals

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type runAuthority struct{ err error }

func (*runAuthority) Authorize(context.Context, domain.Actor, bool) error                { return nil }
func (*runAuthority) Sources(context.Context, domain.Actor) ([]domain.Source, error)     { return nil, nil }
func (*runAuthority) ReadGoal(context.Context, domain.Actor, domain.Goal) error          { return nil }
func (*runAuthority) Validate(context.Context, domain.Goal, domain.ExecutionTerms) error { return nil }
func (*runAuthority) VerifyDecision(context.Context, domain.Goal, domain.ExecutionDecision) error {
	return nil
}
func (a *runAuthority) AuthorizeDispatch(context.Context, domain.Goal) error { return a.err }

func TestDispatcherDurableStopRecovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		phase  corev1.PodPhase
		reason domain.RunReason
		state  domain.OccurrenceState
	}{
		{"idle session with finished runner", corev1.PodSucceeded, domain.RunSessionEnded, domain.OccurrenceFinished},
		{"failed runner", corev1.PodFailed, domain.RunInfrastructureFailed, domain.OccurrenceFailed},
		{"time limit", corev1.PodRunning, domain.RunDurationExpired, domain.OccurrenceFailed},
		{"cancel", corev1.PodRunning, domain.RunCancelled, domain.OccurrenceCancelled},
		{"pause", corev1.PodRunning, domain.RunPaused, domain.OccurrenceCancelled},
		{"revise", corev1.PodRunning, domain.RunSuperseded, domain.OccurrenceCancelled},
		{"consent expires", corev1.PodRunning, domain.RunConsentExpired, domain.OccurrenceCancelled},
		{"authority denial", corev1.PodRunning, domain.RunAuthorityDenied, domain.OccurrenceCancelled},
		{"missing session", corev1.PodRunning, domain.RunSessionMissing, domain.OccurrenceUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "runs.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			store := goalsqlite.New(db.DB())
			require.NoError(t, store.Migrate(ctx))
			now := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
			auth := &runAuthority{}
			svc := &domain.Service{Store: store, Auth: auth, ExecutionAuth: auth, Now: func() time.Time { return now }}
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
			sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: o.SessionName, UID: "session-uid", CreationTimestamp: metav1.NewTime(now)}, Spec: v1.AgentSessionSpec{Class: "assistant", GoalExecution: ref(o)}, Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseIdle}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "runner", OwnerReferences: []metav1.OwnerReference{{UID: sess.UID}}}, Status: corev1.PodStatus{Phase: test.phase}}
			scheme := runtime.NewScheme()
			require.NoError(t, v1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess, pod).Build()
			o, err = store.Claim(ctx, domain.ClaimRequest{ID: o.ID, Worker: "setup", Now: now, Lease: time.Second, OwnerLimit: 1, ClassLimit: 4})
			require.NoError(t, err)
			o, err = store.Attach(ctx, o, string(sess.UID), now)
			require.NoError(t, err)
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
			case domain.RunPaused, domain.RunSuperseded, domain.RunCancelled:
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
			// Restart between the durable observation and pod GC acknowledgement.
			d.Worker = "restarted-controller"
			now = now.Add(6 * time.Second)
			require.NoError(t, d.Tick(ctx))
			held, err := store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			if test.reason != domain.RunSessionMissing {
				require.Equal(t, domain.OccurrenceRunning, held.State, "pod still owns capacity")
			}
			require.NoError(t, k8s.Delete(ctx, pod))
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
			current, err := store.Get(ctx, g.Domain, g.ID)
			require.NoError(t, err)
			require.NotEqual(t, domain.Completed, current.State)
			if test.reason != domain.RunCancelled && test.reason != domain.RunPaused && test.reason != domain.RunSuperseded {
				require.Equal(t, g, current)
			}
		})
	}
}
