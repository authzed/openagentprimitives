//go:build !integration && !e2e

package relsource_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// Build-constrained because these tests call relsource.Register with fixture
// sources claiming definitions schema.zed has never declared. The registry is
// deliberately never reset (registry.go), so an unconstrained fixture would be
// linked into — and left sitting in — the integration and e2e binaries too.

func TestIsLabelRelation_MatchesOnlyTheLabelRelation(t *testing.T) {
	assert.True(t, relsource.IsLabelRelation(relsource.LabelRelation))
	assert.False(t, relsource.IsLabelRelation(relsource.SentinelRelation),
		"the sentinel and the label are different relations carrying different string subjects")
	assert.False(t, relsource.IsLabelRelation("member"))
	assert.False(t, relsource.IsLabelRelation(""))
}

// A label is display data and must stay out of the identity probes, for the
// same structural reason the sentinel does: its subject is an encoded NAME on
// the permission-less `string` type, so a user-subject filter matches nothing.
//
// The failure this prevents is invisible from results — an unmatched probe
// returns nothing, exactly like a person with no links — which is why it is
// caught at registration and asserted here rather than left to a reader to
// notice a console row that never appears.
func TestRegister_RefusesTheLabelRelationAsASubjectIdentityClaim(t *testing.T) {
	src := relsource.Source{
		Name:                  "labelprobefixture",
		Claims:                []string{"widget_directory#label"},
		SubjectIdentityClaims: []string{"widget_directory#label"},
	}

	require.PanicsWithValue(t,
		"relsource: source \"labelprobefixture\" declares the display-name label \"widget_directory#label\" as a SubjectIdentityClaim; its subject is an encoded name on the permission-less `string` type, so probing it can only ever return nothing",
		func() { relsource.Register(src) },
		"a provably empty probe must be refused where it is written, not discovered as a missing console row")
}

// The claim itself is ordinary and must stay writable by its owner: refusing
// it as a SUBJECT-IDENTITY claim says nothing about whether the source may
// write the relation, and conflating the two would stop the sync dead.
func TestRegister_AcceptsTheLabelAsAnOrdinaryClaim(t *testing.T) {
	src := relsource.Source{
		Name:                  "labelwriterfixture",
		Claims:                []string{"gizmo_directory#member", "gizmo_directory#label"},
		SubjectIdentityClaims: []string{"gizmo_directory#member"},
	}

	require.NotPanics(t, func() { relsource.Register(src) })
	assert.True(t, relsource.Owns(src, "gizmo_directory", relsource.LabelRelation),
		"the source that writes a label owns it, which is what stops anything else naming its rows")
}

// The subject type is declared once, next to the relation, precisely so the
// kind that writes the tuple, the bridge declaration that reads it and the
// schema text cannot drift apart on the spelling.
func TestLabelSubjectType_IsThePermissionLessStringType(t *testing.T) {
	assert.Equal(t, "string", relsource.LabelSubjectType)
}
