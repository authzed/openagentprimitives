package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func adoptionSession(name, phase string, deleting bool) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
	if deleting {
		now := metav1.Now()
		s.DeletionTimestamp = &now
		s.Finalizers = []string{spiceboxv1alpha1.FinalizerAgentSession}
	}
	return s
}

func TestSessionAdoptionIsStale(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		adoptionSession("other-running", spiceboxv1alpha1.AgentSessionPhaseRunning, false),
		adoptionSession("other-idle", spiceboxv1alpha1.AgentSessionPhaseIdle, false),
		adoptionSession("other-succeeded", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false),
		adoptionSession("other-failed", spiceboxv1alpha1.AgentSessionPhaseFailed, false),
		adoptionSession("other-deleting", spiceboxv1alpha1.AgentSessionPhaseRunning, true),
	).Build()
	r := &Reconciler{Client: c}

	cases := []struct {
		name  string
		self  *spiceboxv1alpha1.AgentSession
		owner string
		stale bool
	}{
		{"self terminal: stale, its reads are over", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false), "self", true},
		{"self deleting: stale", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseRunning, true), "self", true},
		{"self running: kept", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseRunning, false), "self", false},
		{"other running: kept", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false), "other-running", false},
		{"other idle: kept, it may wake and read", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false), "other-idle", false},
		{"other succeeded: stale", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false), "other-succeeded", true},
		{"other failed: stale", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false), "other-failed", true},
		{"other deleting: stale", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false), "other-deleting", true},
		{"other gone: stale", adoptionSession("self", spiceboxv1alpha1.AgentSessionPhaseSucceeded, false), "other-gone", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.sessionAdoptionIsStale(context.Background(), tc.self, types.NamespacedName{Namespace: "default", Name: tc.owner})
			assert.Equal(t, tc.stale, got)
		})
	}
}
