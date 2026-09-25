package v1alpha1

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSessionHold_roundTrips(t *testing.T) {
	in := &SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "hold-1", Namespace: "demo"},
		Spec: SessionHoldSpec{
			SessionRef: NamespacedRef{Namespace: "demo", Name: "demo-session"},
			Reason:     "12 consecutive out-of-ceiling calls",
			Source:     "tripper/plangate-denial-streak",
		},
	}
	got := in.DeepCopy()
	require.NotNil(t, got)
	assert.Equal(t, "demo-session", got.Spec.SessionRef.Name)
	assert.Equal(t, "tripper/plangate-denial-streak", got.Spec.Source)
}

func TestSessionHold_specCarriesNoTimestamp(t *testing.T) {
	// The trip time is an observation and belongs in status. A volatile value in
	// an SSA-applied spec field makes re-apply non-idempotent.
	assert.NotContains(t, sessionHoldSpecFieldNames(), "TrippedAt",
		"TrippedAt is an observation; it belongs in status")
}

func TestSessionHold_isReleased(t *testing.T) {
	h := &SessionHold{Status: SessionHoldStatus{Phase: SessionHoldPhaseActive}}
	assert.False(t, h.IsReleased())
	h.Status.Phase = SessionHoldPhaseReleased
	assert.True(t, h.IsReleased())
}

func TestAgentSessionPhaseHeld_matchesLifecyclePhase(t *testing.T) {
	assert.Equal(t, "Held", AgentSessionPhaseHeld,
		"must stay byte-identical to lifecycle.PhaseHeld, which projects onto status.phase")
}

func sessionHoldSpecFieldNames() []string {
	t := reflect.TypeOf(SessionHoldSpec{})
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		names = append(names, t.Field(i).Name)
	}
	return names
}
