// pkg/controllers/agentsession/identitychoice_test.go
//
// White-box unit tests for reconcileIdentityChoice. Uses the shared fake-client
// fixtures (buildFakeClient / noopRunnerFactory from passthrough_gate_test.go),
// so no envtest — the branch matrix is deterministic and fast. The exhaustive
// state-graph verification driven through the full reconciler under envtest is
// Task 8; these tests pin the gate function's own decision logic. With no
// LifecycleMemory wired the fold-sync is skipped, so the fixture-provided
// status drives every branch below.
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

// identityClass returns an AgentClass with the given identity mode and optional
// choice timeout.
func identityClass(mode string, timeout *metav1.Duration) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode:          mode,
			IdentityChoiceTimeout: timeout,
		},
	}
}

// identitySession returns a minimal AgentSession referencing cls, with the
// given status applied via mutate (nil for the zero status).
func identitySession(mutate func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func TestReconcileIdentityChoice(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		ac          *spiceboxv1alpha1.AgentClass
		sess        *spiceboxv1alpha1.AgentSession
		wantProceed bool
		wantRequeue bool // RequeueAfter > 0
		check       func(t *testing.T, got *spiceboxv1alpha1.AgentSession)
	}{
		{
			name:        "static agent: mirrors EffectiveIdentityMode=agent, proceeds",
			ac:          identityClass(spiceboxv1alpha1.IdentityModeAgent, nil),
			sess:        identitySession(nil),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.IdentityModeAgent, got.Status.EffectiveIdentityMode)
			},
		},
		{
			name:        "static userPassthrough: mirrors EffectiveIdentityMode=userPassthrough, proceeds",
			ac:          identityClass(spiceboxv1alpha1.IdentityModeUserPassthrough, nil),
			sess:        identitySession(nil),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.IdentityModeUserPassthrough, got.Status.EffectiveIdentityMode)
			},
		},
		{
			name:        "static empty mode: defaults EffectiveIdentityMode=agent, proceeds",
			ac:          identityClass("", nil),
			sess:        identitySession(nil),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.IdentityModeAgent, got.Status.EffectiveIdentityMode)
			},
		},
		{
			name: "ask, unset, not yet parked: proceeds (runner drives the prompt), no mode set",
			ac:   identityClass(spiceboxv1alpha1.IdentityModeAsk, nil),
			sess: identitySession(func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
			}),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Empty(t, got.Status.EffectiveIdentityMode, "gate must not set the mode; the runner emits IdentityChoicePending")
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, got.Status.Phase, "gate must not change phase")
			},
		},
		{
			name: "ask, parked, ParkedAt unset: stamps ParkedAt, parks (proceed=false, requeue)",
			ac:   identityClass(spiceboxv1alpha1.IdentityModeAsk, nil),
			sess: identitySession(func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice
			}),
			wantProceed: false,
			wantRequeue: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				require.NotNil(t, got.Status.IdentityChoiceParkedAt, "ParkedAt should be stamped on first park")
				assert.Empty(t, got.Status.FailureReason, "still within the deadline: not failed")
			},
		},
		{
			name: "ask, parked past default timeout: IdentityChoiceTimeout fold ⇒ Failed/IdentityChoiceTimeout (proceed=false)",
			ac:   identityClass(spiceboxv1alpha1.IdentityModeAsk, nil),
			sess: identitySession(func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice
				parked := metav1.NewTime(time.Now().Add(-40 * time.Minute))
				s.Status.IdentityChoiceParkedAt = &parked
			}),
			wantProceed: false,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
				assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceTimeout, got.Status.FailureReason)
				assert.NotNil(t, got.Status.FinishedAt, "FinishedAt should be stamped on the terminal transition")
			},
		},
		{
			name: "ask, parked past custom short timeout: fails (exercises spec.identityChoiceTimeout)",
			ac:   identityClass(spiceboxv1alpha1.IdentityModeAsk, &metav1.Duration{Duration: time.Minute}),
			sess: identitySession(func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice
				parked := metav1.NewTime(time.Now().Add(-2 * time.Minute))
				s.Status.IdentityChoiceParkedAt = &parked
			}),
			wantProceed: false,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase)
				assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceTimeout, got.Status.FailureReason)
			},
		},
		{
			name: "ask, EffectiveIdentityMode=agent (choice made): proceeds (passthrough gate is a no-op for agent)",
			ac:   identityClass(spiceboxv1alpha1.IdentityModeAsk, nil),
			sess: identitySession(func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice
				s.Status.EffectiveIdentityMode = spiceboxv1alpha1.IdentityModeAgent
			}),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Empty(t, got.Status.FailureReason, "a made choice must not fail the session")
			},
		},
		{
			name: "dynamic, EffectiveIdentityMode=userPassthrough (choice made): proceeds (falls through to passthrough gate)",
			ac:   identityClass(spiceboxv1alpha1.IdentityModeDynamic, nil),
			sess: identitySession(func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice
				s.Status.EffectiveIdentityMode = spiceboxv1alpha1.IdentityModeUserPassthrough
			}),
			wantProceed: true,
			check: func(t *testing.T, got *spiceboxv1alpha1.AgentSession) {
				assert.Equal(t, spiceboxv1alpha1.IdentityModeUserPassthrough, got.Status.EffectiveIdentityMode)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildFakeClient(t, tc.ac, tc.sess)
			r := &Reconciler{Client: c, APIReader: c, RunnerFactory: noopRunnerFactory{}}

			res, proceed, err := r.reconcileIdentityChoice(ctx, tc.sess, tc.ac)
			require.NoError(t, err)
			assert.Equal(t, tc.wantProceed, proceed, "proceed")
			if tc.wantRequeue {
				assert.Greater(t, res.RequeueAfter, time.Duration(0), "should requeue at the choice deadline")
			}

			// Re-Get so the assertions see the persisted status (the operator writes
			// via WriteOwned; a proceed-without-persist branch leaves it unchanged).
			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(tc.sess), &got))
			if tc.check != nil {
				tc.check(t, &got)
			}
		})
	}
}
