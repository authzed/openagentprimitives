//go:build integration

package spicedb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestIntegration_WebActionApproval_DenyApproveRecheck proves E9's approval
// write CLOSES THE LOOP against a real SpiceDB: a browser MUTATE that E8 denies
// (coarse web_host grant held, but no per-action web_action grant) is routed to
// a human; on approval the runner mints the SAME caveated web_action grant
// pkg/agent/runner.webActionGrantBinding builds (pagestate_matches +
// {approved_fingerprint: <the fingerprint presented at the denied check>}); and
// the subsequent acting call presenting that SAME fingerprint now PASSES — while
// a DIFFERENT fingerprint (the page changed between approval and the next call)
// still denies. That mismatch half is the TOCTOU property the whole gate exists
// for, and only a live server evaluates the caveat.
//
// The binding written here is byte-for-byte what webActionGrantBinding mints
// (pkg/agent/runner/web_action_approval.go) — pkg/authz/spicedb cannot import
// the runner (cycle), so the shape is constructed inline and pinned equal to
// browserschema's constants, the same way the E2/E8 integration tests do.
func TestIntegration_WebActionApproval_DenyApproveRecheck(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	// web_host (coarse, uncaveated) + web_action (per-action, pagestate_matches)
	// as ComposeSlots emits them for a class declaring BOTH slots — same fragment
	// the E8 two-layer integration test uses.
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webHostAndActionComposedFragment)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const ns, sessName, host = "ns", "s1", "aa.com"
	const actionKeyRaw = "aa.com/click/place-order-abcd1234"
	const approvedFP, divergedFP = "fp-approved", "fp-diverged"
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	// The coarse web_host grant is ALREADY held — it is what the plan-gate phase
	// approval wrote (Phase C). The per-action web_action grant is what E9 writes
	// on the mutate's own approval, and it is ABSENT at the start.
	webHostID, err := authz.NewObjectID(host, browserschema.WebHostValueTransforms())
	require.NoError(t, err, "derive web_host id")
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{
			ResourceType: browserschema.WebHostType,
			ResourceID:   webHostID,
			Permission:   browserschema.ActPermission,
		}}, time.Now().Add(time.Hour)), "seed the coarse web_host grant")

	checker := toolcheck.Checker{Cli: c}
	// The acting tool's REAL Permission() — External on web_host; the web_action
	// layer rides on Inputs exactly as BuildInputs wires it.
	actPerm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:         browserschema.WebHostType,
			Permission:           browserschema.ActPermission,
			ResourceIDTemplate:   "{host}",
			ResourceIDTransforms: browserschema.WebHostValueTransforms(),
		},
	}
	mutate := func(fp string) authz.Inputs {
		return authz.Inputs{
			Subject:                  "u-1",
			Args:                     map[string]any{"host": host},
			AgentSessionRef:          ns + "/" + sessName,
			SlotResourceTypes:        []string{browserschema.WebHostType, browserschema.WebActionType},
			WebActionRequired:        true,
			WebActionKey:             actionKeyRaw,
			PresentedPageFingerprint: fp,
		}
	}

	// 1) DENY — the coarse grant is not enough for a mutate, and no web_action
	// grant exists yet. This is the deny that routes the call to a human.
	denyRes := checker.CheckToolCall(ctx, actPerm, mutate(approvedFP))
	require.Equal(t, authz.OutcomeDenied, denyRes.Outcome,
		"a mutate with no per-action grant must deny → human approval")
	assert.Contains(t, denyRes.Message, "approval", "the web_action deny routes to approval")

	// 2) APPROVE — the runner mints the caveated web_action grant on the EXACT
	// fingerprint presented at the denied check (what the human saw). This inline
	// binding mirrors pkg/agent/runner.webActionGrantBinding(actionKeyRaw, approvedFP).
	waID, err := authz.NewObjectID(actionKeyRaw, browserschema.WebActionValueTransforms())
	require.NoError(t, err, "derive web_action id")
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{
			ResourceType:  browserschema.WebActionType,
			ResourceID:    waID,
			Permission:    browserschema.ActPermission,
			CaveatName:    browserschema.PagestateMatchesCaveat,
			CaveatContext: map[string]any{browserschema.ApprovedFingerprintParam: approvedFP},
		}}, time.Now().Add(time.Hour)), "E9 approval write: the caveated web_action grant")

	// 3) RE-CHECK, SAME fingerprint — the loop is closed: the subsequent acting
	// call now PASSES.
	assert.Equal(t, authz.OutcomeAllowed, checker.CheckToolCall(ctx, actPerm, mutate(approvedFP)).Outcome,
		"after approval, the same mutate against the SAME page fingerprint must be allowed")

	// 4) DIFFERENT fingerprint — the page changed between approval and this call.
	// The grant exists, but the pagestate_matches caveat refuses: TOCTOU gate.
	assert.Equal(t, authz.OutcomeDenied, checker.CheckToolCall(ctx, actPerm, mutate(divergedFP)).Outcome,
		"a changed page (different fingerprint) must still deny even though the grant exists")

	// Pin the inline binding's vocabulary equal to browserschema's constants, so
	// this test and the runner's webActionGrantBinding cannot name two different
	// caveats or parameters.
	assert.Equal(t, "pagestate_matches", browserschema.PagestateMatchesCaveat)
	assert.Equal(t, "approved_fingerprint", browserschema.ApprovedFingerprintParam)
}
