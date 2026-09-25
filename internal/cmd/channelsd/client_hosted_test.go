package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// TestClientHostedHere_AgreesWithTheKindItself locks the predicate both
// proactive iterators (channelManager, sessionWatcher) use to skip
// channels/sessions channelsd does not relay.
//
// Derived from registry.All() rather than from a list of names, so a kind added
// later is covered without editing this test — the same shape as
// registry.TestNeedsWebhook_AgreesWithTheKindItself. The hardcoded four-kind
// table this replaced named browser, local, slack and fake, and so said nothing
// about either kind that went live since: github (webd-hosted, so client-hosted
// HERE and skipped) or agent (channelsd-hosted, so relayed).
//
// Every shipped kind is blank-imported by main.go, which is what makes the
// registry sweep here equal to the one the running process sees.
func TestClientHostedHere_AgreesWithTheKindItself(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds,
		"main.go's blank imports must have populated the registry — an empty one would make every row vacuous")

	var anyHosted, anyRelayed bool
	for _, k := range kinds {
		want := !k.RelayedByChannelsd()
		anyHosted = anyHosted || want
		anyRelayed = anyRelayed || !want
		t.Run(k.Name(), func(t *testing.T) {
			assert.Equal(t, want, clientHostedHere(k.Name()),
				"clientHostedHere must answer exactly the inverse of kind %q's own RelayedByChannelsd: "+
					"a relayed kind that is skipped goes silently undelivered, and a client-hosted kind "+
					"that is processed produces spurious unknown-kind/nil-sender errors on every poll tick",
				k.Name())
		})
	}
	assert.True(t, anyHosted,
		"at least one registered kind must be client-hosted, or the equality above holds only in one direction")
	assert.True(t, anyRelayed,
		"at least one registered kind must be channelsd-relayed, or the equality above holds only in one direction")
}

// An unregistered kind is deliberately NOT client-hosted, so a real typo still
// surfaces as `unknown kind` downstream instead of being silently skipped; the
// empty kind likewise, so the test sessions that carry none are processed.
func TestClientHostedHere_UnregisteredAndEmptyAreNotClientHosted(t *testing.T) {
	assert.False(t, clientHostedHere("nope"),
		"a typo'd kind must reach the code that reports it by name, not be skipped as client-hosted")
	assert.False(t, clientHostedHere(""),
		"sessions carrying no kind must be processed normally")
}
