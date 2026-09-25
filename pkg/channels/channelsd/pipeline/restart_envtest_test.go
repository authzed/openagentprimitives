//go:build integration

package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestEnvtest_RestartPublisher_PatchesPendingRestart drives the publisher
// (the function the Slack view_submission handler calls) against a real
// envtest apiserver. Verifies the parent AgentSession's
// Status.PendingRestart is populated with the correct fields after the
// publisher runs.
func TestEnvtest_RestartPublisher_PatchesPendingRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env := testenv.Shared(t)
	ns := "default"

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-pub", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo", Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi"}},
	}
	require.NoError(t, env.Client.Create(ctx, sess))
	sess.Status = spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle}
	require.NoError(t, env.Client.Status().Update(ctx, sess))

	publishCalls := 0
	publish := func(_ string, _ []byte) error { publishCalls++; return nil }
	pub := pipeline.NewRestartPublisher(publish, env.Client, testMarkerSigner)

	err := pub(ctx, channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: ns, Name: "sess-pub"},
		CutTurnIndex: 2,
		NewUserText:  "edited!",
		TriggeredBy: channelevents.ExternalIdentity{
			Kind: "slack", ExternalID: "U1", Email: "alice@example.com",
		},
		KindRequestRef: "view-abc",
	})
	require.NoError(t, err)

	// Verify status was patched.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "sess-pub"}, &got))
	require.NotNil(t, got.Status.PendingRestart)
	assert.Equal(t, int32(2), got.Status.PendingRestart.CutTurnIndex)
	assert.Equal(t, "edited!", got.Status.PendingRestart.NewUserText)
	assert.Contains(t, got.Status.PendingRestart.TriggeredBy, "user:", "canonical subject prefix")
	assert.NotEmpty(t, got.Status.PendingRestart.TargetSessionName)

	// Verify NATS publish was attempted (observability path).
	assert.Equal(t, 1, publishCalls, "publisher must publish exactly one NATS envelope")
}
