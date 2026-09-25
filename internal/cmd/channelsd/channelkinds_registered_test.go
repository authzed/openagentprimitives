package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry/crdenumtest"
)

// TestChannelKindsRegistered guards that channelsd blank-imports every channel
// kind the CRD enum admits — the same guard internal/cmd/operator and cmd/oap
// already carry, on the third binary the delivery path runs through.
//
// Those Kinds self-register in init(), so a missing blank import is invisible
// at build time and costs a delivery at runtime — in three distinct ways, only
// the first of which existed when the operator's and the runner's copies of
// this test were written:
//
//   - The outbound relay resolves a human-directed envelope (an interaction
//     card and its resolutions) through registry.DeliversToHuman, which
//     ERRORS on an unregistered name rather than guessing. The relay drops the
//     envelope, so a permission prompt, a credential request, or a session-hold
//     release card reaches nobody — and the drop is logged against the lineage
//     walk, not against a wiring gap.
//   - senderResolver.resolveDeps looks the kind up to build a Sender; a miss
//     means every ordinary reply on that Channel is dropped too.
//   - Listener arming reads it at startup, so the kind never receives inbound
//     at all.
//
// channelsd resolves EVERY inbound Channel's kind through the registry,
// including the ones it does not host: browser, github and local are
// registered here precisely so the relay recognizes them as client-hosted and
// SKIPS them, rather than erroring "unknown kind" (see the comments on those
// imports in main.go). So a missing blank import does not merely disable one
// kind — it turns a deliberate skip into an error on a path that handles every
// other kind's traffic too.
//
// Derived from the shipped enum rather than a literal list, for the reason the
// operator's copy of this test records at length: its own hand-maintained list
// went stale, omitted "github", and kept passing for the entire time the
// operator never registered that kind and every kind=github Channel sat
// permanently Valid=False.
func TestChannelKindsRegistered(t *testing.T) {
	enum := crdenumtest.ChannelKindEnum(t)
	require.NotEmpty(t, enum, "spec.kind enum not found in the embedded install bundle")

	for _, kind := range enum {
		if _, ok := registry.Get(kind); !ok {
			t.Errorf("channel kind %q is not registered in the channelsd binary — add a blank import to internal/cmd/channelsd/main.go", kind)
		}
	}
}
