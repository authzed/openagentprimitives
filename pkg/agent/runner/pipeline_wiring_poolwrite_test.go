package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ToolWritesDecl must never be sourced from a CRD mapping. A spec author who
// could name a tool's write destination would be choosing where data lands.
func TestOnlyABuiltInDeclarationCanCarryAWriteDestination(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns-a", Name: "sess-a"}}

	assert.Nil(t, l.memoryPoolWritesDecl("some_mcp_tool"))
	require.NotNil(t, l.memoryPoolWritesDecl(meta.RecordObservationToolName))
	assert.Equal(t, "resource", l.memoryPoolWritesDecl(meta.RecordObservationToolName).DestinationArg)
}

// The declaration has to actually reach the hook. A built-in lookup nothing
// wires is a gate that never runs, and every pool-write test in
// pkg/authz/hooks would still be green.
func TestTheWriteDeclarationIsWiredIntoTheAudienceHook(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns-a", Name: "sess-a"}}

	d := l.infoLeakAudienceDeps()
	require.NotNil(t, d.LookupWrites, "the audience hook cannot gate a write it is never told about")

	got := d.LookupWrites(meta.RecordObservationToolName)
	require.NotNil(t, got)
	assert.Equal(t, hooks.ToolWritesDecl{DestinationArg: "resource"}, *got)
	assert.Nil(t, d.LookupWrites("some_mcp_tool"))
}
