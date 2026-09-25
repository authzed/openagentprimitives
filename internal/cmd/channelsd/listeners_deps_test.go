package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/viewlink"
)

// TestBuildListenerDeps_WiresArtifactViewMinter is a regression guard for the
// bug where the live-view "View live" button posted fine (the offer sender had
// the minter via the senderResolver) but every click reported "Live view isn't
// configured" — because the LISTENER's Deps never received the
// ArtifactViewMinter. The sender and listener must be wired from the same
// minter, or the two gates (button-visibility vs click-capability) disagree.
func TestBuildListenerDeps_WiresArtifactViewMinter(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// Before wiring: the listener Deps carries no minter.
	require.Nil(t, m.buildListenerDeps(&spiceboxv1alpha1.Channel{}, nil).ArtifactViewMinter,
		"an unset minter must yield a nil Deps.ArtifactViewMinter")

	// After SetArtifactViewMinter: the same instance must reach the listener.
	// Use the shared viewlink.Minter — the channelsd-local type was moved there.
	minter := &viewlink.Minter{}
	m.SetArtifactViewMinter(minter)
	deps := m.buildListenerDeps(&spiceboxv1alpha1.Channel{}, nil)
	require.NotNil(t, deps.ArtifactViewMinter,
		"listener Deps must carry the artifact-view minter so the click handler can mint")
	assert.Same(t, minter, deps.ArtifactViewMinter,
		"listener must get the exact minter that was wired (same instance as the sender)")
}

// TestBuildListenerDeps_WiresSessionViewMinter mirrors
// TestBuildListenerDeps_WiresArtifactViewMinter for the session-view minter:
// SetSessionViewMinter must reach the listener's Deps, keeping the
// resolver/manager wiring symmetric even though no listener consumes it yet.
func TestBuildListenerDeps_WiresSessionViewMinter(t *testing.T) {
	m := newChannelManager(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	require.Nil(t, m.buildListenerDeps(&spiceboxv1alpha1.Channel{}, nil).SessionViewMinter,
		"an unset minter must yield a nil Deps.SessionViewMinter")

	minter := newSessionViewMinter(func() string { return "https://webd.example" })
	m.SetSessionViewMinter(minter)
	deps := m.buildListenerDeps(&spiceboxv1alpha1.Channel{}, nil)
	require.NotNil(t, deps.SessionViewMinter,
		"listener Deps must carry the session-view minter")
	assert.Same(t, minter, deps.SessionViewMinter,
		"listener must get the exact minter instance that was wired")
}
