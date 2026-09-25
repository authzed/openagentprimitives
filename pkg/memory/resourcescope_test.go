package memory_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestResourceScope_RoundTrips(t *testing.T) {
	s, err := memory.ResourceScope("customer", "acme")
	require.NoError(t, err)
	assert.Equal(t, memory.ScopeKindResource, s.Kind)
	assert.Equal(t, "customer:acme", s.ID)

	objType, objID, ok := memory.ResourceRef(s)
	require.True(t, ok, "a scope this package built must parse back")
	assert.Equal(t, "customer", objType)
	assert.Equal(t, "acme", objID)
}

// The door that authorizes a memory read keys on scope.ID ALONE, not on Kind
// (pkg/memory/httpsrv mints ForBearerToken(perm, id, …) and
// CompositeSearcher checks EnsureApproval(ctx, ReadMemory, sc.ID)). So a
// resource ID that could collide with a session ID would let an approval
// minted for one authorize the other. Sessions are "<ns>/<name>"; resources
// are "<type>:<id>". Pin the disjointness rather than assuming it.
func TestResourceScope_IDCannotCollideWithASessionID(t *testing.T) {
	s, err := memory.ResourceScope("customer", "acme")
	require.NoError(t, err)
	assert.NotContains(t, s.ID, "/",
		"a resource scope ID must never contain '/', or it could collide with a session's <ns>/<name>")

	_, err = memory.ResourceScope("customer", "acme/prod")
	require.Error(t, err, "a '/' in the object id must be refused, not encoded away")
	assert.Contains(t, err.Error(), "/")
}

func TestResourceScope_RejectsUnusableRefs(t *testing.T) {
	cases := []struct {
		name           string
		objType, objID string
		want           string
	}{
		{"empty type: nothing to expand view_memory against", "", "acme", "type"},
		{"empty id: no instance to scope to", "customer", "", "id"},
		{"colon in type: ambiguous parse", "cust:omer", "acme", ":"},
		{"colon in id: ambiguous parse", "customer", "ac:me", ":"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := memory.ResourceScope(tc.objType, tc.objID)
			require.Error(t, err)
			assert.Contains(t, strings.ToLower(err.Error()), tc.want)
		})
	}
}

func TestIsResourceScope_DistinguishesFromSession(t *testing.T) {
	res, err := memory.ResourceScope("customer", "acme")
	require.NoError(t, err)
	assert.True(t, memory.IsResourceScope(res))
	assert.False(t, memory.IsResourceScope(memory.Scope{Kind: "session", ID: "default/sess-1"}))
}

// A scope carrying the resource Kind but a malformed ID must not parse — a
// caller that hand-built one gets ok=false rather than a half-parsed ref.
// ResourceRef must accept only what ResourceScope could have produced, so
// each case here is a shape ResourceScope itself refuses.
func TestResourceRef_RefusesAMalformedID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"no colon: not a resource ref at all", "no-colon"},
		{"second colon in the id half: ResourceScope would refuse the ':' in objID", "customer:acme:prod"},
		{"slash surviving into objType after the cut: ResourceScope would refuse the '/' in objType", "a/b:c"},
		{"slash surviving into objID after the cut: ResourceScope would refuse the '/' in objID", "customer:acme/prod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := memory.ResourceRef(memory.Scope{Kind: memory.ScopeKindResource, ID: tc.id})
			assert.False(t, ok)
		})
	}
}

// TestViewMemoryIsTheAudiencePermission pins the name a resource pool's
// audience is expanded under. It is the permission the per-datum mint hands the
// minter for a pool's entries, so a rename here silently re-points every pool
// tag at a permission no schema defines — which expands to nobody, and a tag
// readable by nobody looks exactly like a correctly-restricted one.
func TestViewMemoryIsTheAudiencePermission(t *testing.T) {
	assert.Equal(t, "view_memory", memory.PermissionViewMemory,
		"the wire name a SpiceDB fragment declares, not a Go-side alias")
}

// TestViewMemoryCannotBeSlotted ties the constant to the property the slot
// composer guards on, without either package importing the other's literal.
//
// ComposeSlots OWNS the permission line it writes, so a slot naming a pool's
// AUDIENCE permission would replace "who may read this pool" with the slot's
// own interact+owner expression. The guard keys on the SHAPE of the name (a
// `_memory` suffix without a `write_` prefix), so renaming this constant to
// something outside that shape would quietly take a pool's audience out from
// behind the guard — with no error anywhere, which is why this is asserted
// across the two packages rather than in either one alone.
func TestViewMemoryCannotBeSlotted(t *testing.T) {
	_, _, _, err := schema.ComposeSlots(
		"definition customer {}\n",
		[]schema.SlotPair{{ResourceType: "customer", Permission: memory.PermissionViewMemory}},
	)
	require.Error(t, err, "a slot naming the pool audience permission must be refused")
	assert.Contains(t, err.Error(), memory.PermissionViewMemory)
}
