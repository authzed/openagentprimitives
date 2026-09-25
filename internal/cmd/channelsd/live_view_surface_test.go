package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry/crdenumtest"
)

// TestSupportsLiveViewOffer_IsStatedPerKind pins each registered kind's answer
// LITERALLY rather than reading it back off the declaration.
//
// A test that walked the kinds and asserted whatever they said would follow the
// declaration wherever it went, and so could never catch one wrongly widened —
// which is the direction that hurts: a kind claiming a live-view surface it has
// no sender for puts the tool back to promising the user a button that is never
// rendered. Only a hardcoded expectation catches that.
//
// This lives in channelsd because channelsd blank-imports every kind the CRD
// enum admits (see TestChannelKindsRegistered), so the registry here is the
// full set rather than whichever kinds one package happened to link.
func TestSupportsLiveViewOffer_IsStatedPerKind(t *testing.T) {
	want := map[string]bool{
		// Posts a "👁 View live" interaction button.
		"slack": true,
		// Emit a browser link as a timeline note from their Host.
		"browser": true,
		"local":   true,
		// The in-process test kind records a fixed set of sub-channels, and
		// live_view_offer is not among them.
		"fake": false,
		// Input-only transports: no outbound surface of any kind to render on.
		"bento":  false,
		"github": false,
		// Session-to-session: the counterparty is another AgentSession, which
		// has no browser to open a live view in and no surface to render the
		// offer on. SubChannelSender returns nil for every name here, so the
		// derived check below holds this one to the same answer.
		"agent": false,
	}

	for name, expect := range want {
		t.Run(name, func(t *testing.T) {
			k, ok := registry.Get(name)
			require.True(t, ok, "kind %q must be registered in this binary", name)
			assert.Equal(t, expect, k.SupportsLiveViewOffer(),
				"kind %q's declared live-view surface", name)
		})
	}

	// Every kind the shipped CRD admits must appear above. Derived from the
	// enum rather than counted off the registry, which other tests in this
	// package add stub kinds to — and derived rather than eyeballed, so a kind
	// added to the enum arrives here as a failure instead of as a silent
	// omission nobody decided about.
	for _, kind := range crdenumtest.ChannelKindEnum(t) {
		_, listed := want[kind]
		assert.True(t, listed,
			"channel kind %q is admitted by the CRD but states no live-view answer in this test", kind)
	}
}

// TestSupportsLiveViewOffer_AgreesWithTheSenderWhereItCanBeDerived checks the
// declaration against the thing it declares, for the kinds where that is
// possible at all.
//
// It is possible only for the kinds channelsd hosts. A client-hosted kind
// (local, browser) returns nil from the bare SubChannelSender because its real
// senders are built by its Host, in the user's own process — so deriving the
// answer there would report "no surface" for two kinds that plainly have one.
// That asymmetry is exactly why the declaration exists rather than a registry
// lookup, and it is why those kinds are exempt HERE and pinned literally above.
func TestSupportsLiveViewOffer_AgreesWithTheSenderWhereItCanBeDerived(t *testing.T) {
	for _, name := range crdenumtest.ChannelKindEnum(t) {
		k, ok := registry.Get(name)
		require.True(t, ok, "kind %q must be registered in this binary", name)
		if !k.RelayedByChannelsd() {
			continue
		}
		t.Run(k.Name(), func(t *testing.T) {
			hasSender := k.SubChannelSender("live_view_offer", channelkinds.Deps{}) != nil
			assert.Equal(t, hasSender, k.SupportsLiveViewOffer(),
				"a kind declaring a live-view surface must return a sender for it, and vice versa — "+
					"the relay drops a nil-sender envelope with only a log line, so a disagreement is silent in production")
		})
	}
}
