package authz_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// webActionRefusingWriter mirrors SpiceDB's refusal of an UNCAVEATED write to a
// `with pagestate_matches and expiration` relation: web_action's slot_grant_act
// is declared caveated, so SpiceDB rejects a write that carries no caveat, and —
// because GrantSlots writes one ATOMIC batch — that rejection fails the whole
// batch, web_host grant included. The writer reproduces that: any web_action
// relation in a batch fails the entire call.
type webActionRefusingWriter struct {
	wrote []authz.Relation
}

func (w *webActionRefusingWriter) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	for _, r := range rels {
		if r.ResourceType == "web_action" {
			return fmt.Errorf("spicedb: caveat pagestate_matches required on a web_action slot_grant write")
		}
	}
	w.wrote = append(w.wrote, rels...)
	return nil
}

func (w *webActionRefusingWriter) DeleteRelationships(context.Context, []authz.Relation) error {
	return nil
}

// TestBindApproved_WebActionBindingDoesNotWedgeTheBatch is the MAJOR fix for the
// model-reachable DoS of the mainline approval flow: a plan phase that named a
// web_action slot emits an uncaveated web_action binding, planGateBindings pairs
// it into the approved batch with no caveat, and the single atomic GrantSlots
// write SpiceDB refuses — taking the coarse web_host grant down with it, so the
// human approves the phase and NOTHING binds. BindApproved must filter the
// web_action binding (mirroring CopySlotGrants) so the rest of the batch still
// binds; a per-action web_action grant is only ever minted via the fingerprinted
// E9/persistWebActionApproval path, which writes GrantSlots directly WITH the
// caveat.
func TestBindApproved_WebActionBindingDoesNotWedgeTheBatch(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/web-action-wedge"}
	w := &webActionRefusingWriter{}

	err := authz.BindApproved(systemCtx(), mem, memScope, bindSession(), w,
		[]authz.SlotBinding{
			{ResourceType: "web_host", ResourceID: authz.TrustedObjectID("aa=2Ecom"), Permission: "act"},
			{ResourceType: "web_action", ResourceID: authz.TrustedObjectID("aa=2Ecom=2Fclick=2Fplace-order"), Permission: "act"},
			{ResourceType: "crm_company", ResourceID: authz.TrustedObjectID("acme"), Permission: "contact_access"},
		},
		authz.EnforcePreconditions, time.Now().Add(time.Hour), logr.Discard(), time.Now)

	require.NoError(t, err, "the web_action binding must be filtered so SpiceDB's uncaveated-write refusal cannot wedge the whole batch")
	require.Len(t, w.wrote, 2, "web_host + crm_company carry; only the uncaveated web_action is dropped")
	types := map[string]bool{}
	for _, r := range w.wrote {
		types[r.ResourceType] = true
	}
	assert.True(t, types["web_host"], "the coarse web_host grant the human approved must still bind")
	assert.True(t, types["crm_company"], "an unrelated grant in the batch must not be collateral damage")
	assert.False(t, types["web_action"], "the per-action web_action grant is mintable only via the fingerprinted approval path")
}

// TestBindApproved_OnlyWebActionBindingBindsNothing covers the degenerate batch
// whose sole binding is a web_action: after filtering there is nothing to grant,
// and BindApproved returns cleanly rather than erroring or writing an empty grant.
func TestBindApproved_OnlyWebActionBindingBindsNothing(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/web-action-only"}
	w := &webActionRefusingWriter{}

	err := authz.BindApproved(systemCtx(), mem, memScope, bindSession(), w,
		[]authz.SlotBinding{
			{ResourceType: "web_action", ResourceID: authz.TrustedObjectID("aa=2Ecom=2Fclick=2Fx"), Permission: "act"},
		},
		authz.EnforcePreconditions, time.Now().Add(time.Hour), logr.Discard(), time.Now)

	require.NoError(t, err, "a web_action-only batch filters to empty and is a clean no-op, not an error")
	assert.Empty(t, w.wrote, "nothing is granted when the only binding was the filtered web_action")
}
