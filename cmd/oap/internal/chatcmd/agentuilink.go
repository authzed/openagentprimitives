// Best-effort agent-UI link minter for
// `oap agent chat`. Reads webd's trusted base URL once at chat startup, via the
// same readWebdTrustedBaseURL the session-view minter uses, and needs no
// passthroughlink signing key: the agent-UI shell carries no signed capability
// — it authorizes at open time — so its URL is durable and safe to bake into a
// message. When the ConfigMap can't be read at all the minter stays nil and
// the caller (startSession) prints a startup notice; the agent_ui_offer
// sub-channel sender also surfaces a nil minter at the moment an offer needs
// one, so the notice is an early warning rather than the only signal.
package chatcmd

import (
	"context"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// tuiAgentUIMinter implements channelkinds.AgentUIMinter for the TUI. It wraps
// a frozen base-URL string (read once at chat startup, which is sufficient for
// a single chat session — the same read-once shape buildChatSessionViewMinter
// and buildChatViewMinter use) and composes via the shared
// channelkinds.ComposeAgentUIURL, so the shell's URL shape is defined in
// exactly one place across `oap`, channelsd and webd.
type tuiAgentUIMinter struct {
	webdBaseURL string
}

func (m *tuiAgentUIMinter) MintAgentUILink(sessionRef string) (string, error) {
	return channelkinds.ComposeAgentUIURL(m.webdBaseURL, sessionRef)
}

// buildChatAgentUIMinter tries to construct the agent-UI link minter for the
// TUI. Returns (minter, "") on success and (nil, reason) when the webd
// external-URL ConfigMap can't be read at all.
//
// Like buildChatSessionViewMinter, a missing/empty URL *inside* an otherwise
// readable ConfigMap is not a build failure: the minter is still constructed
// (with an empty webdBaseURL), because channelkinds.ComposeAgentUIURL's own
// ("", nil) clean-skip contract is what lets the agent_ui_offer sender surface
// "not configured yet" at the moment an offer actually needs the URL, rather
// than at startup where it may never matter.
func buildChatAgentUIMinter(ctx context.Context, b *kube.Bundle) (channelkinds.AgentUIMinter, string) {
	base, reason := readWebdTrustedBaseURL(ctx, b)
	if reason != "" {
		// A true nil interface, not a typed-nil *tuiAgentUIMinter: the Deps
		// field this feeds is an interface, and a typed nil there would pass
		// every `!= nil` guard and panic on the first mint.
		return nil, reason
	}
	return &tuiAgentUIMinter{webdBaseURL: base}, ""
}
