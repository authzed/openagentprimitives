package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// TestLeakageGateApplies codifies the kind-gating predicate: only
// KindMeta (internal control-plane tools — new_operation, agent_complete,
// respond_to_user, etc.) bypass the read-side info-leakage gate.
// Everything else (MCP, sandbox, any future external-data kind) is
// gated. Internal tools are special: they don't fetch external
// SpiceDB-keyed resources into context, so requiring an explicit
// toolResourceMap entry for each would be nonsensical and break every
// session in enforcing mode.
func TestLeakageGateApplies(t *testing.T) {
	assert.True(t, leakageGateApplies(tool.KindMCP), "MCP tools are gated")
	assert.True(t, leakageGateApplies(tool.KindSandbox), "sandbox tools are gated (they can read user data)")
	assert.True(t, leakageGateApplies(tool.Kind("unknown")), "unknown kinds default to gated (fail-closed)")
	assert.False(t, leakageGateApplies(tool.KindMeta), "internal meta tools bypass the gate")
}

// TestIdleYieldEventFor_SharedeniedVsPlainIdle pins the event chosen at the
// terminal IdleExit site: a share-denied yield records the distinct
// ShareDeniedYield; every other idle exit records IdleYield.
func TestIdleYieldEventFor_SharedeniedVsPlainIdle(t *testing.T) {
	assert.IsType(t, lifecyclecore.ShareDeniedYield{}, idleYieldEventFor(true),
		"a share-denied idle exit records ShareDeniedYield")
	assert.IsType(t, lifecyclecore.IdleYield{}, idleYieldEventFor(false),
		"a plain (await TTL / cancel) idle exit records IdleYield")
}

// TestShareDeniedYield_RecordsDistinctIdleAuditEvent verifies end-to-end at the
// sequencer level that a share-denied idle yield lands a ShareDeniedYield event
// in the durable lifecycle log (NOT a plain IdleYield), so the audit trail can
// tell a blocked-share yield apart from an await-idle — while still folding to
// phase Idle.
func TestShareDeniedYield_RecordsDistinctIdleAuditEvent(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := memory.NamespacedName{Namespace: "ns-leak", Name: "sess-share-denied"}
	mem := buildLifecycleMemory()

	// The prior run reached Running; then respond_to_user's share-denied yield
	// fires (the runner records idleYieldEventFor(r.ShareDenied)).
	appendLifecycleEvent(t, mem, key, lifecyclecore.RunnerClaimed{})

	l := &Loop{SessionKey: key, LifecycleMemory: mem}
	l.emitLifecycleEvent(ctx, idleYieldEventFor(true))

	events, err := lifecycle.Events(ctx, mem, memory.Scope{Kind: "session", ID: key.Namespace + "/" + key.Name})
	require.NoError(t, err)
	var sawShareDenied, sawIdleYield bool
	for _, ev := range events {
		switch ev.(type) {
		case lifecyclecore.ShareDeniedYield:
			sawShareDenied = true
		case lifecyclecore.IdleYield:
			sawIdleYield = true
		}
	}
	assert.True(t, sawShareDenied, "share-denied yield must record a ShareDeniedYield event")
	assert.False(t, sawIdleYield, "share-denied yield must NOT record a plain IdleYield")
	assert.Equal(t, lifecyclecore.PhaseIdle, lifecyclecore.Fold(events).Phase,
		"a share-denied yield still resolves to phase Idle")
}
