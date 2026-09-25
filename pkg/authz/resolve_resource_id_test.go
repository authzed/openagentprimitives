package authz_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// ResolveResourceID has two branches — a {arg} template and a CEL expression —
// and BOTH must apply check.resourceIDTransforms. The expression branch used to
// return the evaluated string directly, silently dropping the transforms.
//
// The consequence is not cosmetic. A check declaring sha256 does so to keep the
// raw value out of SpiceDB; dropping the transform writes the value itself into
// the object id. Nothing failed — the id was simply wrong, and wrong in the
// direction that leaks.
func TestResolveResourceID_appliesTransformsOnBothBranches(t *testing.T) {
	const raw = "Hello World"

	cases := []struct {
		name  string
		check authz.PermissionCheck
		want  string
	}{
		{
			name: "template + transforms: applied",
			check: authz.PermissionCheck{
				ResourceIDTemplate:   "{v}",
				ResourceIDTransforms: []string{"lowercase", "remove_spaces"},
			},
			want: "helloworld",
		},
		{
			name: "expr + transforms: applied",
			check: authz.PermissionCheck{
				ResourceIDExpr:       "args.v",
				ResourceIDTransforms: []string{"lowercase", "remove_spaces"},
			},
			want: "helloworld",
		},
		{
			name: "expr without transforms: verbatim",
			check: authz.PermissionCheck{
				ResourceIDExpr: "args.v",
			},
			want: raw,
		},
		{
			name: "template without transforms: verbatim",
			check: authz.PermissionCheck{
				ResourceIDTemplate: "{v}",
			},
			want: raw,
		},
		{
			// Order matters and must match the template branch: the transform
			// chain runs left to right over the fully-resolved string.
			name: "expr + ordered chain: applied left to right",
			check: authz.PermissionCheck{
				ResourceIDExpr:       "args.v",
				ResourceIDTransforms: []string{"remove_spaces", "lowercase"},
			},
			want: "helloworld",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := authz.ResolveResourceID(tc.check, map[string]any{"v": raw})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The privacy property stated as its own test, because it is the reason the
// transform chain exists on a value check: SpiceDB must never receive the raw
// value. Asserting "the output is not the input" is weaker than asserting the
// exact hash but survives a change of hash encoding, and it names the property
// that actually matters if it breaks.
func TestResolveResourceID_sha256KeepsTheRawValueOutOfTheObjectID(t *testing.T) {
	const secret = "https://user:pw@internal.example.com/path?token=abc"

	for _, tc := range []struct {
		name  string
		check authz.PermissionCheck
	}{
		{"expr branch", authz.PermissionCheck{ResourceIDExpr: "args.url", ResourceIDTransforms: []string{"sha256"}}},
		{"template branch", authz.PermissionCheck{ResourceIDTemplate: "{url}", ResourceIDTransforms: []string{"sha256"}}},
	} {
		t.Run(tc.name+": raw value absent from the resolved id", func(t *testing.T) {
			got, err := authz.ResolveResourceID(tc.check, map[string]any{"url": secret})
			require.NoError(t, err)
			assert.NotContains(t, got, "internal.example.com", "host must not survive into the object id")
			assert.NotContains(t, got, "token=abc", "query must not survive into the object id")
			assert.NotEqual(t, secret, got)
			assert.Len(t, got, 64, "sha256 hex digest")
		})
	}
}

// An unknown transform must be an error on the expression branch too, not a
// silent pass-through — otherwise a typo in a security-relevant chain degrades
// to "no transform applied", which is exactly the failure this all guards.
func TestResolveResourceID_unknownTransformErrorsOnBothBranches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check authz.PermissionCheck
	}{
		{"expr branch", authz.PermissionCheck{ResourceIDExpr: "args.v", ResourceIDTransforms: []string{"no_such_transform"}}},
		{"template branch", authz.PermissionCheck{ResourceIDTemplate: "{v}", ResourceIDTransforms: []string{"no_such_transform"}}},
	} {
		t.Run(tc.name+": errors", func(t *testing.T) {
			_, err := authz.ResolveResourceID(tc.check, map[string]any{"v": "x"})
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), "no_such_transform"),
				"the error must name the offending transform, got %q", err)
		})
	}
}
