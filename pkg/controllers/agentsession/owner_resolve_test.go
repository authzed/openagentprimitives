package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestResolveOwnerSubject(t *testing.T) {
	const starter = "user:YWJjQGV4YW1wbGUuY29t"
	cases := []struct {
		name       string
		identMode  string
		owner      *spiceboxv1alpha1.ChannelOwnerPolicy
		starterAnn string
		outputRef  string
		want       string
		wantErr    bool
	}{
		{name: "agent + starter present ⇒ starter", identMode: "agent", starterAnn: starter, want: starter},
		{name: "agent + explicit ⇒ explicit overrides", identMode: "agent", starterAnn: starter,
			owner: &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:sec#member"}, want: "group:sec#member"},
		{name: "agent + no starter + ownerless.permission ⇒ permission", identMode: "agent",
			owner: &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "team:x#member"}},
			want:  "team:x#member"},
		{name: "agent + no starter + fromOutputChannel ⇒ output group", identMode: "agent",
			owner:     &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true}},
			outputRef: "slack_channel:C9#member", want: "slack_channel:C9#member"},
		{name: "agent + no starter + no declared owner + output group ⇒ output group", identMode: "agent",
			outputRef: "slack_channel:C0DEMO123#member", want: "slack_channel:C0DEMO123#member"},
		{name: "agent + no starter + declared permission ⇒ permission, not the offered output group", identMode: "agent",
			owner:     &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "team:x#member"}},
			outputRef: "slack_channel:C0DEMO123#member", want: "team:x#member"},
		{name: "passthrough ⇒ starter forced", identMode: "userPassthrough", starterAnn: starter, want: starter},
		{name: "agent + nothing resolvable ⇒ error", identMode: "agent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOwnerSubject(tc.identMode, tc.owner, tc.starterAnn, tc.outputRef, nil)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
