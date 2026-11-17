package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

var _ channelkinds.GroupGoalHost = (*Kind)(nil)

func TestGroupGoalHost(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name         string
		binding      *spiceboxv1alpha1.ChannelBinding
		member, view string
		ok           bool
	}{
		{
			name: "channel thread: member and view refs of the channel",
			binding: &spiceboxv1alpha1.ChannelBinding{
				Kind: KindName, Key: "thread:C1:111.222",
				External: map[string]string{"channel_id": "C1", "thread_ts": "111.222"},
			},
			member: "slack_channel:C1#member", view: "slack_channel:C1#view", ok: true,
		},
		{
			name: "DM (D channel): not a group conversation",
			binding: &spiceboxv1alpha1.ChannelBinding{
				Kind: KindName, Key: "thread:D1:1.1", External: map[string]string{"channel_id": "D1"},
			},
		},
		{
			name: "group DM (G channel): not a group conversation",
			binding: &spiceboxv1alpha1.ChannelBinding{
				Kind: KindName, Key: "thread:G1:1.1", External: map[string]string{"channel_id": "G1"},
			},
		},
		{
			name:    "per-user DM key with no channel id: not a group conversation",
			binding: &spiceboxv1alpha1.ChannelBinding{Kind: KindName, Key: "dm:U1"},
		},
		{name: "nil binding: not ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, mok := k.ConversationMemberRef(tc.binding)
			a, aok := k.ConversationAudienceRef(tc.binding)
			assert.Equal(t, tc.ok, mok)
			assert.Equal(t, tc.ok, aok)
			assert.Equal(t, tc.member, m)
			assert.Equal(t, tc.view, a)
		})
	}

	t.Run("RunAnchor: copy with thread anchor cleared, same channel, input untouched", func(t *testing.T) {
		in := &spiceboxv1alpha1.ChannelBinding{
			Name: "c", Kind: KindName, Key: "thread:C1:111.222",
			External: map[string]string{
				"channel_id": "C1", "thread_ts": "111.222", "permalink": "https://x.slack.com/archives/C1/p1", "team_id": "T1",
			},
		}
		orig := in.DeepCopy()
		out, err := k.RunAnchor(in)
		require.NoError(t, err)
		assert.Equal(t, orig, in, "input must not be mutated")
		assert.Equal(t, "channel:C1", out.Key)
		assert.Equal(t, "C1", out.External["channel_id"])
		assert.Equal(t, "T1", out.External["team_id"])
		assert.NotContains(t, out.External, "thread_ts")
		assert.NotContains(t, out.External, "permalink")
	})

	t.Run("RunAnchor: a binding with no channel is an error", func(t *testing.T) {
		_, err := k.RunAnchor(&spiceboxv1alpha1.ChannelBinding{Kind: KindName, Key: "dm:U1"})
		require.Error(t, err)
	})

	assert.Equal(t, []string{"slack_channel#member"}, k.GroupMemberLinks(),
		"member ref only: the audience ref (#view) is never a group-goal owner type")
}

func TestMembershipCurrent(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns1"}}
	ch.Spec.Kind = KindName

	src := func(ns, kind string, ready *metav1.ConditionStatus) *spiceboxv1alpha1.RelationshipSource {
		s := &spiceboxv1alpha1.RelationshipSource{ObjectMeta: metav1.ObjectMeta{Name: "s-" + ns + kind, Namespace: ns}}
		s.Spec.Kind = kind
		if ready != nil {
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type: spiceboxv1alpha1.RelationshipSourceConditionReady, Status: *ready, Reason: "Why",
			})
		}
		return s
	}
	yes, no := metav1.ConditionTrue, metav1.ConditionFalse

	cases := []struct {
		name    string
		objs    []*spiceboxv1alpha1.RelationshipSource
		wantErr string
	}{
		{name: "ready slack sync in namespace: nil", objs: []*spiceboxv1alpha1.RelationshipSource{src("ns1", "slack", &yes)}},
		{name: "no source at all: error names the missing sync", wantErr: "no ready Slack directory sync (RelationshipSource) in namespace ns1"},
		{
			name:    "slack sync only in another namespace: error",
			objs:    []*spiceboxv1alpha1.RelationshipSource{src("other", "slack", &yes)},
			wantErr: "no ready Slack directory sync",
		},
		{
			name:    "non-slack source: error",
			objs:    []*spiceboxv1alpha1.RelationshipSource{src("ns1", "github", &yes)},
			wantErr: "no ready Slack directory sync",
		},
		{
			name:    "slack sync not Ready: error carries the reason",
			objs:    []*spiceboxv1alpha1.RelationshipSource{src("ns1", "slack", &no)},
			wantErr: "is not Ready: Why",
		},
		{
			name:    "slack sync with no Ready condition yet: error",
			objs:    []*spiceboxv1alpha1.RelationshipSource{src("ns1", "slack", nil)},
			wantErr: "is not Ready",
		},
		{
			name: "one not-ready and one ready: nil",
			objs: []*spiceboxv1alpha1.RelationshipSource{src("ns1", "slack", &no), func() *spiceboxv1alpha1.RelationshipSource {
				s := src("ns1", "slack", &yes)
				s.Name = "second"
				return s
			}()},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sch := runtime.NewScheme()
			require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
			b := fake.NewClientBuilder().WithScheme(sch)
			for _, o := range tc.objs {
				b = b.WithObjects(o).WithStatusSubresource(o)
			}
			err := (&Kind{}).MembershipCurrent(context.Background(), b.Build(), ch)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
