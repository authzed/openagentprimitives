package all_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

func TestAllKindsRegistered_IncludesLineageAndDispatchSnapshot(t *testing.T) {
	for _, name := range []string{"lineage", "tool_dispatch_snapshot", "channel_msg_ref"} {
		_, ok := memory.LookupKind(name)
		assert.True(t, ok, "kind %q must be registered via kinds/all blank import", name)
	}
}

// The plan gate replays its authorization state by folding this kind, so a
// binary that reaches the gate without it registered would silently lose every
// approval and denial across a restart. Asserted HERE rather than in the kind's
// own package, where its init() would make the assertion vacuous.
func TestAllKindsRegistered_IncludesPlanGateAudit(t *testing.T) {
	k, ok := memory.LookupKind("plan_gate_audit")
	assert.True(t, ok, "kind %q must be registered via kinds/all blank import", "plan_gate_audit")
	if ok {
		assert.True(t, k.Retention().AppendOnly,
			"plan-gate records must stay append-only through the registry")
	}
}

// The runner writes this at compose time and the OPERATOR serves the memory
// API, so the kind has to be registered in every binary that touches the
// facade — an unregistered kind is rejected at Put, and the prompt record would
// be silently absent exactly when someone needs it. Asserted here rather than in
// the kind's own package, where its init() would make the assertion vacuous.
func TestAllKindsRegistered_IncludesSystemPrompt(t *testing.T) {
	k, ok := memory.LookupKind("system_prompt")
	assert.True(t, ok, "kind %q must be registered via kinds/all blank import", "system_prompt")
	if ok {
		assert.True(t, k.Retention().AppendOnly,
			"the instructions an agent ran under are evidence; they must stay append-only")
	}
}

// The runner writes this beside every request build, and a steelthread capture
// reads it to fill expect.toolOffered / toolNotOffered. A binary missing this
// registration answers every write with 400 "unknown Kind" — silently, since
// the runner's own write is best-effort and logged, not fatal — so the gap
// would surface only much later, as a session that turns out uncapturable.
func TestAll_RegistersToolCatalog(t *testing.T) {
	_, ok := memory.LookupKind("tool_catalog")
	assert.True(t, ok, "tool_catalog must be registered via the kinds/all blank import")
}

// channelsd writes this at the delivery that opens a triggered session, and a
// steelthread capture reads it to build the signed-webhook replay. A binary
// missing this registration answers every write with 400 "unknown Kind" —
// silently, since channelsd's own write is best-effort and logged, not fatal —
// so the gap would surface only much later, as a triggered session that turns
// out uncapturable.
func TestAll_RegistersTriggerDelivery(t *testing.T) {
	_, ok := memory.LookupKind("trigger_delivery")
	assert.True(t, ok, "trigger_delivery must be registered via the kinds/all blank import")
}
