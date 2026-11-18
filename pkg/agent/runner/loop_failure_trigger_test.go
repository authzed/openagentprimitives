package runner

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	rtNS          = "default"
	rtSession     = "demo-reviewbot-gh-abc123"
	rtChannelName = "demo-reviewbot-gh"
	rtSecretName  = "demo-reviewbot-gh-creds"
	rtKindName    = "faketrigger-runner"
)

// runnerTriggerKind stands in for github: a channel kind whose trigger has a
// status surface, recording what the runner did to it.
type runnerTriggerKind struct {
	fakekind.Kind

	mu        sync.Mutex
	standing  *channelkinds.TriggerClaim
	claims    int
	published []channelkinds.TriggerConclusion
}

var _ channelkinds.TriggerStatusReporter = (*runnerTriggerKind)(nil)

func (*runnerTriggerKind) Name() string               { return rtKindName }
func (*runnerTriggerKind) TriggerSurfaceKind() string { return "a pull request's fixture check run" }
func (*runnerTriggerKind) TriggerProviderStateIn(string) channelkinds.TriggerProviderState {
	return channelkinds.TriggerProviderState{}
}

func (k *runnerTriggerKind) TriggerSurface(
	*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.ChannelBinding,
	channelkinds.WebhookSecrets, channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	return &runnerTriggerSurface{kind: k}, nil
}

type runnerTriggerSurface struct{ kind *runnerTriggerKind }

func (*runnerTriggerSurface) Surface() string { return "the fixture check run" }

func (s *runnerTriggerSurface) Claim(context.Context) (channelkinds.TriggerClaim, error) {
	s.kind.mu.Lock()
	defer s.kind.mu.Unlock()
	s.kind.claims++
	if s.kind.standing != nil {
		return *s.kind.standing, nil
	}
	return channelkinds.TriggerClaim{Ref: "fixture run 1"}, nil
}

func (s *runnerTriggerSurface) Conclude(_ context.Context, c channelkinds.TriggerConclusion) error {
	s.kind.mu.Lock()
	defer s.kind.mu.Unlock()
	s.kind.published = append(s.kind.published, c)
	return nil
}

// runnerTrigger is the process-wide registration (the registry panics on a
// duplicate and has no Unregister); reset per case.
var runnerTrigger = func() *runnerTriggerKind {
	k := &runnerTriggerKind{}
	chregistry.Register(k)
	return k
}()

func resetRunnerTrigger(t *testing.T) *runnerTriggerKind {
	t.Helper()
	runnerTrigger.mu.Lock()
	defer runnerTrigger.mu.Unlock()
	runnerTrigger.standing, runnerTrigger.claims, runnerTrigger.published = nil, 0, nil
	return runnerTrigger
}

// triggerCluster holds the input Channel and its credentials, plus the session
// itself when withSession — without it a real StatusPatcher's Failed write
// cannot land.
func triggerCluster(t *testing.T, withSession bool) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	objs := []client.Object{
		&spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: rtChannelName, Namespace: rtNS},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: rtKindName, Role: spiceboxv1alpha1.ChannelRoleInput,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: rtSecretName},
			},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rtSecretName, Namespace: rtNS}},
	}
	if withSession {
		objs = append(objs, &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: rtSession, Namespace: rtNS},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
		})
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
}

// TestFail_AnswersTheTriggerTheRunnerLeftOpen pins the gap a triggered session
// used to fall into: the runner wrote Failed (a budget cap, a provider error,
// a halt) and the pull request's check run went on showing "in progress",
// because only the operator's own failure paths ever answered it.
func TestFail_AnswersTheTriggerTheRunnerLeftOpen(t *testing.T) {
	const reason = spiceboxv1alpha1.ReasonAgentSessionBudget
	cases := []struct {
		name string
		// wired: whether FailedTrigger is set (a triggered session) at all.
		wired bool
		// writeLands: whether the Failed status write persists.
		writeLands bool
		standing   *channelkinds.TriggerClaim
		wantErr    bool
		wantClaims int
		// wantPublished: the trigger now reads could_not_finish.
		wantPublished bool
	}{
		{
			name:  "triggered session fails in the runner: trigger concluded could_not_finish with the reason",
			wired: true, writeLands: true,
			wantClaims: 1, wantPublished: true,
		},
		{
			name:  "agent already posted its verdict before failing: verdict kept, nothing published",
			wired: true, writeLands: true,
			standing:   &channelkinds.TriggerClaim{Concluded: true, Outcome: channelkinds.TriggerOutcomeClean},
			wantClaims: 1,
		},
		{
			name:  "Failed write did not land: session is not failing, so the trigger is not touched",
			wired: true, writeLands: false,
			wantErr: true,
		},
		{
			name:  "no trigger wired (kubectl-driven session): nothing claimed or published",
			wired: false, writeLands: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := resetRunnerTrigger(t)
			k.standing = tc.standing
			c := triggerCluster(t, tc.writeLands)
			key := client.ObjectKey{Namespace: rtNS, Name: rtSession}

			l := &Loop{
				Status:     NewStatusPatcher(c, key),
				SessionKey: memory.NamespacedName{Namespace: rtNS, Name: rtSession},
			}
			if tc.wired {
				l.FailedTrigger = &triggeranswer.Trigger{
					Client: c, Namespace: rtNS,
					Binding: &spiceboxv1alpha1.ChannelBinding{Name: rtChannelName, Kind: rtKindName, Key: "pr:demo-org/platform#42"},
				}
			}

			err := l.fail(context.Background(), reason, "run-time exceeded budget.maxDuration")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			k.mu.Lock()
			claims, published := k.claims, append([]channelkinds.TriggerConclusion(nil), k.published...)
			k.mu.Unlock()
			assert.Equal(t, tc.wantClaims, claims)
			if !tc.wantPublished {
				assert.Empty(t, published)
				return
			}
			require.Len(t, published, 1)
			assert.Equal(t, channelkinds.TriggerOutcomeCouldNotFinish, published[0].Outcome)
			assert.Equal(t, triggeranswer.StoppedSummary(reason), published[0].Summary,
				"the runner publishes the same wording the operator's mid-run failure does")
			assert.NotContains(t, published[0].Summary, "budget.maxDuration",
				"the operator-facing message must not reach the trigger's surface")
		})
	}
}
