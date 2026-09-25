// Registry-wide coherence for the two halves of one fact:
// channelkinds.Kind.SupportsMonitoring and NewMonitoringSender.
//
// This test lives here, not in pkg/channels/channelkinds, because this is the binary
// that blank-imports EVERY kind (main.go) -- so registry.All() is genuinely
// complete only from here. A kind whose two answers disagree is invisible in
// its own package's tests and fatal in production: the credential-update
// watcher's deliverability pre-check
// (pkg/channels/channelsd/pipeline/credential_update.go) can only see
// SupportsMonitoring, has no resolved Secret with which to construct a sender,
// and decides from that ONE answer whether a platform admin can be reached at
// all. A kind that claims support and hands back nil makes that watcher stamp
// CardDelivered=True for a card the relay silently dropped -- the precise
// false claim the CardDelivered condition exists to prevent.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func TestEveryRegisteredKindAgreesWithItselfAboutMonitoring(t *testing.T) {
	kinds := registry.All()
	if len(kinds) == 0 {
		t.Fatal("no kinds registered; this test proves nothing without main.go's blank imports")
	}
	for _, k := range kinds {
		t.Run(k.Name()+": SupportsMonitoring matches NewMonitoringSender", func(t *testing.T) {
			ch := &spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon"},
				Spec: spiceboxv1alpha1.ChannelSpec{
					Kind: k.Name(),
					Role: spiceboxv1alpha1.ChannelRoleMonitoring,
					Slack: &spiceboxv1alpha1.SlackChannelConfig{
						OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C_MON"},
					},
				},
			}
			sender := k.NewMonitoringSender(channelkinds.Deps{Channel: ch})
			if k.SupportsMonitoring() {
				assert.NotNil(t, sender,
					"kind %q claims SupportsMonitoring but returns a nil MonitoringSender; callers that can only see the claim would record a delivery nobody received", k.Name())
				return
			}
			assert.Nil(t, sender,
				"kind %q denies SupportsMonitoring but returns a real MonitoringSender; the credential-update watcher would refuse a surface that in fact works", k.Name())
		})
	}
}
