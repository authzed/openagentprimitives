package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestSessionWatcher_AwaitingCredentialsClearsWatchdog pins the contract
// that a session parked in AwaitingCredentials drops watchdog tracking on
// the next reconcile. Without this, the listener's postStartingStatus
// Touch starts the 30s silence countdown, and the agent's "Taking longer
// than expected" warn fires while the user is still in the middle of
// clicking through identityd's link flow.
func TestSessionWatcher_AwaitingCredentialsClearsWatchdog(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "sess",
			Labels:    map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		},
	}
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).Build()

	wd := newStatusWatchdog(nil, nil)
	wd.Touch("default", "sess")
	wd.mu.Lock()
	st := wd.state["default/sess"]
	require.NotNil(t, st, "post-Touch state must exist")
	st.LastActivity = time.Now().Add(-2 * time.Minute) // well past the warn window
	wd.mu.Unlock()

	sw := newSessionWatcher(cli, nil, wd)
	sw.reconcile(context.Background(), zap.New().WithName("test"))

	wd.mu.Lock()
	_, stillTracked := wd.state["default/sess"]
	wd.mu.Unlock()
	assert.False(t, stillTracked, "AwaitingCredentials reconcile must Clear (which Forgets) so a prior warn doesn't strand")
}
