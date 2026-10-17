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

// webHostAndActionComposedFragment is web_host + web_action as ComposeSlots emits
// them for a class that declares BOTH slots: an uncaveated web_host#slot_grant_act
// (the coarse host capability) and a web_action#slot_grant_act caveated `with
// pagestate_matches and expiration` (the per-action revalidation). pagestate_matches
// and expiration are in the canonical scaffold Schema (E1); agentsession#interact
// (for the `->interact` arrow) is too.
const webHostAndActionComposedFragment = `
definition web_host {
    relation slot_grant_act: agentsession with expiration
    relation owner: user
    permission act = slot_grant_act->interact + owner
}

definition web_action {
    relation slot_grant_act: agentsession with pagestate_matches and expiration
    relation owner: user
    permission act = slot_grant_act->interact + owner
}
`

// TestIntegration_WebActionTwoLayerGate_RoundTrip proves Phase E's E8 two-layer
// acting gate end to end against a real SpiceDB: a browser MUTATE must hold BOTH
// the coarse web_host#act grant AND the per-action web_action#act grant whose
// pagestate_matches caveat still matches the live page, while a non-mutate call is
// authorized on the host grant alone. The checker drives the SAME path production
// does — the acting tool's web_host Permission() plus the web_action key +
// fingerprint + mutate flag carried on Inputs (what NamedArgs stamps and
// BuildInputs lifts).
//
//	A both grants, matching fingerprint, mutate flag  → ALLOWED
//	B host grant only (action has no grant), mutate   → DENIED (needs approval)
//	C web_action grant present, fingerprint diverged  → DENIED (TOCTOU)
//	D observe (no mutate flag), host grant only        → ALLOWED (web_action never checked)
func TestIntegration_WebActionTwoLayerGate_RoundTrip(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webHostAndActionComposedFragment)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const ns, sessName, host = "ns", "s1", "aa.com"
	const actionKeyRaw = "aa.com/click/place-order-abcd1234"
	const otherActionKeyRaw = "aa.com/click/delete-account-99999999"
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	// The grant ids as the bind path mints them: NewObjectID over each slot's
	// published transform chain (byte-for-byte identical — spicedb_escape).
	webHostID, err := authz.NewObjectID(host, browserschema.WebHostValueTransforms())
	require.NoError(t, err, "derive the web_host object id")
	webActionID, err := authz.NewObjectID(actionKeyRaw, browserschema.WebActionValueTransforms())
	require.NoError(t, err, "derive the web_action object id")

	// Write BOTH grants: the uncaveated host capability AND the caveated per-action
	// grant pinning the page fingerprint approved at bind time ({approved_fingerprint: "fp-A"}).
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{
			{
				ResourceType: browserschema.WebHostType,
				ResourceID:   webHostID,
				Permission:   browserschema.ActPermission,
			},
			{
				ResourceType:  browserschema.WebActionType,
				ResourceID:    webActionID,
				Permission:    browserschema.ActPermission,
				CaveatName:    "pagestate_matches",
				CaveatContext: map[string]any{"approved_fingerprint": "fp-A"},
			},
		}, time.Now().Add(time.Hour)), "GrantSlots (host + caveated action)")

	// The acting tool's REAL Permission() — External on web_host#act, instance = the
	// page host. The web_action layer rides on Inputs, exactly as dispatch wires it.
	checker := toolcheck.Checker{Cli: c}
	actPerm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:         browserschema.WebHostType,
			Permission:           browserschema.ActPermission,
			ResourceIDTemplate:   "{host}",
			ResourceIDTransforms: browserschema.WebHostValueTransforms(),
		},
	}
	inputsFor := func(required bool, actionKey, fp string) authz.Inputs {
		return authz.Inputs{
			Subject:                  "u-1",
			Args:                     map[string]any{"host": host},
			AgentSessionRef:          ns + "/" + sessName,
			SlotResourceTypes:        []string{browserschema.WebHostType, browserschema.WebActionType},
			WebActionRequired:        required,
			WebActionKey:             actionKey,
			PresentedPageFingerprint: fp,
		}
	}

	// CASE A — host grant + matching-fingerprint action grant + mutate flag → ALLOWED.
	assert.Equal(t, authz.OutcomeAllowed,
		checker.CheckToolCall(ctx, actPerm, inputsFor(true, actionKeyRaw, "fp-A")).Outcome,
		"a mutate holding BOTH grants, with the page fingerprint still matching, must be allowed")

	// CASE B — host grant held, but the action has NO web_action grant → DENIED,
	// routed to approval. The coarse host grant is not enough for a mutate.
	bRes := checker.CheckToolCall(ctx, actPerm, inputsFor(true, otherActionKeyRaw, "fp-A"))
	assert.Equal(t, authz.OutcomeDenied, bRes.Outcome,
		"a mutate with the host grant but no per-action grant must deny")
	assert.Contains(t, bRes.Message, "approval", "the web_action deny routes to human approval")
	// CASE F (E10) — this is a FIRST-TIME ask: no web_action grant has ever
	// existed for otherActionKeyRaw, so the denial must NOT be flagged as a
	// changed-page re-prompt.
	assert.False(t, bRes.ChangedPageReprompt,
		"no grant exists at all for this instance, so this must read as a first-time ask, not a re-prompt")

	// CASE C — action grant present, but the live page diverged (fingerprint
	// mismatch) → DENIED. This is the TOCTOU revalidation firing.
	cRes := checker.CheckToolCall(ctx, actPerm, inputsFor(true, actionKeyRaw, "fp-B"))
	assert.Equal(t, authz.OutcomeDenied, cRes.Outcome,
		"a changed page (mismatched fingerprint) must deny even though the grant exists")
	// CASE E (E10) — this IS a changed-page re-prompt: a human already approved
	// actionKeyRaw (the grant written above pins approved_fingerprint "fp-A"),
	// and the live page ("fp-B") no longer matches it. The denial must say so,
	// distinctly from CASE F's first-time ask above.
	assert.True(t, cRes.ChangedPageReprompt,
		"a grant EXISTS for this instance and only its caveat failed — this must be flagged as a changed-page re-prompt")

	// CASE D — a NON-mutate call (observe/navigate/session: no mutate flag) is
	// authorized on the host grant alone; the web_action leg is never consulted,
	// so a key/fingerprint it does not carry is irrelevant.
	assert.Equal(t, authz.OutcomeAllowed,
		checker.CheckToolCall(ctx, actPerm, inputsFor(false, "", "")).Outcome,
		"a non-mutate call needs only the host grant; the web_action check never fires")
}

// TestIntegration_WebActionReprompt_EscalationKnob proves, against a real
// SpiceDB, Phase E's E10 off-by-default model-escalation knob: with the
// RepromptEscalation knob at its zero value (off), a changed-page re-prompt
// ALWAYS denies — the escape-safe default, never dependent on a model, even
// when a judge is wired and would confirm the change benign. Only an
// EXPLICITLY enabled knob + a judge confirming benign ever suppresses it.
func TestIntegration_WebActionReprompt_EscalationKnob(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webHostAndActionComposedFragment)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const ns, sessName, host = "ns", "s2", "bb.com"
	const actionKeyRaw = "bb.com/click/place-order-abcd1234"
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	webHostID, err := authz.NewObjectID(host, browserschema.WebHostValueTransforms())
	require.NoError(t, err, "derive the web_host object id")
	webActionID, err := authz.NewObjectID(actionKeyRaw, browserschema.WebActionValueTransforms())
	require.NoError(t, err, "derive the web_action object id")

	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{
			{ResourceType: browserschema.WebHostType, ResourceID: webHostID, Permission: browserschema.ActPermission},
			{
				ResourceType:  browserschema.WebActionType,
				ResourceID:    webActionID,
				Permission:    browserschema.ActPermission,
				CaveatName:    "pagestate_matches",
				CaveatContext: map[string]any{"approved_fingerprint": "fp-A"},
			},
		}, time.Now().Add(time.Hour)), "GrantSlots (host + caveated action)")

	actPerm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:         browserschema.WebHostType,
			Permission:           browserschema.ActPermission,
			ResourceIDTemplate:   "{host}",
			ResourceIDTransforms: browserschema.WebHostValueTransforms(),
		},
	}
	in := authz.Inputs{
		Subject:                  "u-1",
		Args:                     map[string]any{"host": host},
		AgentSessionRef:          ns + "/" + sessName,
		SlotResourceTypes:        []string{browserschema.WebHostType, browserschema.WebActionType},
		WebActionRequired:        true,
		WebActionKey:             actionKeyRaw,
		PresentedPageFingerprint: "fp-B", // diverged from the approved fp-A
	}

	// DEFAULT — RepromptEscalation left at its zero value: always re-prompts,
	// even though a judge is wired and would say benign. The judge must never
	// be consulted at all.
	benignJudge := &alwaysBenignJudge{}
	defaultChecker := toolcheck.Checker{Cli: c}
	defRes := defaultChecker.CheckToolCall(ctx, actPerm, in)
	assert.Equal(t, authz.OutcomeDenied, defRes.Outcome, "the knob defaults to off, so a diverged page must still re-prompt")
	assert.True(t, defRes.ChangedPageReprompt)

	// ENABLED + a judge confirming benign — suppresses the re-prompt.
	enabledChecker := toolcheck.Checker{
		Cli:                c,
		RepromptEscalation: toolcheck.RepromptEscalationConfig{Enabled: true, Judge: benignJudge},
	}
	enRes := enabledChecker.CheckToolCall(ctx, actPerm, in)
	assert.Equal(t, authz.OutcomeAllowed, enRes.Outcome,
		"an explicitly enabled knob with a judge confirming benign must suppress the re-prompt")
	assert.Equal(t, 1, benignJudge.calls, "the judge is consulted only when the knob is explicitly enabled")
}

// alwaysBenignJudge is a toolcheck.RepromptJudge stub that always confirms
// benign, for the integration test above.
type alwaysBenignJudge struct{ calls int }

func (j *alwaysBenignJudge) ConfirmBenign(_ context.Context, _, _ string) (bool, error) {
	j.calls++
	return true, nil
}
