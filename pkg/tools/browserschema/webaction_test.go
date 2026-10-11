package browserschema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// web_action is the REVALIDATION sibling of web_host: a separate session-only
// acting slot whose slot_grant_act the composer caveats with pagestate_matches
// (Phase E's TOCTOU gate). It is shaped IDENTICALLY to web_host — an inert owner
// leg plus an `act` permission the slot composer owns — so the only thing the
// per-type caveat keys on is the definition NAME, pinned here at the one source.
func TestWebActionResource_IsSessionOnlyActingSlotShape(t *testing.T) {
	r := browserschema.WebActionResource()

	assert.Equal(t, "web_action", r.Name)
	assert.Equal(t, browserschema.WebActionType, r.Name)

	// Session-only: the approval IS the authority, and ValidateStanding then
	// forbids an approverPermission.
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, r.Standing,
		"web_action is session-only — the per-action approval is the authority")
	assert.Empty(t, r.ApproverPermission,
		"session-only forbids an approverPermission")
	require.NoError(t, r.ValidateStanding(),
		"the declared standing must be self-consistent, or every resolveStandingFor errors")

	// The inert owner leg, exactly like web_host, so the composed permission is
	// `slot_grant_act->interact + owner`.
	require.Len(t, r.Relations, 1)
	assert.Equal(t, "owner", r.Relations[0].Name)
	assert.Equal(t, "user", r.Relations[0].SubjectType)

	// Exactly one permission, named act (reusing ActPermission), whose BASE
	// expr the composer replaces.
	require.Len(t, r.Permissions, 1)
	assert.Equal(t, browserschema.ActPermission, r.Permissions[0].Name)
	assert.Equal(t, "act", r.Permissions[0].Name)
	assert.NotEmpty(t, r.Permissions[0].Title, "the card needs a human phrase, not the wire handle")
}

// A display is required so a revalidation approval card shows the action behind
// a web mark rather than the bare wire type name — same discipline as web_host.
func TestWebActionResource_DeclaresADisplay(t *testing.T) {
	d := browserschema.WebActionResource().Display
	require.NotNil(t, d, "web_action must declare a display")
	assert.NotEmpty(t, d.Icon, "the card needs an icon")
	assert.NotEmpty(t, d.Label, "the card needs a label deriver")
}

// WebActionFragment carries exactly the one web_action resource — the single
// declaration source the guardian and agentclass controllers both read, so the
// cluster schema never carries two web_action blocks.
func TestWebActionFragment_CarriesWebActionOnce(t *testing.T) {
	f := browserschema.WebActionFragment()
	require.NotNil(t, f)
	require.Len(t, f.Resources, 1)
	assert.Equal(t, "web_action", f.Resources[0].Name)
	assert.Empty(t, f.RawZed, "web_action is structured, not RawZed")
}

// The id transform chain matches web_host's: a canonical host escaped into a
// legal SpiceDB object id. A fresh slice per call so no caller can mutate a
// shared one.
func TestWebActionValueTransforms_IsSpicedbEscape(t *testing.T) {
	got := browserschema.WebActionValueTransforms()
	assert.Equal(t, []string{"spicedb_escape"}, got)

	got[0] = "mutated"
	assert.Equal(t, []string{"spicedb_escape"}, browserschema.WebActionValueTransforms(),
		"each call must return a fresh slice — a caller mutating one must not poison the next")
}

// web_action reuses the SAME `act` permission as web_host, so the slot-grant
// relation name both derive is identical: slot_grant_act. The caveat that
// distinguishes them is injected per resource TYPE by the composer, not encoded
// in the relation name — pinned here so a rename of either side is caught.
func TestWebActionGrantRelationName_IsSlotGrantAct(t *testing.T) {
	assert.Equal(t, "slot_grant_act", authz.SlotGrantRelationName(browserschema.ActPermission))
}
