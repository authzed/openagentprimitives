package fake

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// Compile-time check: *Kind satisfies the optional outbound-anchor interface.
// Declared for the reason session_owner.go declares its own — implementing the
// method without the assertion would let a later refactor drop it back off the
// interface silently.
var _ channelkinds.OutboundAnchorProvider = (*Kind)(nil)

// OutboundAnchor lets a fake Channel serve as the DESTINATION of a session
// whose inbound origin is a different Channel — a webhook, a cron tick.
//
// Without it the fake kind can stand in for every transport except that one.
// A split-channel session asks its role=output Channel where replies go
// (channelsd's inbound pipeline, and the Channel controller's own apply-time
// destination rules), and a kind that cannot answer makes the session
// undeliverable, so the pipeline refuses to create it at all. That is the right
// answer for a real transport with no configured destination; for the fake kind
// it was only ever a gap, because a fake Channel HAS no destination to
// configure — its sender appends to a process-global Driver keyed by
// (namespace, channel name) and reads neither the key nor the external map.
//
// The gap is invisible in a same-Channel scenario (role=both is its own origin
// and destination and never asks), which is why every fake bundle written
// before a captured trigger bundle existed passed without it.
//
// Pure function of ch, as the interface requires: the Channel's own name is the
// only identifier a fake Channel has, and it is stable across re-derivations.
// The key mirrors slack's bare-channel anchor shape and the external map uses
// this kind's own "channel_id" convention (see ChannelViewSubjectRef), so a
// split-channel fake pair behaves the way the real transport does rather than
// in a shape unique to this method.
//
// Errors only on a nil or unnamed Channel — there is no field a human could be
// told to fill in, which is exactly the difference from slack's version.
func (Kind) OutboundAnchor(ch *spiceboxv1alpha1.Channel) (string, map[string]string, error) {
	if ch == nil || ch.Name == "" {
		return "", nil, fmt.Errorf("a fake Channel anchors on its own metadata.name, and this one has none")
	}
	return "channel:" + ch.Name, map[string]string{"channel_id": ch.Name}, nil
}
