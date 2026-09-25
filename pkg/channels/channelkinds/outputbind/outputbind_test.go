package outputbind_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"

	// Register both kinds so Anchor exercises a real registry: slack
	// implements OutboundAnchorProvider, bento does not.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// slackCh builds a slack Channel with the given name/role/agentClass. A
// non-empty channelID attaches OutputDefaults so the Channel is anchorable.
func slackCh(t *testing.T, name, role, agentClass, channelID string) *spiceboxv1alpha1.Channel {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       "slack",
			Role:       role,
			AgentClass: agentClass,
			Slack:      &spiceboxv1alpha1.SlackChannelConfig{},
		},
	}
	if channelID != "" {
		ch.Spec.Slack.OutputDefaults = &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID}
	}
	return ch
}

func clientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		objs     []client.Object
		wantErr  error
		wantName string
	}{
		{
			name:     "exactly one role=output for the class: resolves it",
			objs:     []client.Object{slackCh(t, "test-output", spiceboxv1alpha1.ChannelRoleOutput, "demo-agent", "C1")},
			wantName: "test-output",
		},
		{
			name:    "no role=output for the class: ErrNoOutputChannel",
			objs:    []client.Object{slackCh(t, "other", spiceboxv1alpha1.ChannelRoleOutput, "other-agent", "C1")},
			wantErr: outputbind.ErrNoOutputChannel,
		},
		{
			name: "two role=output for the class: ErrAmbiguousOutputChannel",
			objs: []client.Object{
				slackCh(t, "out-a", spiceboxv1alpha1.ChannelRoleOutput, "demo-agent", "C1"),
				slackCh(t, "out-b", spiceboxv1alpha1.ChannelRoleOutput, "demo-agent", "C2"),
			},
			wantErr: outputbind.ErrAmbiguousOutputChannel,
		},
		{
			name:    "role=both is NOT an output candidate: ErrNoOutputChannel",
			objs:    []client.Object{slackCh(t, "interactive", spiceboxv1alpha1.ChannelRoleBoth, "demo-agent", "C1")},
			wantErr: outputbind.ErrNoOutputChannel,
		},
		{
			name: "role=both alongside a real output: the output wins, no ambiguity",
			objs: []client.Object{
				slackCh(t, "interactive", spiceboxv1alpha1.ChannelRoleBoth, "demo-agent", "C1"),
				slackCh(t, "test-output", spiceboxv1alpha1.ChannelRoleOutput, "demo-agent", "C2"),
			},
			wantName: "test-output",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := outputbind.Resolve(context.Background(), clientWith(t, tc.objs...), "default", "demo-agent")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantName, got.Name)
		})
	}
}

func TestAnchor(t *testing.T) {
	t.Run("anchorable slack channel: key + external", func(t *testing.T) {
		key, external, err := outputbind.Anchor(slackCh(t, "test-output", spiceboxv1alpha1.ChannelRoleOutput, "demo-agent", "C1"))
		require.NoError(t, err)
		assert.Equal(t, "channel:C1", key)
		assert.Equal(t, map[string]string{"channel_id": "C1"}, external)
	})

	t.Run("slack channel with no outputDefaults: ErrKindNoAnchor", func(t *testing.T) {
		_, _, err := outputbind.Anchor(slackCh(t, "test-output", spiceboxv1alpha1.ChannelRoleOutput, "demo-agent", ""))
		require.ErrorIs(t, err, outputbind.ErrKindNoAnchor)
	})

	t.Run("kind not implementing the interface: ErrKindNoAnchor", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"},
			Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "bento", Role: spiceboxv1alpha1.ChannelRoleOutput},
		}
		_, _, err := outputbind.Anchor(ch)
		require.ErrorIs(t, err, outputbind.ErrKindNoAnchor)
	})

	t.Run("unregistered kind: ErrKindNoAnchor", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
			Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "nope", Role: spiceboxv1alpha1.ChannelRoleOutput},
		}
		_, _, err := outputbind.Anchor(ch)
		require.ErrorIs(t, err, outputbind.ErrKindNoAnchor)
	})

	t.Run("nil channel: ErrKindNoAnchor", func(t *testing.T) {
		_, _, err := outputbind.Anchor(nil)
		require.ErrorIs(t, err, outputbind.ErrKindNoAnchor)
	})
}
