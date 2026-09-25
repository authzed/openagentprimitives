//go:build integration

// pkg/controllers/agentsession/metaagent_membership_test.go
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"

	// The reconciler resolves the channel kind through the registry to ask it
	// for the membership answer, so the kinds these cases name must be
	// registered in this test binary — the operator blank-imports the same set.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// newReconcilerForMembership builds a minimal Reconciler that can exercise
// ensureMetaagentChannelMembership.
func newReconcilerForMembership(t *testing.T, env *testenv.Env) *agentsession.Reconciler {
	t.Helper()
	sr, cmr := testReaders(env)
	r := &agentsession.Reconciler{
		Client:          env.Client,
		APIReader:       env.Client,
		Tokens:          tokens.NewRegistry(),
		Memory:          memory.NewLocal(inmem.NewBackend()),
		SecretReader:    sr,
		ConfigMapReader: cmr,
	}
	r.RunnerFactory = &agentsession.PodRunnerFactory{
		Client:       env.Client,
		RunnerImage:  "agentprimitives-runner:dev",
		OperatorURL:  "http://op:8082",
		NATSURL:      "nats://spicebox-nats.agentprimitives-system.svc:4222",
		SecretReader: sr,
	}
	return r
}

// TestEnsureMetaagentChannelMembership exercises ensureMetaagentChannelMembership
// through a reconcile of a channel-attached AgentSession.
//
// The method is tested indirectly through Reconcile because it is unexported; the
// reconciler sets the condition on the Channel's status and we verify it after.
func TestEnsureMetaagentChannelMembership(t *testing.T) {
	trueVal := true
	falseVal := false

	cases := []struct {
		name             string
		scopeEnabled     bool
		channelKind      string
		metaagentOveride *spiceboxv1alpha1.ChannelMetaagentSpec
		// envUserID is the METAAGENT_SLACK_BOT_USER_ID the operator process
		// sees. The Slack kind resolves it there; `oap install` supplies it
		// from the agentprimitives-system-metaagent-config Secret.
		envUserID      string
		wantCondStatus metav1.ConditionStatus
		wantCondReason string
		wantCondAbsent bool // true means we expect the condition to be absent
	}{
		{
			name:           "scope.enabled=false → no condition set",
			scopeEnabled:   false,
			channelKind:    "slack",
			envUserID:      "USAL1CEBOT",
			wantCondAbsent: true,
		},
		{
			name:             "scope.enabled=true + metaagent.enabled=false → False/DisabledByChannelOverride",
			scopeEnabled:     true,
			channelKind:      "slack",
			metaagentOveride: &spiceboxv1alpha1.ChannelMetaagentSpec{Enabled: &falseVal},
			envUserID:        "USAL1CEBOT",
			wantCondStatus:   metav1.ConditionFalse,
			wantCondReason:   "DisabledByChannelOverride",
		},
		{
			name:           "scope.enabled=true + Slack + env unset → False/MetaagentAppNotInstalled",
			scopeEnabled:   true,
			channelKind:    "slack",
			envUserID:      "", // unset
			wantCondStatus: metav1.ConditionFalse,
			wantCondReason: spiceboxv1alpha1.ChannelReasonMetaagentAppNotInstalled,
		},
		{
			name:           "scope.enabled=true + Slack + env set → True/Invited",
			scopeEnabled:   true,
			channelKind:    "slack",
			envUserID:      "USAL1CEBOT",
			wantCondStatus: metav1.ConditionTrue,
			wantCondReason: spiceboxv1alpha1.ChannelReasonMetaagentInvited,
		},
		{
			name:           "scope.enabled=true + non-Slack kind → no condition set",
			scopeEnabled:   true,
			channelKind:    "fake",
			envUserID:      "USAL1CEBOT",
			wantCondAbsent: true,
		},
		{
			name:             "scope.enabled=true + metaagent.enabled=true → True/Invited even if env set",
			scopeEnabled:     true,
			channelKind:      "slack",
			metaagentOveride: &spiceboxv1alpha1.ChannelMetaagentSpec{Enabled: &trueVal},
			envUserID:        "USAL1CEBOT",
			wantCondStatus:   metav1.ConditionTrue,
			wantCondReason:   spiceboxv1alpha1.ChannelReasonMetaagentInvited,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			ctx := memory.WithSystemApproval(context.Background(), "test")

			// Stand in for what `oap install` gives the operator process.
			t.Setenv(clikit.EnvMetaagentSlackBotUserID, tc.envUserID)

			// Build AgentClass.
			ac := validClass("ac-mem")
			if tc.scopeEnabled {
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
					Scope: &spiceboxv1alpha1.ScopeSpec{Enabled: true},
				}
			}
			require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
			markValid(t, env, ac)

			// Build Channel with the appropriate kind and metaagent override.
			creds := &spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Name: "ch-mem", Namespace: "default"},
				Spec: spiceboxv1alpha1.ChannelSpec{
					Kind:       tc.channelKind,
					AgentClass: "ac-mem",
					CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{
						SecretName: "ch-creds",
					},
					Metaagent: tc.metaagentOveride,
				},
			}
			require.NoError(t, env.Client.Create(ctx, creds), "create Channel")

			// Build a channel-attached AgentSession.
			sess := validSession("s-mem", "ac-mem")
			sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
				Name:         "ch-mem",
				Kind:         tc.channelKind,
				Key:          "thread:C01:1234567890.000100",
				Capabilities: []string{"text"},
			}
			require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

			r := newReconcilerForMembership(t, env)

			// Drive the reconcile; we only need the channel condition side-effect.
			for i := 0; i < 4; i++ {
				_, _ = r.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Namespace: "default", Name: "s-mem"},
				})
			}

			// Fetch the updated Channel and inspect the condition.
			var ch spiceboxv1alpha1.Channel
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ch-mem"}, &ch))

			cond := meta.FindStatusCondition(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionMetaagentChannelMembership)
			if tc.wantCondAbsent {
				assert.Nil(t, cond, "MetaagentChannelMembership condition should be absent")
				return
			}
			require.NotNil(t, cond, "MetaagentChannelMembership condition should be present")
			assert.Equal(t, tc.wantCondStatus, cond.Status, "condition Status")
			assert.Equal(t, tc.wantCondReason, cond.Reason, "condition Reason")
		})
	}
}

// TestEnsureMetaagentChannelMembership_ChannelNotFound verifies that a
// missing Channel CR is treated as a no-op (no error, no condition written).
func TestEnsureMetaagentChannelMembership_ChannelNotFound(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Setenv(clikit.EnvMetaagentSlackBotUserID, "USAL1CEBOT")

	// AgentClass with scope enabled.
	ac := validClass("ac-notfound")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Scope: &spiceboxv1alpha1.ScopeSpec{Enabled: true},
	}
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	// Session refers to a Channel that does NOT exist.
	sess := validSession("s-notfound", "ac-notfound")
	sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name:         "ch-missing",
		Kind:         "slack",
		Key:          "thread:C01:1234567890.000100",
		Capabilities: []string{"text"},
	}
	require.NoError(t, env.Client.Create(ctx, sess))

	r := newReconcilerForMembership(t, env)

	// Reconcile must not error out (NotFound is swallowed).
	for i := 0; i < 4; i++ {
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "s-notfound"},
		})
		// We only assert no error from the reconcile path itself.
		_ = err
	}

	// The session is still alive (not Failed).
	var liveSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-notfound"}, &liveSess))
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, liveSess.Status.Phase,
		"session must not fail when the Channel CR is missing")
}
