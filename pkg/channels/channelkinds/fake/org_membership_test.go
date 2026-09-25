package fake

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestKind_AttributesOrgMembership pins the fake kind's capability contract:
// org-membership attribution is opt-in per Channel via spec.fake.orgScoped,
// so a scenario can exercise the session-start gate while every ordinary
// test channel keeps its pre-gate behavior. The var assignment is the
// assertion that Kind implements the optional capability interface.
func TestKind_AttributesOrgMembership(t *testing.T) {
	var k channelkinds.OrgMembershipAttributor = Kind{}

	cases := []struct {
		name string
		ch   *spiceboxv1alpha1.Channel
		want bool
	}{
		{name: "nil channel: not attributing", ch: nil, want: false},
		{name: "no fake config: not attributing", ch: &spiceboxv1alpha1.Channel{}, want: false},
		{
			name: "fake config without orgScoped: not attributing",
			ch: &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{
				Fake: &spiceboxv1alpha1.FakeChannelConfig{},
			}},
			want: false,
		},
		{
			name: "orgScoped: attributing",
			ch: &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{
				Fake: &spiceboxv1alpha1.FakeChannelConfig{OrgScoped: true},
			}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, k.AttributesOrgMembership(tc.ch))
		})
	}
}
