// pkg/controllers/agentsession/failedtrigger_reap_test.go
//
// The failed-trigger backstop must never stand between a Failed session and
// its cleanup. Driven through Reconcile, over the reap fixture, because the
// property is about ORDER inside Reconcile: an answer that could not be
// delivered asks for a retry, and the reap, session GC and storage reclaim
// still run in that same pass with the retry folded into their requeue.
package agentsession

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	// downKindName is a kind whose trigger surface exists and whose provider
	// is down: every Claim fails, so every answer attempt is a transient
	// failure the backstop wants to retry.
	downKindName    = "faketrigger-provider-down"
	downChannelName = "demo-reviewbot-down"
	downSecretName  = "demo-reviewbot-down-creds"
)

type providerDownKind struct{ fakekind.Kind }

var _ channelkinds.TriggerStatusReporter = providerDownKind{}

func (providerDownKind) Name() string               { return downKindName }
func (providerDownKind) TriggerSurfaceKind() string { return "a pull request's fixture check run" }
func (providerDownKind) TriggerProviderStateIn(string) channelkinds.TriggerProviderState {
	return channelkinds.TriggerProviderState{}
}

func (providerDownKind) TriggerSurface(
	*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.ChannelBinding,
	channelkinds.WebhookSecrets, channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	return providerDownSurface{}, nil
}

type providerDownSurface struct{}

func (providerDownSurface) Surface() string { return "the fixture check run on demo-org/platform#42" }
func (providerDownSurface) Claim(context.Context) (channelkinds.TriggerClaim, error) {
	return channelkinds.TriggerClaim{}, errors.New("status 503")
}

func (providerDownSurface) Conclude(context.Context, channelkinds.TriggerConclusion) error {
	return errors.New("status 503")
}

func init() { chregistry.Register(providerDownKind{}) }

// bindToDownProvider makes the fixture's session a triggered one whose
// provider is down: an input binding of downKindName, plus the Channel and
// credentials Secret the answer is addressed through.
func (f reapFixture) bindToDownProvider(t *testing.T) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, f.c.Create(ctx, &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: downChannelName, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           downKindName,
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: downSecretName},
		},
	}))
	require.NoError(t, f.c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: downSecretName, Namespace: "default"},
	}))

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(ctx, client.ObjectKeyFromObject(f.sess), &sess))
	sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: downChannelName, Kind: downKindName, Key: "pr:demo-org/platform#42",
		NATSSubjectPrefix: "ap.session.default." + sess.Name,
	}
	require.NoError(t, f.c.Update(ctx, &sess))
}

func (f reapFixture) triggerSettled(t *testing.T) bool {
	t.Helper()
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(memory.WithSystemApproval(context.Background(), "test"),
		client.ObjectKeyFromObject(f.sess), &sess))
	return conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionTriggerAnswered) != nil
}

func TestReconcile_FailedTriggerRetryDoesNotBlockCleanup(t *testing.T) {
	cases := []struct {
		name string
		// finishedDelta places finishedAt relative to the fixture's clock.
		finishedDelta time.Duration
		wantReaped    bool
		wantRequeue   time.Duration
	}{
		{
			name:          "provider down, past the reap grace: sandbox reaped in the same pass, requeue carries the answer retry",
			finishedDelta: -2 * time.Minute,
			wantReaped:    true,
			wantRequeue:   failedTriggerRetry,
		},
		{
			name:          "provider down, within the reap grace: requeue is the sooner of the remaining grace and the answer retry",
			finishedDelta: -20 * time.Second,
			wantReaped:    false,
			wantRequeue:   40 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseFailed, time.Minute, tc.finishedDelta)
			f.bindToDownProvider(t)

			res := f.reconcile(t)

			assert.Equal(t, tc.wantReaped, f.sandboxGone(t))
			assert.Equal(t, tc.wantRequeue, res.RequeueAfter)
			assert.False(t, f.triggerSettled(t), "a transient failure is left unsettled so the retry can answer")
			assert.True(t, f.sessionPresent(t))
		})
	}
}
