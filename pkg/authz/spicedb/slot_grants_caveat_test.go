package spicedb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// relationshipFor is the single point where a slot grant's optional
// pagestate_matches caveat is attached. These tests pin its two directions:
// an ordinary (uncaveated) grant renders byte-identically to the pre-caveat
// code — no OptionalCaveat — while a web_action grant carries the caveat name
// and the frozen approved_fingerprint context.

// No-regression proof: an uncaveated slot grant — every grant but web_action's,
// built exactly as GrantSlots builds it (authz.SlotGrantRelation, empty
// CaveatName) — renders with NO OptionalCaveat and otherwise unchanged. If the
// E2 write path ever attached a caveat to an uncaveated grant, SpiceDB would
// refuse every ordinary slot (its relation is not declared `with` any caveat),
// so this guards the whole existing grant surface.
func TestRelationshipFor_UncaveatedGrantRendersWithoutOptionalCaveat(t *testing.T) {
	sess := authz.SessionRef{Namespace: "ns", Name: "s1"}
	expiry := time.Now().Add(time.Hour)

	rel := authz.SlotGrantRelation("git_repo", "acme/widgets", "push", sess)
	rel.ExpiresAt = expiry
	// GrantSlots copies these through verbatim; an ordinary binding leaves both zero.
	require.Empty(t, rel.CaveatName, "an ordinary slot grant carries no caveat name")
	require.Nil(t, rel.CaveatContext, "an ordinary slot grant carries no caveat context")

	got, err := relationshipFor(rel)
	require.NoError(t, err, "an uncaveated grant never fails to render")
	assert.Nil(t, got.GetOptionalCaveat(),
		"an uncaveated slot grant must render with NO OptionalCaveat — byte-identical to the pre-caveat path")

	// The rest of the tuple is unchanged by the caveat feature.
	assert.Equal(t, "git_repo", got.GetResource().GetObjectType())
	assert.Equal(t, "acme/widgets", got.GetResource().GetObjectId())
	assert.Equal(t, authz.SlotGrantRelationName("push"), got.GetRelation())
	assert.Equal(t, "agentsession", got.GetSubject().GetObject().GetObjectType())
	assert.Equal(t, "ns/s1", got.GetSubject().GetObject().GetObjectId())
	require.NotNil(t, got.GetOptionalExpiresAt(), "the expiry still renders")
	assert.WithinDuration(t, expiry, got.GetOptionalExpiresAt().AsTime(), time.Second)
}

// The caveated counterpart: a web_action grant renders the pagestate_matches
// caveat with the frozen approved_fingerprint as its stored context. This is the
// shape the E2 write path produces and the integration test writes to real SpiceDB.
func TestRelationshipFor_WebActionGrantRendersPagestateCaveat(t *testing.T) {
	sess := authz.SessionRef{Namespace: "ns", Name: "s1"}
	rel := authz.SlotGrantRelation(browserschema.WebActionType, "aa=2Ecom", browserschema.ActPermission, sess)
	rel.ExpiresAt = time.Now().Add(time.Hour)
	rel.CaveatName = "pagestate_matches"
	rel.CaveatContext = map[string]any{"approved_fingerprint": "fp-A"}

	got, err := relationshipFor(rel)
	require.NoError(t, err, "a well-formed caveat context renders cleanly")
	cav := got.GetOptionalCaveat()
	require.NotNil(t, cav, "a web_action grant must render WITH an OptionalCaveat")
	assert.Equal(t, "pagestate_matches", cav.GetCaveatName())
	assert.Equal(t, "fp-A", cav.GetContext().GetFields()["approved_fingerprint"].GetStringValue(),
		"the approved fingerprint frozen at bind time must be stored in the caveat context")
}
