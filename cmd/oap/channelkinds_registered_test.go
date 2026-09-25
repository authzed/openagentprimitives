package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry/crdenumtest"
)

// TestChannelKindsRegistered guards that oap blank-imports every channel kind it
// offers on the command line. `oap channel create` resolves the kind through
// registry.Get and lists the choices via registry.All, so a missing blank
// import makes the kind unreachable from the CLI and invisible in the menu —
// including any wizard that kind ships.
//
// We shipped exactly this: pkg/channels/channelkinds/bento had a complete Wizard that no
// user could reach, because oap never registered the kind.
//
// Derived from the Channel CRD's shipped spec.kind enum
// (crdenumtest.ChannelKindEnum) rather than a hand-maintained literal list —
// see internal/cmd/operator/channelkinds_registered_test.go's doc for why: a
// hand-written list is a second fact that can go stale independently of the
// registry it is meant to guard.
func TestChannelKindsRegistered(t *testing.T) {
	enum := crdenumtest.ChannelKindEnum(t)
	require.NotEmpty(t, enum, "spec.kind enum not found in the embedded install bundle")

	for _, kind := range enum {
		if _, ok := registry.Get(kind); !ok {
			t.Errorf("channel kind %q is not registered in the oap binary — "+
				"add a blank import to cmd/oap/internal/channelcmd/create.go", kind)
		}
	}
}
