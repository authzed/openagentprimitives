package runner

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// desc builds one surface entry: a permission on a resource type, at a tier.
func desc(t *testing.T, perm, resourceType string, impact authz.StateImpact) permsurface.Descriptor {
	t.Helper()
	h, err := permsurface.NewPermHandle(perm, resourceType)
	require.NoError(t, err)
	return permsurface.Descriptor{
		Handle: h, Permission: perm, ResourceType: resourceType, StateImpact: impact,
	}
}

// The worked example is built from the SESSION'S OWN surface.
//
// It used to be hardcoded git, which shipped to every agent — including one
// whose declarable handles are entirely CRM, immediately after the prompt says
// these are the ONLY handles it may declare and anything else is silently
// dropped. An example an agent cannot follow is worse than none: measured,
// agents transcribe the example almost verbatim, so a git-shaped one aimed at a
// CRM agent is a template for handles that do not exist.
func TestPlanExample_UsesTheSessionsOwnHandles(t *testing.T) {
	crm := []permsurface.Descriptor{
		desc(t, "list", "crm_company", authz.Readonly),
		desc(t, "read", "crm_company", authz.Readonly),
		desc(t, "update", "crm_company", authz.Readwrite),
	}

	got := planExample(crm)

	assert.Contains(t, got, "perm:update:crm_company", "the example must speak the agent's own vocabulary")
	assert.NotContains(t, got, "git_repo", "and must not name a type this session cannot declare")
	assert.Contains(t, got, "crm_company", "the slot names the session's own resource type")
}

// The acting phase carries the READ it depends on.
//
// This is the whole reason the example is load-bearing: an agent copied a ship
// phase that declared write and push with no read, and stopped mid-task to ask
// a human for the read. Whatever the example shows is what gets copied, so it
// must show the correct shape in every surface, not just git's.
func TestPlanExample_ActingPhaseCarriesItsRead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		surface  []permsurface.Descriptor
		wantRead string
	}{
		{
			name: "crm: update carries read on the same type",
			surface: []permsurface.Descriptor{
				desc(t, "read", "crm_company", authz.Readonly),
				desc(t, "update", "crm_company", authz.Readwrite),
			},
			wantRead: "perm:read:crm_company",
		},
		{
			name: "git: push carries read on the same type",
			surface: []permsurface.Descriptor{
				desc(t, "read", "git_repo", authz.Readonly),
				desc(t, "write", "git_repo", authz.Readwrite),
				desc(t, "push", "git_repo", authz.External),
			},
			wantRead: "perm:read:git_repo",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := planExample(tc.surface)
			acting := got[strings.LastIndex(got, `{"id":`):]
			require.NotEmpty(t, acting)
			assert.Contains(t, acting, tc.wantRead,
				"the phase that writes must show the read it depends on")
		})
	}
}

// A read-only session has nothing to act with, so a two-phase example would be
// a shape it can never produce.
func TestPlanExample_ReadOnlySurfaceShowsOnePhase(t *testing.T) {
	got := planExample([]permsurface.Descriptor{
		desc(t, "list", "crm_company", authz.Readonly),
		desc(t, "read", "crm_company", authz.Readonly),
	})
	assert.Equal(t, 1, strings.Count(got, `"permissions"`), "one phase, because nothing here changes anything")
	assert.Contains(t, got, "perm:read:crm_company")
}

// No surface, no example. An empty one would be noise, and the caller already
// says "this session has no gated tools".
func TestPlanExample_EmptySurfaceRendersNothing(t *testing.T) {
	assert.Empty(t, planExample(nil))
}
