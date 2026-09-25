// pkg/controllers/agentsession/startapproval_gate_test.go
//
// White-box unit tests for reconcileStartApproval — the gate that withholds
// the runner (and, by returning early, owner resolution) while a session an
// org non-member started sits parked on AnnotationStartApprovalRequestRef.
// Mirrors identitychoice_test.go's fixture shape: fake client, no envtest.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// startApprovalSession returns a minimal AgentSession, parked for start
// approval when parked is true, with status tweaks applied via mutate.
func startApprovalSession(parked bool, mutate func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	if parked {
		s.Annotations = map[string]string{
			spiceboxv1alpha1.AnnotationStartApprovalRequestRef: "startappr-s1-1",
		}
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func TestReconcileStartApproval(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		sess        *spiceboxv1alpha1.AgentSession
		wantProceed bool
		wantRequeue bool
		check       func(t *testing.T, got *spiceboxv1alpha1.AgentSession)
	}{
		{
			name:        "no marker: proceeds untouched",
			sess:        startApprovalSession(false, nil),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Empty(t, got.Status.Phase, "gate must not touch an unmarked session")
				assert.Nil(t, got.Status.StartApprovalParkedAt)
			},
		},
		{
			name:        "marker present, fresh: parks — phase AwaitingStartApproval, ParkedAt stamped, requeue",
			sess:        startApprovalSession(true, nil),
			wantProceed: false,
			wantRequeue: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval, got.Status.Phase)
				require.NotNil(t, got.Status.StartApprovalParkedAt, "ParkedAt stamped on first sight")
				assert.Empty(t, got.Status.FailureReason, "within the deadline: not failed")
			},
		},
		{
			name: "marker present, already parked within deadline: stays parked, requeue",
			sess: startApprovalSession(true, func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval
				parked := metav1.NewTime(time.Now().Add(-time.Hour))
				s.Status.StartApprovalParkedAt = &parked
			}),
			wantProceed: false,
			wantRequeue: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval, got.Status.Phase)
				assert.Empty(t, got.Status.FailureReason)
			},
		},
		{
			name: "marker present, parked past the deadline: Failed/StartApprovalTimeout",
			sess: startApprovalSession(true, func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval
				parked := metav1.NewTime(time.Now().Add(-25 * time.Hour))
				s.Status.StartApprovalParkedAt = &parked
			}),
			wantProceed: false,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
				assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionStartApprovalTimeout, got.Status.FailureReason)
				assert.NotNil(t, got.Status.FinishedAt, "FinishedAt stamped on the terminal transition")
			},
		},
		{
			name: "marker present but session already terminal: never re-parks (stale-cache clobber guard)",
			sess: startApprovalSession(true, func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
				s.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionStartDenied
			}),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
					"a terminal session must stay terminal — the deny raced a stale marker read exactly here")
			},
		},
		{
			name: "marker present but StartFailure set (denied, phase read stale): never parks",
			sess: startApprovalSession(true, func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval
				s.Status.StartFailure = &spiceboxv1alpha1.AgentSessionStartFailure{
					Reason: spiceboxv1alpha1.ReasonAgentSessionStartDenied, Message: "denied",
				}
			}),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase,
					"a denied session must never be unparked to Pending")
			},
		},
		{
			name: "marker removed while phase still parked (approved): unparks to Pending, proceeds",
			sess: startApprovalSession(false, func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingStartApproval
				parked := metav1.NewTime(time.Now().Add(-time.Minute))
				s.Status.StartApprovalParkedAt = &parked
			}),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase,
					"an approved session resumes the normal start flow")
				assert.Empty(t, got.Status.FailureReason)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildFakeClient(t, tc.sess)
			r := &Reconciler{Client: c, APIReader: c, RunnerFactory: noopRunnerFactory{}}

			res, proceed, err := r.reconcileStartApproval(ctx, tc.sess)
			require.NoError(t, err)
			assert.Equal(t, tc.wantProceed, proceed, "proceed")
			if tc.wantRequeue {
				assert.Greater(t, res.RequeueAfter, time.Duration(0), "should requeue at the approval deadline")
			}

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(tc.sess), &got))
			if tc.check != nil {
				tc.check(t, &got)
			}
		})
	}
}
