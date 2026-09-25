// channelsdArtifactViewMinter was the channelsd-local implementation of
// channelkinds.ArtifactViewMinter. It has been moved to the shared package
// pkg/platform/identity/passthroughlink/viewlink so `oap agent chat` can reuse it for
// TUI deep-links without duplicating the signing logic.
//
// This file is retained only to keep internal/cmd/channelsd/main.go's usage readable;
// the real implementation lives in viewlink.Minter.
package main

import (
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/viewlink"
)

// newArtifactViewMinter constructs the channelkinds.ArtifactViewMinter for
// channelsd from the per-process passthroughlink.Signer and the live webd
// base URL getter. The returned *viewlink.Minter satisfies the interface.
func newArtifactViewMinter(signer *passthroughlink.Signer, webdBaseURL func() string) *viewlink.Minter {
	return &viewlink.Minter{
		Signer:      signer,
		WebdBaseURL: webdBaseURL,
	}
}
