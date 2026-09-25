package main

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/viewlink"
)

// newArtifactViewMinter builds the ArtifactViewMinter webd's builtin chat uses
// to mint "<webd-base>/artifact-view?d=…&sig=…" links for the live_view_offer
// sub-channel sender (the agent's artifact_offer_view "View live" link).
//
// The signer is issued as channelsd (NOT identityd, webd's cookieSigner
// issuer): artifactViewDeps.VerifyLink verifies incoming artifact-view links
// WithExpectedIssuer(IssuerChannelsd) / WithExpectedAudience(AudienceWebd), and
// the viewlink.Minter stamps AudienceWebd on the payload — so a channelsd-issued
// signer over the same key mints links webd's own verifier accepts. This
// mirrors cmd/oap's buildChatViewMinter and channelsd's minter wiring; only the
// webd builtin-chat path was missing it, which silently swallowed live-view
// offers.
//
// Returns nil when keyBytes is empty (no passthroughlink signing key): browser
// view links are then genuinely unavailable, and the builtin sender surfaces
// that loudly rather than dropping the agent's offers silently.
func newArtifactViewMinter(keyBytes []byte, webdBaseURL func() string) channelkinds.ArtifactViewMinter {
	if len(keyBytes) == 0 {
		return nil
	}
	return &viewlink.Minter{
		Signer:      passthroughlink.New(keyBytes, passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd)),
		WebdBaseURL: webdBaseURL,
	}
}
