// agentUIMinter is channelsd's implementation of channelkinds.AgentUIMinter.
// Like sessionViewMinter — and unlike the artifact-view minter, which mints a
// signed, time-limited deep-link — the agent-UI shell is a durable link with
// no capability in it: the page authorizes at open time, so nothing here is
// signed and the URL is safe to bake into a message at post time.
//
// It is a separate type from sessionViewMinter rather than a second method on
// it: one struct per page, so a page's URL shape, its doc, and its wiring stay
// in one place and neither page can be changed by an edit aimed at the other.
package main

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// agentUIMinter composes the agent-UI shell page URL. Satisfies
// channelkinds.AgentUIMinter (matched by method signature).
type agentUIMinter struct {
	// WebdBaseURL is a live getter for webd's externally reachable trusted
	// base URL — the same source newSessionViewMinter and newArtifactViewMinter
	// read. Returns "" when webd is not yet configured; MintAgentUILink returns
	// ("", nil) in that case, a clean skip for callers.
	WebdBaseURL func() string
}

// newAgentUIMinter constructs the channelkinds.AgentUIMinter for channelsd
// from the live webd base URL getter (spiceboxv1alpha1.WebdExternalURLConfigMap
// / WebdTrustedURLKey). No signer is needed: the agent-UI link carries no
// capability to sign.
func newAgentUIMinter(webdBaseURL func() string) *agentUIMinter {
	return &agentUIMinter{WebdBaseURL: webdBaseURL}
}

// MintAgentUILink returns the shell page URL for the given sessionRef
// ("ns/name"), composed by the shared channelkinds.ComposeAgentUIURL so the
// query form — required for a namespace named "api", see that function — is
// defined in exactly one place. Returns ("", nil) when the webd base URL is
// empty (webd not yet installed, or its ConfigMap not yet populated), which
// callers treat as a clean skip.
//
// The nil-receiver / nil-getter guard returns an error rather than an empty
// URL: an unconfigured base URL is a normal startup state, but a minter that
// was never constructed is a wiring bug, and the two must not be reported the
// same way.
func (m *agentUIMinter) MintAgentUILink(sessionRef string) (string, error) {
	if m == nil || m.WebdBaseURL == nil {
		return "", fmt.Errorf("agent UI minter not configured")
	}
	return channelkinds.ComposeAgentUIURL(m.WebdBaseURL(), sessionRef)
}
