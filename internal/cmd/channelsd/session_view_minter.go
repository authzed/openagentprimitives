// sessionViewMinter is channelsd's implementation of
// channelkinds.SessionViewMinter. Unlike the artifact-view minter (a signed,
// time-limited deep-link minted via passthroughlink.Signer + viewlink.Minter),
// the session-view page is a durable plain-path link: authorization happens
// at open time via the page's own CheckInteract (agentsession#interact), so
// this minter does no signing at all — it just composes
// "<trusted-base>/session-view/<ns>/<name>" from the same live webd base URL
// getter newArtifactViewMinter uses, via the shared
// channelkinds.ComposeSessionViewURL (also used by cmd/oap's TUI minter and
// internal/cmd/webd's built-in chat minter, so the URL shape is defined once).
package main

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// sessionViewMinter composes the durable session-view page URL. Satisfies
// channelkinds.SessionViewMinter (matched by method signature).
type sessionViewMinter struct {
	// WebdBaseURL is a live getter for webd's externally reachable trusted
	// base URL. Returns "" when webd is not yet configured; MintSessionViewLink
	// returns ("", nil) in that case — a clean skip for callers.
	WebdBaseURL func() string
}

// newSessionViewMinter constructs the channelkinds.SessionViewMinter for
// channelsd from the live webd base URL getter — the same source
// newArtifactViewMinter uses (spiceboxv1alpha1.WebdExternalURLConfigMap /
// WebdTrustedURLKey). No signer is needed: the session-view link carries no
// capability to sign.
func newSessionViewMinter(webdBaseURL func() string) *sessionViewMinter {
	return &sessionViewMinter{WebdBaseURL: webdBaseURL}
}

// MintSessionViewLink returns "<trusted-base>/session-view/<ns>/<name>" for
// the given sessionRef ("ns/name"). subject and backLink are accepted for
// interface parity with channelkinds.SessionViewMinter (and
// ArtifactViewMinter) but are deliberately NOT embedded in the URL — the
// session-view page is a durable plain-path link, not a signed capability;
// CheckInteract at open time is the sole authorization boundary. Returns
// ("", nil) when the webd base URL is empty (webd not yet installed or the
// ConfigMap not yet populated) — callers treat an empty URL as a clean skip.
func (m *sessionViewMinter) MintSessionViewLink(sessionRef string, _ identity.Principal, _ string) (string, error) {
	if m == nil || m.WebdBaseURL == nil {
		return "", fmt.Errorf("session view minter not configured")
	}
	return channelkinds.ComposeSessionViewURL(m.WebdBaseURL(), sessionRef)
}
