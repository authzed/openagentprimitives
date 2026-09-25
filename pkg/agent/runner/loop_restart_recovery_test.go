package runner

// loop_restart_recovery_test.go covers two restart-recovery sub-features
// (verified at the sequencer level, no full Run() required):
//
//  1. revoked-origin-denied-after-respawn: a tool origin revoked in a prior
//     run (recorded via EmitRevoked in the lifecycle log) is re-applied to
//     RevokedOrigins by claimAndRecover, so the revocation_guard PreToolCall
//     hook still denies calls on the respawned runner.
//
//  2. open-decision-rearmed-on-restart: a session whose lifecycle log contains
//     a DecisionAsked without a matching DecisionResolved (the prior runner
//     crashed mid-approval) gets its pending decision re-armed in the approval
//     orchestrator by claimAndRecover, so a subsequent DeliverDecision reaches
//     the re-arm goroutine instead of being silently dropped.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/credential"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// buildLifecycleMemory returns a fresh in-process memory facade with the
// lifecycle kind registered, ready for claimAndRecover tests.
func buildLifecycleMemory() *memory.Local {
	return memory.NewLocal(inmem.NewBackend())
}

// appendLifecycleEvent is a test helper that appends ev to the lifecycle log
// for the given session key. No signing facade is wired, so the append-only
// enforcement passes (provVerify is nil in tests).
func appendLifecycleEvent(t *testing.T, mem *memory.Local, key memory.NamespacedName, ev lifecyclecore.Event) {
	t.Helper()
	scope := memory.Scope{Kind: "session", ID: key.Namespace + "/" + key.Name}
	err := lifecycle.Append(memory.WithSystemApproval(context.Background(), "test"), mem, scope, ev, time.Now().UTC(), lifecycle.OrderKey{})
	require.NoError(t, err, "lifecycle.Append must succeed in test")
}

// TestRestartRecovery_RevokedOriginDeniedAfterRespawn seeds a lifecycle log
// with a Revoked{Kind:"tool-origin", Key:"mcpserver/alice"} entry (simulating
// what EmitRevoked writes when the operator revokes an MCP server), then calls
// claimAndRecover on a fresh Loop (simulating a runner respawn). It asserts
// that the fresh RevokedOrigins set now reports "mcpserver/alice" as revoked.
func TestRestartRecovery_RevokedOriginDeniedAfterRespawn(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := memory.NamespacedName{Namespace: "ns-restart", Name: "sess-revoke"}
	mem := buildLifecycleMemory()

	// Seed the lifecycle log exactly as the old runner would have written it:
	// RunnerClaimed marks the start of the prior run, then a Revoked event
	// carries the tool-origin key, then RunnerTerminal ends it.
	appendLifecycleEvent(t, mem, key, lifecyclecore.SettingsAccepted{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.RunnerClaimed{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{
		Kind: "tool-origin",
		Key:  "mcpserver/alice",
	})
	// Simulate another revocation (different origin) to verify accumulation.
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{
		Kind: "tool-origin",
		Key:  "mcpserver/bob",
	})
	// A Revoked entry with empty Kind (old format pre-fix) must be skipped.
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{})

	// Fresh runner: RevokedOrigins starts empty (the prior runner's in-process
	// set is gone), LifecycleMemory points at the same durable log. internal/cmd/runner
	// threads the same *toolorigin.Set into both the guard hook and the registry
	// that claimAndRecover re-applies through, so the fixture does too.
	revokedOrigins := toolorigin.New()
	reg := revocation.NewRegistry()
	require.NoError(t, reg.Register(revokedOrigins))
	l := &Loop{
		SessionKey:         key,
		LifecycleMemory:    mem,
		RevokedOrigins:     revokedOrigins,
		RevocationRegistry: reg,
	}

	l.claimAndRecover(ctx)

	// Both named origins must be re-applied.
	assert.True(t, revokedOrigins.IsRevoked("mcpserver/alice"),
		"revoked origin from lifecycle log must be re-applied on respawn")
	assert.True(t, revokedOrigins.IsRevoked("mcpserver/bob"),
		"all revoked origins from lifecycle log must be re-applied on respawn")
	// The empty-Kind entry must not cause any side effects.
	assert.False(t, revokedOrigins.IsRevoked(""),
		"empty-key Revoked event must not be re-applied")
}

// recordingCredInvalidator captures the Secrets a re-applied credential
// revocation invalidates.
type recordingCredInvalidator struct{ got []string }

func (r *recordingCredInvalidator) InvalidateSecret(ns, name string) error {
	r.got = append(r.got, ns+"/"+name)
	return nil
}

// The lifecycle log records EVERY revocable kind (EmitRevoked writes kind+key
// verbatim), but claimAndRecover only ever re-applied "tool-origin" — it matched
// on that literal string and skipped everything else. A credential revoked
// during the prior run therefore came back to life on a runner restart: the
// fresh broker cache re-resolved it and the fresh MCPTools re-froze it.
//
// The fix routes replayed Revoked events through the same revocation.Registry
// the live subscriber uses, so a new revocable kind needs no edit here — which
// is what pkg/authz/revocation's package doc already promised ("adding a revocable
// kind = register one Invalidator + a publisher trigger; no consumer edits").
func TestRestartRecovery_RevokedCredentialReAppliedAfterRespawn(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := memory.NamespacedName{Namespace: "ns-restart", Name: "sess-cred-revoke"}
	mem := buildLifecycleMemory()

	appendLifecycleEvent(t, mem, key, lifecyclecore.SettingsAccepted{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.RunnerClaimed{})
	// The prior runner recorded BOTH kinds. Only tool-origin was ever replayed.
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{Kind: "tool-origin", Key: "mcpserver/alice"})
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{Kind: "credential", Key: "id-ns/gh-pat"})
	// Old-format entry (empty Kind) must still be skipped.
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{})
	// An unknown kind must be skipped, not panic — a newer runner may have
	// written a kind this binary does not register.
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{Kind: "kind-from-the-future", Key: "x/y"})

	revokedOrigins := toolorigin.New()
	credInv := &recordingCredInvalidator{}

	reg := revocation.NewRegistry()
	require.NoError(t, reg.Register(revokedOrigins))
	require.NoError(t, reg.Register(credential.New(credInv)))

	l := &Loop{
		SessionKey:         key,
		LifecycleMemory:    mem,
		RevokedOrigins:     revokedOrigins,
		RevocationRegistry: reg,
	}

	l.claimAndRecover(ctx)

	assert.True(t, revokedOrigins.IsRevoked("mcpserver/alice"),
		"tool-origin re-application must not regress")
	assert.Equal(t, []string{"id-ns/gh-pat"}, credInv.got,
		"a credential revoked in the prior run must be re-applied on respawn")
}

// A Loop with no registry (a session that never wired revocation) must not panic
// on a log that contains Revoked events.
func TestRestartRecovery_NilRevocationRegistrySkipsReApply(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := memory.NamespacedName{Namespace: "ns-restart", Name: "sess-no-registry"}
	mem := buildLifecycleMemory()

	appendLifecycleEvent(t, mem, key, lifecyclecore.RunnerClaimed{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.Revoked{Kind: "credential", Key: "id-ns/gh-pat"})

	l := &Loop{SessionKey: key, LifecycleMemory: mem}
	assert.NotPanics(t, func() { l.claimAndRecover(ctx) })
}

// TestRestartRecovery_OpenDecisionRearmedOnRestart seeds a lifecycle log with
// a DecisionAsked (no matching DecisionResolved), then calls claimAndRecover
// on a fresh Loop with an approval orchestrator wired. It asserts that:
//  1. The orchestrator has a pending entry for the old requestID immediately
//     after claimAndRecover (the re-arm goroutine called Await).
//  2. DeliverDecision("r1", ...) unblocks the re-arm goroutine.
//  3. After the decision is delivered, the pending entry is removed from the
//     orchestrator (the goroutine completed).
func TestRestartRecovery_OpenDecisionRearmedOnRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 5*time.Second)
	defer cancel()

	key := memory.NamespacedName{Namespace: "ns-restart", Name: "sess-decision"}
	mem := buildLifecycleMemory()
	sessionRef := key.Namespace + "/" + key.Name

	// Seed: prior runner asked for approval on r1 but crashed before it resolved.
	appendLifecycleEvent(t, mem, key, lifecyclecore.SettingsAccepted{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.RunnerClaimed{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.DecisionAsked{
		RequestID: "r1",
		Kind:      lifecyclecore.DecisionToolCall,
	})

	orch := approval.New()
	l := &Loop{
		SessionKey:      key,
		LifecycleMemory: mem,
		Approval:        orch,
	}

	l.claimAndRecover(ctx)

	// Give the re-arm goroutine a moment to call Approval.Await and register
	// with the orchestrator. A 100 ms budget is generous for an in-process no-op.
	deadline := time.Now().Add(200 * time.Millisecond)
	var pending int
	for time.Now().Before(deadline) {
		pending = orch.PendingForSession(sessionRef)
		if pending == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 1, pending,
		"orchestrator must have exactly 1 pending entry for the re-armed requestID after claimAndRecover")
	assert.Equal(t, []string{"r1"}, orch.PendingRequestIDsForSession(sessionRef),
		"the pending entry must be for requestID r1")

	// Deliver a decision for the old requestID — this simulates the channel
	// delivering the user's response to the pre-crash approval request.
	orch.DeliverDecision("r1", approval.Decision{Approved: true, ApproverID: "user:alice"})

	// Wait for the re-arm goroutine to consume the decision and complete.
	deadline = time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if orch.PendingForSession(sessionRef) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, 0, orch.PendingForSession(sessionRef),
		"orchestrator pending count must reach 0 after DeliverDecision unblocks the re-arm goroutine")
}

// TestRestartRecovery_MultipleOpenDecisionsAllRearmed verifies that when the
// lifecycle log has two open decisions (both asked, neither resolved), both
// are re-armed and both can be independently delivered.
func TestRestartRecovery_MultipleOpenDecisionsAllRearmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 5*time.Second)
	defer cancel()

	key := memory.NamespacedName{Namespace: "ns-restart", Name: "sess-multi"}
	mem := buildLifecycleMemory()
	sessionRef := key.Namespace + "/" + key.Name

	appendLifecycleEvent(t, mem, key, lifecyclecore.SettingsAccepted{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.RunnerClaimed{})
	appendLifecycleEvent(t, mem, key, lifecyclecore.DecisionAsked{RequestID: "r1", Kind: lifecyclecore.DecisionToolCall})
	appendLifecycleEvent(t, mem, key, lifecyclecore.DecisionAsked{RequestID: "r2", Kind: lifecyclecore.DecisionContentInspect})

	orch := approval.New()
	l := &Loop{
		SessionKey:      key,
		LifecycleMemory: mem,
		Approval:        orch,
	}

	l.claimAndRecover(ctx)

	// Both decisions must be registered in the orchestrator.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if orch.PendingForSession(sessionRef) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 2, orch.PendingForSession(sessionRef),
		"orchestrator must have both pending entries after claimAndRecover")

	// Deliver both decisions.
	orch.DeliverDecision("r1", approval.Decision{Approved: true})
	orch.DeliverDecision("r2", approval.Decision{Approved: false})

	// Both goroutines must complete.
	deadline = time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if orch.PendingForSession(sessionRef) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, 0, orch.PendingForSession(sessionRef),
		"all re-arm goroutines must complete after both decisions are delivered")
}

// TestRestartRecovery_NilApprovalSkipsRearm verifies that claimAndRecover does
// not panic when Approval is nil — it logs and skips the re-arm step.
func TestRestartRecovery_NilApprovalSkipsRearm(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := memory.NamespacedName{Namespace: "ns-restart", Name: "sess-noapproval"}
	mem := buildLifecycleMemory()

	appendLifecycleEvent(t, mem, key, lifecyclecore.DecisionAsked{RequestID: "r1", Kind: lifecyclecore.DecisionToolCall})

	l := &Loop{
		SessionKey:      key,
		LifecycleMemory: mem,
		Approval:        nil, // not wired
	}

	// Must not panic.
	require.NotPanics(t, func() { l.claimAndRecover(ctx) })
}

// TestRestartRecovery_NoLogRevokedOriginsUntouched verifies that when
// LifecycleMemory is nil (kubectl/test path), RevokedOrigins is not touched.
func TestRestartRecovery_NoLogRevokedOriginsUntouched(t *testing.T) {
	revokedOrigins := toolorigin.New()
	_ = revokedOrigins.Invalidate("mcpserver/existing") // pre-existing

	l := &Loop{
		SessionKey:      memory.NamespacedName{Namespace: "default", Name: "sess-nolog"},
		LifecycleMemory: nil, // no log wired
		RevokedOrigins:  revokedOrigins,
	}
	l.claimAndRecover(memory.WithSystemApproval(context.Background(), "test"))

	// Pre-existing entry must be preserved; no new entries must appear.
	assert.True(t, revokedOrigins.IsRevoked("mcpserver/existing"),
		"pre-existing revoked origin must not be cleared by claimAndRecover")
}
