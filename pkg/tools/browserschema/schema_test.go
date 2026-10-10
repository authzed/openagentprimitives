package browserschema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// The web_host resource is the browser's session-only acting slot, shaped like
// git_repo's push: an inert owner leg plus an `act` permission the slot composer
// owns. These are the properties downstream (C3 Permission, C8 unattended grant)
// and the plan gate depend on, so they are pinned here at the single source.
func TestResource_IsSessionOnlyActingSlotShape(t *testing.T) {
	r := browserschema.Resource()

	assert.Equal(t, "web_host", r.Name)
	assert.Equal(t, browserschema.WebHostType, r.Name)

	// Session-only: the approval IS the authority, and ValidateStanding then
	// forbids an approverPermission.
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, r.Standing,
		"web_host is session-only — no local permission governs who may approve reaching a host")
	assert.Empty(t, r.ApproverPermission,
		"session-only forbids an approverPermission")
	require.NoError(t, r.ValidateStanding(),
		"the declared standing must be self-consistent, or every resolveStandingFor errors")

	// The inert owner leg, exactly like git_repo, so the composed permission is
	// `slot_grant_act->interact + owner`.
	require.Len(t, r.Relations, 1)
	assert.Equal(t, "owner", r.Relations[0].Name)
	assert.Equal(t, "user", r.Relations[0].SubjectType)

	// Exactly one permission, named act, whose BASE expr the composer replaces.
	require.Len(t, r.Permissions, 1)
	assert.Equal(t, browserschema.ActPermission, r.Permissions[0].Name)
	assert.Equal(t, "act", r.Permissions[0].Name)
	assert.NotEmpty(t, r.Permissions[0].Title, "the card needs a human phrase, not the wire handle")
}

// A display is required (the brief: web_host "has a display"): an icon plus a
// label deriver, so an approval card shows the host behind a web mark rather
// than the bare wire type name.
func TestResource_DeclaresADisplay(t *testing.T) {
	d := browserschema.Resource().Display
	require.NotNil(t, d, "web_host must declare a display")
	assert.NotEmpty(t, d.Icon, "the card needs an icon")
	assert.NotEmpty(t, d.Label, "the card needs a label deriver to show the host")
}

// Fragment carries exactly the one web_host resource — the single declaration
// source the guardian and agentclass controllers both read, so the cluster
// schema never carries two web_host blocks.
func TestFragment_CarriesWebHostOnce(t *testing.T) {
	f := browserschema.Fragment()
	require.NotNil(t, f)
	require.Len(t, f.Resources, 1)
	assert.Equal(t, "web_host", f.Resources[0].Name)
	assert.Empty(t, f.RawZed, "web_host is structured, not RawZed")
}

// TestWebHostValueTransforms_CasefoldsBeforeEscaping pins the case-canonicalization
// fix: a DNS host is case-insensitive, but the two legs that derive a web_host id
// disagree on case — the acting tool's CHECK keys off the already-lower-cased
// CurrentHost(), while a plan-gate GRANT write keys off the free-form host a
// planner named. Folding to lowercase BEFORE the escape makes both land on one
// id, so an "AA.com" slot value grants where the "aa.com" check looks, instead of
// silently re-prompting every acting call.
func TestWebHostValueTransforms_CasefoldsBeforeEscaping(t *testing.T) {
	upper, err := authz.NewObjectID("AA.com", browserschema.WebHostValueTransforms())
	require.NoError(t, err)
	lower, err := authz.NewObjectID("aa.com", browserschema.WebHostValueTransforms())
	require.NoError(t, err)

	assert.Equal(t, lower.String(), upper.String(),
		"AA.com and aa.com name the same host, so they must derive the same web_host id")
	assert.Equal(t, "aa=2Ecom", upper.String(),
		"the host is lower-cased (casefold_identity) first, then its dots escaped (spicedb_escape)")
}
