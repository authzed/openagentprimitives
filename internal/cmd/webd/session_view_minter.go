// webdSessionViewMinter is webd's implementation of
// channelkinds.SessionViewMinter for the built-in web chat's
// session_view_offer sub-channel sender. Unlike the artifact-view minter
// (newArtifactViewMinter), it needs no passthroughlink signing key: the
// session-view page enforces its own CheckInteract at open time, so this
// composes a durable plain-path URL directly from the live trusted origin —
// the same source TrustedOrigin() uses — via the shared
// channelkinds.ComposeSessionViewURL (also used by internal/cmd/channelsd's Slack
// minter and cmd/oap's TUI minter, so the URL shape is defined once).
package main

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// webdSessionViewMinter wraps a live trusted-origin getter. Needing no
// signer means it can always be constructed (unlike newArtifactViewMinter,
// which is nil whenever webd lacks a passthroughlink signing key).
type webdSessionViewMinter struct {
	trustedURLGet func() string
}

func newWebdSessionViewMinter(trustedURLGet func() string) *webdSessionViewMinter {
	return &webdSessionViewMinter{trustedURLGet: trustedURLGet}
}

func (m *webdSessionViewMinter) MintSessionViewLink(sessionRef string, _ identity.Principal, _ string) (string, error) {
	return channelkinds.ComposeSessionViewURL(m.trustedURLGet(), sessionRef)
}
