package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry/crdenumtest"
)

// TestChannelKindsRegistered guards that the operator blank-imports every
// channel kind the Channel controller may have to validate. Those Kinds
// self-register in init(); a missing blank import makes registry.Get miss and
// validate() fail the Channel at step 1 with `unknown kind "<name>"`.
//
// That failure is total and silent-looking: the Channel can never reach
// Valid=True, and channelsd gates listener arming on Valid=True
// (internal/cmd/channelsd/listeners.go), so the kind's listener never starts. We shipped
// exactly this — bento was registered in internal/cmd/channelsd and internal/cmd/runner but not
// here, so every bento cron Channel sat Valid=False/SpecInvalid and no
// scheduled session ever fired. Nothing failed at build time.
//
// Derived from the Channel CRD's shipped spec.kind enum
// (crdenumtest.ChannelKindEnum) rather than a second hand-maintained literal
// list — a literal list can itself go stale, which is exactly what happened
// here a second time: this test's own list was ["browser", "slack", "local",
// "bento", "fake"], omitting "github", so it kept passing for the entire
// time the operator never blank-imported the github kind and every
// kind=github Channel was permanently Valid=False in production. Comparing
// against the enum instead means a kind added to the CRD without a matching
// blank import here fails this test automatically — nobody has to remember
// to update a second list by hand. See
// pkg/channels/channelkinds/registry/crdenum_test.go's
// TestChannelKindEnumMatchesRegistry for the same fix applied to the enum
// itself; this test asks the mirror question ("is everything the enum names
// registered in THIS binary") rather than "do the enum and one binary's
// registry match exactly".
func TestChannelKindsRegistered(t *testing.T) {
	enum := crdenumtest.ChannelKindEnum(t)
	require.NotEmpty(t, enum, "spec.kind enum not found in the embedded install bundle")

	for _, kind := range enum {
		if _, ok := registry.Get(kind); !ok {
			t.Errorf("channel kind %q is not registered in the operator binary — add a blank import to internal/cmd/operator/main.go", kind)
		}
	}
}

// TestTriggerStatusReporterReachableFromTheOperator states the behavior behind
// those blank imports rather than the wiring, the same claim the runner's own
// test makes for its side.
//
// The AgentSession reconciler answers the trigger of a session that failed
// before its runner ever came up — the only party that can, because the agent
// is what did not start. It resolves the input binding's kind through this
// registry, so with no linked kind reporting trigger status that report is dead
// code here and a pull request goes on showing a review that will never arrive.
func TestTriggerStatusReporterReachableFromTheOperator(t *testing.T) {
	for _, k := range registry.All() {
		if _, ok := registry.TriggerStatusReporterFor(k.Name()); ok {
			return
		}
	}
	t.Error("no channel kind linked into the operator reports trigger status — a session that dies before its first turn can never answer the event that started it")
}
