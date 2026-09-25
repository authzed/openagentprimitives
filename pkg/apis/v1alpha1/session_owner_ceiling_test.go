package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ownerCeiling.fixed is documented as pinning the owner "regardless of Channel
// policy", but it was enforced ONLY by refusing a conflicting Channel config —
// and that refusal is guarded on the policy being non-nil with a non-empty
// explicit owner. A Channel that declares no owner block at all, which is the
// default and is permitted for any kind that supplies a starting user,
// satisfies neither guard.
//
// So an admin who pins a sensitive class to a group, precisely so no
// individual can self-approve their own tool calls, silently got the starter
// as owner — with approve, hold, manage_scope and fork following from it — and
// the Channel stayed Valid=True throughout.
func TestResolveOwnerSubject_FixedCeilingBeatsAnUndeclaredChannelPolicy(t *testing.T) {
	subject, source, err := spiceboxv1alpha1.ResolveOwnerSubject(
		"",  // ordinary identity mode
		nil, // the default: the Channel declares no owner policy
		"user:starter@corp",
		"",
		&spiceboxv1alpha1.OwnerCeiling{Fixed: "group:security#member"},
	)
	require.NoError(t, err)
	assert.Equal(t, "group:security#member", subject,
		"a fixed ceiling must pin the owner even when no Channel policy exists to refuse")
	assert.Equal(t, spiceboxv1alpha1.OwnerSourceCeiling, source,
		"the source must say the ceiling decided, so a surface describing ownership to humans does not claim the starter owns it")
}

// The pin also outranks an explicit Channel policy. The Channel controller
// refuses that combination at config time, but a resolver that would take the
// Channel's value if it ever arrived is one config-validation gap away from
// the same defect.
func TestResolveOwnerSubject_FixedCeilingBeatsAnExplicitChannelPolicy(t *testing.T) {
	subject, source, err := spiceboxv1alpha1.ResolveOwnerSubject(
		"",
		&spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "user:someone@corp"},
		"user:starter@corp",
		"",
		&spiceboxv1alpha1.OwnerCeiling{Fixed: "group:security#member"},
	)
	require.NoError(t, err)
	assert.Equal(t, "group:security#member", subject)
	assert.Equal(t, spiceboxv1alpha1.OwnerSourceCeiling, source)
}

// starterOnly means what it says: with no starting user there is no owner this
// ceiling admits, so resolution must fail rather than fall through to a
// whole-channel population.
func TestResolveOwnerSubject_StarterOnlyCeilingRefusesACollectiveFallback(t *testing.T) {
	_, _, err := spiceboxv1alpha1.ResolveOwnerSubject(
		"",
		nil,
		"", // no starting user
		"group:everyone-in-channel#member",
		&spiceboxv1alpha1.OwnerCeiling{StarterOnly: true},
	)
	require.Error(t, err,
		"starterOnly with no starter must fail closed, not acquire the channel's membership as owner")
}

// A nil ceiling is the common case and must change nothing: the existing
// precedence still decides.
func TestResolveOwnerSubject_NilCeilingPreservesExistingPrecedence(t *testing.T) {
	subject, source, err := spiceboxv1alpha1.ResolveOwnerSubject(
		"", nil, "user:starter@corp", "", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "user:starter@corp", subject)
	assert.Equal(t, spiceboxv1alpha1.OwnerSourceStarter, source)
}

// Passthrough still outranks the ceiling: in that mode the session ACTS as the
// starting user, so an owner who is someone else would describe a session that
// does not exist.
func TestResolveOwnerSubject_PassthroughStillOutranksAFixedCeiling(t *testing.T) {
	subject, source, err := spiceboxv1alpha1.ResolveOwnerSubject(
		spiceboxv1alpha1.IdentityModeUserPassthrough,
		nil,
		"user:starter@corp",
		"",
		&spiceboxv1alpha1.OwnerCeiling{Fixed: "group:security#member"},
	)
	require.NoError(t, err)
	assert.Equal(t, "user:starter@corp", subject)
	assert.Equal(t, spiceboxv1alpha1.OwnerSourcePassthrough, source)
}
