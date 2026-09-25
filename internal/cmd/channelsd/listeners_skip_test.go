package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// A role=output Channel still gets an inbound listener — only monitoring is
// listener-less. This guards against a future change that adds output to the
// reconcile skip set, which would silently break reviewbot's Slack-requested
// reviews (its slack Channel is role=output AND the surface humans request
// reviews on). See ChannelSpec.Role and the reconcile loop's skip.
func TestListenerlessRoleOnlyMonitoring(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{spiceboxv1alpha1.ChannelRoleMonitoring, true},
		{spiceboxv1alpha1.ChannelRoleOutput, false},
		{spiceboxv1alpha1.ChannelRoleInput, false},
		{spiceboxv1alpha1.ChannelRoleBoth, false},
		{"", false}, // default role gets a listener too
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			assert.Equal(t, tc.want, listenerlessRole(tc.role),
				"only monitoring is listener-less; role %q must not be skipped", tc.role)
		})
	}
}

func TestChannelsdHostsKind(t *testing.T) {
	cases := []struct {
		name string
		kind string
		want bool
	}{
		{name: "fake kind: channelsd hosts it", kind: "fake", want: true},
		{name: "local kind: client-hosted, not hosted by channelsd", kind: "local", want: false},
		{name: "unknown kind: not hosted (skip, fail safe)", kind: "nope", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, ok := registry.Get(tc.kind)
			got := ok && k.RelayedByChannelsd()
			assert.Equal(t, tc.want, got)
		})
	}
}
