//go:build integration

package spicedb

import (
	"context"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// webActionComposedFragment is the web_action definition as ComposeSlots emits
// it for a declared web_action#act slot: slot_grant_act caveated `with
// pagestate_matches and expiration`, the one thing distinguishing it from
// web_host's uncaveated grant. The unit test
// TestComposeSlots_CaveatsWebActionGrantOnlyNotOtherSlots (pkg/authz/guardian/
// schema) proves ComposeSlots produces exactly this; this integration test then
// proves a real SpiceDB ENFORCES the pagestate_matches caveat on the grant.
//
// pagestate_matches itself is in the canonical scaffold Schema (E1), so
// WriteSchemaWithExtra resolves the caveat reference.
const webActionComposedFragment = `
definition web_action {
    relation slot_grant_act: agentsession with pagestate_matches and expiration
    relation owner: user
    permission act = slot_grant_act->interact + owner
}
`

// The acting-MUTATE round-trip against a real SpiceDB. A caveated web_action
// slot grant pins the page fingerprint approved at bind time
// ({approved_fingerprint: "fp-A"}); the acting check presents the LIVE fingerprint
// and the grant resolves only while the two match. This is Phase E's TOCTOU
// revalidation enforced end to end — compiler.Compile cannot test caveat
// EVALUATION, only a live server can.
//
// Five cases, covering both the toolcheck.checkExternalSlotGrant path (which always
// supplies the arg) and the raw SpiceDB Permissionship (including the ABSENT-arg →
// CONDITIONAL case the checker never produces but must map to deny):
//
//  1. present fp-A (matches)        → ALLOWED      (HAS_PERMISSION)
//  2. present fp-B (mismatch)       → DENIED       (NO_PERMISSION)
//  3. present ""   (caveat guard)   → DENIED       (NO_PERMISSION via != "")
//  4. NO presented arg              → raw CONDITIONAL, mapped to DENY
//  5. different action id (no grant)→ DENIED       (NO_PERMISSION)
func TestIntegration_WebActionSlotGrant_PagestateCaveat_RoundTrip(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webActionComposedFragment)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const ns, sessName, host = "ns", "s1", "aa.com"
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	// The grant id as the bind path mints it: NewObjectID over web_action's
	// published transform chain (byte-for-byte web_host's — spicedb_escape).
	grantID, err := authz.NewObjectID(host, browserschema.WebActionValueTransforms())
	require.NoError(t, err, "derive the web_action object id")

	// Write the CAVEATED grant through the real GrantSlots writer: this is the E2
	// write path, carrying pagestate_matches + the approved fingerprint frozen at
	// bind time. A web_action grant written WITHOUT the caveat would be refused by
	// SpiceDB (the relation is declared `with pagestate_matches`).
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{
			ResourceType:  browserschema.WebActionType,
			ResourceID:    grantID,
			Permission:    browserschema.ActPermission,
			CaveatName:    "pagestate_matches",
			CaveatContext: map[string]any{"approved_fingerprint": "fp-A"},
		}}, time.Now().Add(time.Hour)), "GrantSlots (caveated)")

	// *Client satisfies toolcheck.Client (CheckPermission) directly, so the real
	// external-check path runs against real SpiceDB. The Check carries the SAME
	// escape chain the acting tool's Permission() sets.
	checker := toolcheck.Checker{Cli: c}
	actPerm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:         browserschema.WebActionType,
			Permission:           browserschema.ActPermission,
			ResourceIDTemplate:   "{host}",
			ResourceIDTransforms: browserschema.WebActionValueTransforms(),
		},
	}
	inputsFor := func(fp, h string) authz.Inputs {
		return authz.Inputs{
			Subject:                  "u-1",
			Args:                     map[string]any{"host": h},
			AgentSessionRef:          ns + "/" + sessName,
			SlotResourceTypes:        []string{browserschema.WebActionType},
			PresentedPageFingerprint: fp,
		}
	}

	// CASE 1: the live fingerprint matches the one approved → ALLOWED.
	assert.Equal(t, authz.OutcomeAllowed, checker.CheckToolCall(ctx, actPerm, inputsFor("fp-A", host)).Outcome,
		"a web_action grant must permit acting while the page fingerprint still matches")

	// CASE 2: the page changed under the agent — fingerprint no longer matches → DENIED.
	assert.Equal(t, authz.OutcomeDenied, checker.CheckToolCall(ctx, actPerm, inputsFor("fp-B", host)).Outcome,
		"a changed page (mismatched fingerprint) must deny and route to a human — the TOCTOU gate")

	// CASE 3: an empty presented fingerprint is refused by the caveat's own
	// `presented_fingerprint != ""` guard → DENIED (never a silent allow on "").
	assert.Equal(t, authz.OutcomeDenied, checker.CheckToolCall(ctx, actPerm, inputsFor("", host)).Outcome,
		"an empty presented fingerprint must deny via the caveat guard")

	// CASE 5: a different action id has no grant at all → DENIED.
	assert.Equal(t, authz.OutcomeDenied, checker.CheckToolCall(ctx, actPerm, inputsFor("fp-A", "bb.com")).Outcome,
		"an action on a host with no grant must deny regardless of the presented fingerprint")

	// CASE 4 (THE critical one) is at the raw SpiceDB Permissionship level, because
	// checkExternalSlotGrant ALWAYS supplies the arg and so never itself produces a
	// CONDITIONAL. We prove both halves: (a) SpiceDB returns CONDITIONAL when the
	// arg is ABSENT, and (b) the raw Permissionship for the four supplied-arg cases
	// matches what the checker decided above.
	rawCheck := func(fpCtx *structpb.Struct, objID string) *v1.CheckPermissionResponse {
		resp, cerr := c.CheckPermission(ctx, &v1.CheckPermissionRequest{
			Resource:   &v1.ObjectReference{ObjectType: browserschema.WebActionType, ObjectId: objID},
			Permission: authz.SlotGrantRelationName(browserschema.ActPermission),
			Subject: &v1.SubjectReference{
				Object: &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + sessName},
			},
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
			Context:     fpCtx,
		})
		require.NoError(t, cerr, "raw CheckPermission must not error")
		return resp
	}
	fpStruct := func(fp string) *structpb.Struct {
		s, serr := structpb.NewStruct(map[string]any{"presented_fingerprint": fp})
		require.NoError(t, serr)
		return s
	}

	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		rawCheck(fpStruct("fp-A"), grantID.String()).GetPermissionship(),
		"raw: matching fingerprint resolves HAS_PERMISSION")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		rawCheck(fpStruct("fp-B"), grantID.String()).GetPermissionship(),
		"raw: mismatched fingerprint resolves NO_PERMISSION")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		rawCheck(fpStruct(""), grantID.String()).GetPermissionship(),
		"raw: empty fingerprint resolves NO_PERMISSION via the caveat guard")

	// THE critical case: with the presented_fingerprint arg ABSENT, SpiceDB cannot
	// finish evaluating pagestate_matches and returns CONDITIONAL — NOT
	// HAS_PERMISSION. checkExternalSlotGrant's `== HAS_PERMISSION` mapping (asserted
	// by the unit test TestCheck_External_WebAction_ConditionalMapsToDeny) turns this
	// into a deny; the security hole would be a refactor that treated CONDITIONAL as
	// allow.
	absent := rawCheck(nil, grantID.String())
	require.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION, absent.GetPermissionship(),
		"raw: an ABSENT presented_fingerprint partial-evaluates to CONDITIONAL — the case that must map to deny")
	assert.NotEqual(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, absent.GetPermissionship(),
		"CONDITIONAL is emphatically not HAS_PERMISSION")
}
