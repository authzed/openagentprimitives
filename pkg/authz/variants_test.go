package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveVariant_FirstMatchWins(t *testing.T) {
	variants := []PermissionVariant{
		{When: `args.objectType == "contacts"`, Check: Permission{StateImpact: Readonly, Check: &PermissionCheck{
			ResourceType: "crm_company", ResourceIDExpr: `"X"`, Permission: "contact_access",
		}}},
		{When: `args.objectType == "companies"`, Check: Permission{StateImpact: Passthrough}},
	}
	got, matched, err := ResolveVariant(variants, map[string]any{"objectType": "contacts"})
	require.NoError(t, err)
	require.True(t, matched, "expected match")
	assert.Equal(t, Readonly, got.StateImpact)
}

func TestResolveVariant_NoMatchFallsThrough(t *testing.T) {
	variants := []PermissionVariant{
		{When: `args.objectType == "contacts"`, Check: Permission{StateImpact: Readonly, Check: &PermissionCheck{
			ResourceType: "x", ResourceIDExpr: `"x"`, Permission: "read",
		}}},
	}
	_, matched, err := ResolveVariant(variants, map[string]any{"objectType": "deals"})
	require.NoError(t, err)
	assert.False(t, matched, "expected no match")
}

func TestResolveVariant_BadCELIsRuntimeError(t *testing.T) {
	variants := []PermissionVariant{{When: "this is not valid CEL %%%", Check: Permission{StateImpact: Passthrough}}}
	_, _, err := ResolveVariant(variants, map[string]any{})
	require.Error(t, err, "want error from invalid CEL")
}
